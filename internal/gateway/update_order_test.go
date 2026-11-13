package gateway

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/koltyakov/control/internal/buildinfo"
	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/update"
)

func TestIndependentNodeCommitCompatibilityAndBusyPeer(t *testing.T) {
	for _, test := range []struct {
		name                      string
		declared, current, commit bool
	}{
		{"compatible-before-gateway", true, false, true},
		{"legacy-before-gateway", false, false, false},
		{"legacy-after-gateway", false, true, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			g, d, info := deferralFixture(t)
			delete(g.nodes, "offline")
			delete(g.updateStatus, "offline")
			d.Participants = nil
			g.options.Software.GatewayProtocol = 1
			s := g.updateStatus["online"]
			s.Software.Version, s.Software.SHA256 = "older", "older"
			s.Paused, s.State, s.LeaseUntil = true, "ready", time.Now().Add(time.Minute)
			if test.declared {
				info.NodeGatewayMax = 1
				s.StagedSoftware = info
			}
			g.updateStatus["online"] = s
			peers := g.rolloutPeers(d)
			peers = append(peers, rolloutPeer{node: model.Node{ID: "busy", OS: "linux", Software: s.Software}, status: update.Status{ID: d.ID, Software: s.Software, State: "staged", Busy: true}, fresh: true})
			commits := 0
			_, err := g.advanceNodes(d, peers, test.current, func(id, kind string, command update.Command) {
				if kind == "update.commit" {
					if id != "online" || command.ID != d.ID {
						t.Fatal("committed busy or wrong node")
					}
					if len(g.updates.Current().InFlight) != 1 {
						t.Fatal("commit escaped durable intent")
					}
					commits++
				}
			})
			if err != nil || (commits == 1) != test.commit {
				t.Fatal("wrong independent commit", commits, err)
			}
		})
	}
}

