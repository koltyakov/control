package gateway

import (
	"slices"
	"testing"
	"time"

	"github.com/koltyakov/control/internal/buildinfo"
	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/update"
)

func TestGatewayHoldsRestartWhileNodesDownload(t *testing.T) {
	asset := update.Asset{OS: "linux", Arch: "amd64", File: update.AssetName("linux", "amd64"), Size: 1, SHA256: "aa"}
	d := &update.Deployment{ID: "next", Manifest: update.Manifest{Version: "v2", Assets: []update.Asset{asset}}}
	old := model.Node{ID: "worker", Online: true, Software: buildinfo.Info{Version: "v1", OS: "linux", Arch: "amd64", SHA256: "bb"}}
	current := old
	current.Software.Version, current.Software.SHA256 = "v2", "aa"
	staged := time.Now()
	peer := func(n model.Node, id, state string) rolloutPeer {
		return rolloutPeer{node: n, fresh: true, status: update.Status{ID: id, State: state}}
	}
	for _, tc := range []struct {
		name  string
		peers []rolloutPeer
		at    time.Time
		hold  bool
	}{
		{"offer not yet sent", []rolloutPeer{peer(old, "previous", "applied")}, staged.Add(time.Second), false},
		{"node downloading", []rolloutPeer{peer(old, "next", "downloading")}, staged.Add(time.Minute), true},
		{"node staged", []rolloutPeer{peer(old, "next", "staged")}, staged.Add(time.Second), false},
		{"node already current", []rolloutPeer{peer(current, "previous", "applied")}, staged.Add(time.Second), false},
		{"stale peer", []rolloutPeer{{node: old, status: update.Status{ID: "next", State: "downloading"}}}, staged.Add(time.Second), false},
		{"hold bound reached", []rolloutPeer{peer(old, "next", "downloading")}, staged.Add(gatewayDownloadHold), false},
	} {
		if got := gatewayHoldForDownloads(d, tc.peers, staged, tc.at); got != tc.hold {
			t.Errorf("%s: hold = %v, want %v", tc.name, got, tc.hold)
		}
	}
}

func TestGatewayGoesFirstUnlessNodesMustLead(t *testing.T) {
	selected := time.Now()
	compatible := buildinfo.Info{Version: "v1", GatewayProtocol: 1, NodeGatewayMin: 0, NodeGatewayMax: 1}
	legacy := buildinfo.Info{Version: "v0"}
	peer := func(info buildinfo.Info) rolloutPeer {
		return rolloutPeer{node: model.Node{ID: "worker", Online: true, Software: info}, fresh: true, status: update.Status{Software: info}}
	}
	epoch := func(n int) buildinfo.Info { return buildinfo.Info{GatewayProtocol: n} }
	for _, tc := range []struct {
		name   string
		peers  []rolloutPeer
		staged bool
		path   string
		info   buildinfo.Info
		at     time.Time
		hold   bool
	}{
		{"staging", []rolloutPeer{peer(compatible)}, false, "", buildinfo.Info{}, selected.Add(time.Second), true},
		{"staged and compatible", []rolloutPeer{peer(compatible)}, true, "/staged", epoch(1), selected.Add(time.Second), true},
		{"legacy node on unchanged epoch", []rolloutPeer{peer(legacy)}, true, "/staged", epoch(1), selected.Add(time.Second), true},
		{"node cannot reach new epoch", []rolloutPeer{peer(compatible)}, true, "/staged", epoch(2), selected.Add(time.Second), false},
		{"legacy node beyond epoch 1", []rolloutPeer{peer(legacy)}, true, "/staged", epoch(2), selected.Add(time.Second), false},
		{"gateway cannot self-apply", []rolloutPeer{peer(compatible)}, true, "", epoch(1), selected.Add(time.Second), false},
		{"window elapsed", []rolloutPeer{peer(compatible)}, false, "", buildinfo.Info{}, selected.Add(gatewayFirstWindow), false},
		{"no selection time", []rolloutPeer{peer(compatible)}, true, "/staged", epoch(1), time.Time{}, false},
	} {
		start, at := selected, tc.at
		if at.IsZero() { // The rollout loop has not selected this deployment yet.
			start, at = time.Time{}, selected
		}
		if got := gatewayGoesFirst(tc.peers, tc.staged, tc.path, tc.info, start, at); got != tc.hold {
			t.Errorf("%s: gateway first = %v, want %v", tc.name, got, tc.hold)
		}
	}
}

func TestHeldNodesReceiveNoOffersOrCommits(t *testing.T) {
	g, d, info := deferralFixture(t)
	delete(g.nodes, "offline")
	delete(g.updateStatus, "offline")
	d.Participants = nil
	s := g.updateStatus["online"]
	s.Software.Version, s.Software.SHA256 = "older", "older"
	s.Paused, s.State, s.LeaseUntil = true, "ready", time.Now().Add(time.Minute)
	info.NodeGatewayMax = 1
	s.StagedSoftware = info
	g.updateStatus["online"] = s
	peers := g.rolloutPeers(d)
	stale := rolloutPeer{node: model.Node{ID: "fresh", OS: "linux", Software: s.Software}, status: update.Status{ID: "previous", Software: s.Software, State: "applied"}, fresh: true}
	peers = append(peers, stale)
	var sent []string
	record := func(id, kind string, _ update.Command) { sent = append(sent, id+" "+kind) }
	if _, err := g.advanceNodes(d, peers, false, true, record); err != nil {
		t.Fatal(err)
	}
	if len(sent) != 0 || len(g.updates.Current().InFlight) != 0 {
		t.Fatalf("held nodes received %v, in flight %v", sent, g.updates.Current().InFlight)
	}
	if _, err := g.advanceNodes(d, peers, true, false, record); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(sent, "fresh update.offer") || !slices.Contains(sent, "online update.commit") {
		t.Fatalf("released nodes received %v", sent)
	}
}
