package gateway

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/koltyakov/control/internal/buildinfo"
	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/protocol"
	"github.com/koltyakov/control/internal/update"
)

func deferralFixture(t *testing.T) (*Gateway, *update.Deployment, buildinfo.Info) {
	t.Helper()
	data := []byte("deferral fixture executable")
	sum := sha256.Sum256(data)
	asset := update.Asset{OS: "linux", Arch: "amd64", File: update.AssetName("linux", "amd64"), Size: int64(len(data)), SHA256: hex.EncodeToString(sum[:])}
	info := buildinfo.Info{Version: "v1", OS: asset.OS, Arch: asset.Arch, SHA256: asset.SHA256}
	g, err := New(t.TempDir(), commonKey, Options{SuperuserKey: superKey, Software: info})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// These fixtures have no WebSockets or connection goroutines.
		clear(g.peers)
		_ = g.Close()
	})
	if err := g.updates.Upload(bytes.NewReader(data), asset); err != nil {
		t.Fatal(err)
	}
	d, err := g.updates.Publish(update.Manifest{Version: info.Version, Assets: []update.Asset{asset}}, "development")
	if err != nil {
		t.Fatal(err)
	}
	if err := g.updates.BeginInstall(d.ID, []string{"online", "offline"}); err != nil {
		t.Fatal(err)
	}
	g.nodes["online"] = model.Node{ID: "online", OS: info.OS, Software: info, Online: true}
	g.nodes["offline"] = model.Node{ID: "offline", OS: info.OS, Software: info, LastSeen: time.Now().Add(-time.Hour)}
	g.peers["online"] = &connection{out: make(chan *protocol.Packet, 16)}
	g.updateStatus["online"] = update.Status{ID: d.ID, Software: info, State: "applied", SeenAt: time.Now()}
	g.updateStatus["offline"] = update.Status{ID: d.ID, State: "ready", Paused: true, SeenAt: time.Now().Add(-time.Hour)}
	return g, g.updates.Current(), info
}

func TestDeferredParticipantsKeepRestartGraceAndRequireAcknowledgment(t *testing.T) {
	g, d, info := deferralFixture(t)
	now := time.Now()
	g.nodes["offline"] = model.Node{ID: "offline", OS: info.OS, Software: info, LastSeen: now.Add(-updateReconnectGrace + time.Second)}
	current, err := g.reconcileDeferred(d, now)
	if err != nil || len(current.Deferred) != 0 || len(g.rolloutPeers(current)) != 2 {
		t.Fatal("normal restart lost its reconnection grace", current, err)
	}
	current, err = g.reconcileDeferred(current, now.Add(time.Second))
	if err != nil || !slices.Equal(current.Deferred, []string{"offline"}) || len(g.rolloutPeers(current)) != 1 {
		t.Fatal("expired offline participant still blocked rollout", current, err)
	}
	if !current.UpdatedAt.Equal(d.UpdatedAt) || g.updateStatus["offline"].State != "ready" {
		t.Fatal("deferral renewed installation or fabricated an applied acknowledgment")
	}
	reloaded, err := update.OpenRepository(filepath.Join(filepath.Dir(g.path), "updates"))
	if err != nil || !slices.Equal(reloaded.Current().Deferred, []string{"offline"}) {
		t.Fatal("deferral was not durable", err)
	}
	copy := reloaded.Current()
	copy.Deferred[0] = "changed"
	if reloaded.Current().Deferred[0] != "offline" {
		t.Fatal("deployment read aliases deferred state")
	}
	// A returning node immediately participates again, even before it has any
	// fresh report. Its stale pre-restart readiness cannot authorize a restart.
	g.peers["offline"] = &connection{out: make(chan *protocol.Packet, 16)}
	if len(g.rolloutPeers(current)) != 2 || g.reserveRestart(current, g.rolloutPeers(current)) {
		t.Fatal("returning deferred node bypassed current idle admission")
	}
	for _, status := range []update.Status{
		{ID: d.ID, Software: info, State: "applied", SeenAt: now.Add(-time.Minute)},
		{ID: "old", Software: info, State: "applied", SeenAt: now},
		{ID: d.ID, Software: info, State: "applied", Paused: true, SeenAt: now},
	} {
		g.updateStatus["offline"] = status
		current, err = g.reconcileDeferred(current, now)
		if err != nil || len(current.Deferred) != 1 {
			t.Fatal("unconfirmed return erased deferral", status, err)
		}
	}
	g.updateStatus["offline"] = update.Status{ID: d.ID, Software: info, State: "applied", SeenAt: now}
	current, err = g.reconcileDeferred(current, now)
	if err != nil || len(current.Deferred) != 0 {
		t.Fatal("fresh applied and resumed node remained deferred", current, err)
	}
}