func TestNodeCommitIntentSurvivesRestartAndFailedPersistence(t *testing.T) {
	g, d, info := deferralFixture(t)
	if err := g.updates.NodeInstall(d.ID, "online", true); err != nil {
		t.Fatal(err)
	}
	repo, err := update.OpenRepository(filepath.Join(filepath.Dir(g.path), "updates"))
	if err != nil || !slices.Equal(repo.Current().InFlight, []string{"online"}) {
		t.Fatal("lost commit intent", err)
	}
	copy := repo.Current()
	copy.InFlight[0] = "changed"
	if repo.Current().InFlight[0] != "online" {
		t.Fatal("snapshot aliases commit intent")
	}
	peers := g.rolloutPeers(d)
	for i := range peers {
		peers[i].fresh = false
	}
	_, err = g.advanceNodes(g.updates.Current(), peers, true, func(string, string, update.Command) {})
	if err != nil || len(g.updates.Current().InFlight) != 1 {
		t.Fatal("stale report cleared commit intent", err)
	}
	peers = []rolloutPeer{{node: g.nodes["online"], status: update.Status{ID: d.ID, Software: info, State: "applied", Paused: true}, fresh: true}}
	_, err = g.advanceNodes(g.updates.Current(), peers, true, func(string, string, update.Command) {})
	if err != nil || len(g.updates.Current().InFlight) != 1 {
		t.Fatal("paused report cleared commit intent", err)
	}
	peers[0].status.Paused = false
	_, err = g.advanceNodes(g.updates.Current(), peers, true, func(string, string, update.Command) {})
	if err != nil || len(g.updates.Current().InFlight) != 0 {
		t.Fatal("fresh resumed report did not confirm commit", err)
	}
	path := filepath.Join(filepath.Dir(g.path), "updates", "deployment.json")
	if err := os.Rename(path, path+".saved"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	s := update.Status{ID: d.ID, Software: info, State: "ready", Paused: true, LeaseUntil: time.Now().Add(time.Minute)}
	s.Software.Version, s.Software.SHA256 = "older", "older"
	peers[0].status = s
	_, err = g.advanceNodes(g.updates.Current(), peers, true, func(_ string, kind string, _ update.Command) {
		if kind == "update.commit" {
			t.Error("failed persistence dispatched a commit")
		}
	})
	if err == nil || len(g.updates.Current().InFlight) != 0 {
		t.Fatal("failed commit intent was acknowledged", err)
	}
}

func TestNodeReservationsAreBoundedAndDoNotPauseBusyNodes(t *testing.T) {
	g, d, info := deferralFixture(t)
	var peers []rolloutPeer
	for _, id := range []string{"a", "b", "c", "busy"} {
		s := update.Status{ID: d.ID, Software: info, State: "staged", Busy: id == "busy"}
		s.Software.Version, s.Software.SHA256 = "older", "older"
		peers = append(peers, rolloutPeer{node: model.Node{ID: id, OS: info.OS, Software: info, Online: true}, status: s, fresh: true})
	}
	prepared := []string{}
	_, err := g.advanceNodes(d, peers, true, func(id, kind string, _ update.Command) {
		if kind == "update.prepare" {
			prepared = append(prepared, id)
		}
	})
	if err != nil || !slices.Equal(prepared, []string{"a", "b"}) {
		t.Fatal("unbounded reservations", prepared, err)
	}
}

func TestIncompatibleExecutableDoesNotBypassSelectedGateway(t *testing.T) {
	g := &Gateway{options: Options{Software: buildinfo.Info{GatewayProtocol: 1}}}
	s := update.Status{StagedSoftware: buildinfo.Info{NodeGatewayMin: 2, NodeGatewayMax: 2}}
	if g.nodeMayUpdate(&update.Deployment{}, s, false) || g.nodeMayUpdate(&update.Deployment{}, s, true) {
		t.Fatal("incompatible node was eligible")
	}
}

func TestSupervisedSelectedGatewayDoesNotBlockNodeMigration(t *testing.T) {
	g, d, info := deferralFixture(t)
	g.options.Software.Runtime = "supervised"
	s := update.Status{ID: d.ID, Software: info, State: "staged", SeenAt: time.Now()}
	s.Software.Runtime = "supervised"
	// A restarted legacy child can retain its selection/lease without separate
	// staged compatibility metadata. The selected gateway code is already live.
	if got := g.nodeUpdateProgress(d, s, time.Now()); got == "waiting-gateway" {
		t.Fatal("single-process migration was confused with gateway software compatibility")
	}
	asset, _ := d.Manifest.Asset(info.OS, info.Arch)
	if update.IsCurrent(g.options.Software, d.Manifest.Version, asset) || updateAppliedAndResumed(d, s) {
		t.Fatal("retained supervisors were falsely acknowledged as migrated")
	}
	g.options.Software.Version, g.options.Software.SHA256 = "older", "older"
	s.Software.Version, s.Software.SHA256 = "older", "older"
	if got := g.nodeUpdateProgress(d, s, time.Now()); got != "waiting-gateway" {
		t.Fatal("unknown staged compatibility bypassed an older gateway", got)
	}
}

func TestSupervisedGatewayRolloutReportsStartupRefreshWithoutRetry(t *testing.T) {
	g, d, _ := deferralFixture(t)
	s := g.updateStatus["online"]
	s.Software.Runtime, s.State = "supervised", "staged"
	g.updateStatus["online"] = s
	if err := g.updates.NodeInstall(d.ID, "online", true); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); g.rolloutLoop(ctx) }()
	defer func() { cancel(); <-done }()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case packet := <-g.peers["online"].out:
			t.Fatal("selected supervised runtime received a redundant update", packet.Kind)
		case <-ticker.C:
			current := g.updates.Current()
			if current.Phase != "blocked" {
				continue
			}
			if len(current.InFlight) != 0 || current.Error == "" || current.GatewayState != "applied" {
				t.Fatal("migration blocker was not durably distinguished from an active install", current)
			}
			manifest := d.Manifest
			manifest.Version = "recovery-build"
			if _, err := g.updates.Publish(manifest, "test"); err != nil {
				t.Fatal("settled startup blockers prevented publishing their fix", err)
			}
			return
		case <-ctx.Done():
			t.Fatal("running selected gateway code left its in-flight node waiting forever")
		}
	}
}

func TestSupervisedNodeBlockersReleaseRolloutCapacity(t *testing.T) {
	g, d, info := deferralFixture(t)
	g.options.Software.Runtime = "supervised"
	asset, _ := d.Manifest.Asset(info.OS, info.Arch)
	gatewaySoftwareCurrent := update.IsSoftwareCurrent(g.options.Software, d.Manifest.Version, asset)
	var peers []rolloutPeer
	for _, id := range []string{"migration-a", "migration-b"} {
		s := update.Status{ID: d.ID, Software: info, State: "staged"}
		s.Software.Runtime = "supervised"
		peers = append(peers, rolloutPeer{node: model.Node{ID: id, OS: info.OS, Software: s.Software, Online: true}, status: s, fresh: true})
		if err := g.updates.NodeInstall(d.ID, id, true); err != nil {
			t.Fatal(err)
		}
	}
	prepared := []string{}
	allApplied, err := g.advanceNodes(g.updates.Current(), peers, gatewaySoftwareCurrent, func(id, kind string, _ update.Command) {
		t.Error("settled supervised runtime was retried", id, kind)
	})
	if err != nil || allApplied || len(g.updates.Current().InFlight) != 0 {
		t.Fatal("exact selected bytes did not settle the binary switch separately from migration", allApplied, err)
	}
	pending := update.Status{ID: d.ID, Software: info, State: "staged"}
	pending.Software.Version, pending.Software.SHA256 = "older", "older"
	peers = append(peers, rolloutPeer{node: model.Node{ID: "pending", OS: info.OS, Software: pending.Software, Online: true}, status: pending, fresh: true})
	prepared = nil
	allApplied, err = g.advanceNodes(g.updates.Current(), peers, gatewaySoftwareCurrent, func(id, kind string, _ update.Command) {
		if kind == "update.prepare" {
			prepared = append(prepared, id)
		}
	})
	if err != nil || allApplied || len(g.updates.Current().InFlight) != 0 || !slices.Equal(prepared, []string{"pending"}) {
		t.Fatal("migration blockers monopolized rollout capacity", prepared, allApplied, err)
	}
}

func TestStartupRefreshDoesNotSettleUncertainCommits(t *testing.T) {
	for _, name := range []string{"paused", "restarting", "stale", "different-checksum", "different-deployment"} {
		t.Run(name, func(t *testing.T) {
			g, d, info := deferralFixture(t)
			s := update.Status{ID: d.ID, Software: info, State: "staged"}
			s.Software.Runtime = "supervised"
			peer := rolloutPeer{node: g.nodes["online"], status: s, fresh: true}
			switch name {
			case "paused":
				peer.status.Paused = true
			case "restarting":
				peer.status.State = "restarting"
			case "stale":
				peer.fresh = false
			case "different-checksum":
				peer.status.Software.SHA256 = "different"
			case "different-deployment":
				peer.status.ID = "other"
			}
			if err := g.updates.NodeInstall(d.ID, "online", true); err != nil {
				t.Fatal(err)
			}
			_, err := g.advanceNodes(g.updates.Current(), []rolloutPeer{peer}, true, func(string, string, update.Command) {})
			if err != nil || len(g.updates.Current().InFlight) != 1 {
				t.Fatal("uncertain commit was discarded", name, err)
			}
			manifest := d.Manifest
			manifest.Version = "replacement"
			if _, err := g.updates.Publish(manifest, "test"); err == nil {
				t.Fatal("uncertain commit allowed superseding publication")
			}
		})
	}
}