func TestDeferredPersistenceFailureStillBlocksRollout(t *testing.T) {
	g, d, _ := deferralFixture(t)
	path := filepath.Join(filepath.Dir(g.path), "updates", "deployment.json")
	if err := os.Rename(path, path+".saved"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := g.reconcileDeferred(d, time.Now()); err == nil {
		t.Fatal("failed deferral write was acknowledged")
	}
	if len(g.updates.Current().Deferred) != 0 || len(g.rolloutPeers(g.updates.Current())) != 2 {
		t.Fatal("failed deferral removed a participant")
	}
}

func TestRolloutCompletesWithDeferredNodesAndUpdatesThemOnReturn(t *testing.T) {
	g, d, info := deferralFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); g.rolloutLoop(ctx) }()
	defer func() { cancel(); <-done }()
	waitPhase := func(phase string, resumed bool) {
		t.Helper()
		deadline := time.NewTimer(6 * time.Second)
		defer deadline.Stop()
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-deadline.C:
				t.Fatalf("rollout did not reach %s: %+v", phase, g.updates.Current())
			case <-ticker.C:
				current := g.updates.Current()
				if current.Phase == phase && (!resumed || len(current.Deferred) == 0) {
					return
				}
			}
		}
	}
	waitPhase("complete", false)
	if current := g.updates.Current(); !slices.Equal(current.Deferred, []string{"offline"}) || !slices.Contains(current.Participants, "offline") {
		t.Fatal("completion lost pending offline identity", current)
	}
	// Completing the available fleet must actually unblock another publication.
	next := d.Manifest
	next.Version = "v2"
	g.rolloutMu.Lock()
	_, err := g.updates.Publish(next, "development")
	g.rolloutMu.Unlock()
	if err != nil {
		t.Fatal("offline participant still blocked publication", err)
	}
	// Restore the already-running version for this fixture's catch-up flow.
	g.rolloutMu.Lock()
	_, err = g.updates.Publish(d.Manifest, "development")
	if err == nil {
		err = g.updates.SetDeferred(d.ID, []string{"offline"})
	}
	g.rolloutMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	out := make(chan *protocol.Packet, 16)
	g.mu.Lock()
	g.peers["offline"] = &connection{out: out}
	g.mu.Unlock()
	select {
	case packet := <-out:
		if packet.Kind != "update.offer" {
			t.Fatal("returning node was not offered the selected update", packet.Kind)
		}
	case <-time.After(6 * time.Second):
		t.Fatal("returning deferred node never received its update")
	}
	g.mu.Lock()
	g.updateStatus["online"] = update.Status{ID: d.ID, Software: info, State: "applied", SeenAt: time.Now()}
	g.updateStatus["offline"] = update.Status{ID: d.ID, Software: info, State: "applied", SeenAt: time.Now()}
	g.mu.Unlock()
	waitPhase("complete", true)
	if len(g.updates.Current().Deferred) != 0 {
		t.Fatal("catch-up did not clear pending deferral")
	}
}

func TestUpdateSelectionExposesNoFleetRolloutData(t *testing.T) {
	g, d, _ := deferralFixture(t)
	if err := g.updates.SetDeferred(d.ID, []string{"offline"}); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(g.Handler())
	defer server.Close()
	request, err := http.NewRequest(http.MethodGet, server.URL+"/v1/updates", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+commonKey)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	var fields map[string]json.RawMessage
	if err := json.NewDecoder(response.Body).Decode(&fields); err != nil || response.StatusCode != 200 || len(fields) != 2 || fields["manifest"] == nil || fields["id"] == nil {
		t.Fatal("selection leaked rollout data or omitted the manifest", fields, err)
	}
	if err := (testAdmin{URL: server.URL, Key: "invalid"}).JSON(t.Context(), "GET", "/v1/updates", nil, nil); err == nil {
		t.Fatal("unauthenticated caller read the selection")
	}
}
