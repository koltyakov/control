package gateway

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/koltyakov/control/internal/buildinfo"
	"github.com/koltyakov/control/internal/identity"
	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/protocol"
	"github.com/koltyakov/control/internal/update"
	"google.golang.org/protobuf/proto"
)

func TestUpdateStatusSurvivesDisconnectReconnectAndGatewayRestart(t *testing.T) {
	dir := t.TempDir()
	g, err := New(dir, commonKey)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = g.Close() })
	s := httptest.NewServer(g.Handler())
	t.Cleanup(s.Close)
	data := []byte("update fixture")
	sum := sha256.Sum256(data)
	asset := update.Asset{OS: "linux", Arch: "amd64", File: update.AssetName("linux", "amd64"), Size: int64(len(data)), SHA256: hex.EncodeToString(sum[:])}
	if err := g.updates.Upload(bytes.NewReader(data), asset); err != nil {
		t.Fatal(err)
	}
	d, err := g.updates.Publish(update.Manifest{Version: "v2", Assets: []update.Asset{asset}}, "development")
	if err != nil {
		t.Fatal(err)
	}
	id, err := identity.Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ws := rawPeer(t, s.URL, commonKey, "worker", legacyUser, id)
	g.mu.Lock()
	n := g.nodes[id.ID]
	n.OS, n.Software = "linux", buildinfo.Info{OS: "linux", Arch: "amd64", Version: "v1"}
	g.nodes[id.ID] = n
	c := g.peers[id.ID]
	g.mu.Unlock()
	graceZero := func() bool {
		g.mu.Lock()
		defer g.mu.Unlock()
		return g.nodes[id.ID].UpdateUntil.IsZero()
	}
	if !g.receiveUpdateStatus(n, c, model.JSON(update.Status{ID: "old-deployment", State: "restarting"})) || !graceZero() {
		t.Fatal("stale deployment started restart grace")
	}
	g.mu.Lock()
	current := n
	current.Software.Version = "v2"
	g.nodes[id.ID] = current
	g.mu.Unlock()
	if !g.receiveUpdateStatus(current, c, model.JSON(update.Status{ID: d.ID, State: "restarting"})) || !graceZero() {
		t.Fatal("same-version skip started restart grace")
	}
	if !g.receiveUpdateStatus(current, c, model.JSON(update.Status{ID: d.ID, State: "applied"})) {
		t.Fatal("applied status rejected")
	}
	g.mu.Lock()
	g.nodes[id.ID] = n
	g.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	packet, err := proto.Marshal(&protocol.Packet{Kind: "update.status", From: "forged", Data: model.JSON(update.Status{ID: d.ID, State: "restarting"})})
	if err != nil {
		t.Fatal(err)
	}
	if err := ws.Write(ctx, websocket.MessageBinary, packet); err != nil {
		t.Fatal(err)
	}
	wait := func(check func() bool) {
		t.Helper()
		for {
			g.mu.Lock()
			ok := check()
			g.mu.Unlock()
			if ok {
				return
			}
			select {
			case <-ctx.Done():
				t.Fatal("update state did not change")
			case <-time.After(10 * time.Millisecond):
			}
		}
	}
	wait(func() bool { return !g.nodes[id.ID].UpdateUntil.IsZero() })
	_ = ws.CloseNow()
	wait(func() bool { return !g.nodes[id.ID].Online })
	g.mu.Lock()
	n = g.nodes[id.ID]
	g.mu.Unlock()
	if n.UpdateUntil.Before(n.LastSeen.Add(time.Minute)) {
		t.Fatal("restart grace is less than a minute")
	}
	read := func() model.NodeActivitySnapshot {
		t.Helper()
		var pool model.PoolActivitySnapshot
		if err := (testAdmin{URL: s.URL, Key: commonKey}).JSON(ctx, "GET", "/v1/status", nil, &pool); err != nil || len(pool.Nodes) != 1 {
			t.Fatal("status", err)
		}
		return pool.Nodes[0]
	}
	if got := read(); got.Status != "update" || got.Online {
		t.Fatal("restart displayed offline or falsified presence", got)
	}
	ws = rawPeer(t, s.URL, commonKey, "worker", legacyUser, id)
	if got := read(); got.Status != "update" || !got.Online {
		t.Fatal("reconnection cleared update grace", got)
	}
	_ = ws.CloseNow()
	wait(func() bool { return !g.nodes[id.ID].Online })
	s.Close()
	_ = g.Close()
	g, err = New(dir, commonKey)
	if err != nil {
		t.Fatal(err)
	}
	s = httptest.NewServer(g.Handler())
	t.Cleanup(s.Close)
	if got := read(); got.Status != "update" || got.Online {
		t.Fatal("gateway restart lost update grace", got)
	}
	n = g.nodes[id.ID]
	if !g.nodeUpdating(n, n.UpdateUntil.Add(-time.Nanosecond)) || g.nodeUpdating(n, n.UpdateUntil) {
		t.Fatal("update grace boundary is incorrect")
	}
	g.mu.Lock()
	n.UpdateUntil = time.Now().Add(-time.Second)
	g.nodes[id.ID] = n
	g.mu.Unlock()
	if got := read(); got.Status != "offline" {
		t.Fatal("expired update grace hid offline status", got)
	}
}

func TestNodeUpdatingRequiresFreshProgressOrRestartGrace(t *testing.T) {
	g, d, asset := updateStatusFixture(t)
	now := time.Now().UTC()
	n := model.Node{ID: "worker", Online: true}
	for _, tc := range []struct {
		state    string
		online   bool
		age      time.Duration
		updating bool
	}{
		{"downloading", true, 0, true},
		{"staged", true, 0, true},
		{"busy", true, 0, true},
		{"ready", true, 0, true},
		{"restarting", true, 0, true},
		{"applied", true, 0, false},
		{"idle", true, 0, false},
		{"error", true, 0, false},
		{"restarting", false, 0, false},
		{"ready", true, 8 * time.Second, false},
	} {
		n.Online = tc.online
		g.updateStatus[n.ID] = update.Status{ID: d.ID, State: tc.state, SeenAt: now.Add(-tc.age)}
		if got := g.nodeUpdating(n, now); got != tc.updating {
			t.Errorf("state=%s online=%v age=%s: updating=%v", tc.state, tc.online, tc.age, got)
		}
	}
	n.Online = true
	for _, state := range []string{"downloading", "staged", "busy", "ready", "restarting"} {
		g.updateStatus[n.ID] = update.Status{ID: "previous-deployment", State: state, SeenAt: now}
		if g.nodeUpdating(n, now) {
			t.Errorf("old deployment's %s status still displays update", state)
		}
	}
	current := buildinfo.Info{OS: asset.OS, Arch: asset.Arch, Version: d.Manifest.Version}
	for _, state := range []string{"applied", "idle", "ready", "staged"} {
		g.updateStatus[n.ID] = update.Status{ID: d.ID, State: state, Software: current, SeenAt: now}
		n.UpdateUntil = now.Add(time.Minute)
		if g.nodeUpdating(n, now) {
			t.Errorf("current resumed node's %s status still displays update", state)
		}
	}
	g.updateStatus[n.ID] = update.Status{ID: d.ID, State: "applied", Software: current, Paused: true, SeenAt: now}
	n.UpdateUntil = time.Time{}
	if !g.nodeUpdating(n, now) {
		t.Fatal("current node still in maintenance displayed normal availability")
	}
}

func updateStatusFixture(t *testing.T) (*Gateway, *update.Deployment, update.Asset) {
	t.Helper()
	g, err := New(t.TempDir(), commonKey, Options{SuperuserKey: superKey})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = g.Close() })
	data := []byte("update display fixture")
	sum := sha256.Sum256(data)
	asset := update.Asset{OS: "linux", Arch: "amd64", File: update.AssetName("linux", "amd64"), Size: int64(len(data)), SHA256: hex.EncodeToString(sum[:])}
	if err := g.updates.Upload(bytes.NewReader(data), asset); err != nil {
		t.Fatal(err)
	}
	d, err := g.updates.Publish(update.Manifest{Version: "v2", Assets: []update.Asset{asset}}, "development")
	if err != nil {
		t.Fatal(err)
	}
	return g, d, asset
}

func TestResumedUpdateClearsGraceAndRestoresHealthDurably(t *testing.T) {
	g, d, asset := updateStatusFixture(t)
	s := httptest.NewServer(g.Handler())
	defer s.Close()
	id, err := identity.Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ws := rawPeer(t, s.URL, commonKey, "worker", legacyUser, id)
	defer func() { _ = ws.CloseNow() }()
	g.mu.Lock()
	n := g.nodes[id.ID]
	n.OS, n.Software = asset.OS, buildinfo.Info{OS: asset.OS, Arch: asset.Arch, Version: "v1"}
	g.nodes[id.ID] = n
	c := g.peers[id.ID]
	g.mu.Unlock()
	if !g.receiveUpdateStatus(n, c, model.JSON(update.Status{ID: d.ID, State: "restarting", Software: n.Software})) {
		t.Fatal("restart status rejected")
	}
	current := buildinfo.Info{OS: asset.OS, Arch: asset.Arch, Version: d.Manifest.Version}
	for _, status := range []update.Status{
		{ID: "previous-deployment", State: "applied", Software: current},
		{ID: d.ID, State: "applied", Software: n.Software},
		{ID: d.ID, State: "applied", Software: current, Paused: true},
	} {
		if !g.receiveUpdateStatus(n, c, model.JSON(status)) {
			t.Fatal("status rejected")
		}
		g.mu.Lock()
		pending := !g.nodes[id.ID].UpdateUntil.IsZero()
		g.mu.Unlock()
		if !pending {
			t.Fatal("unconfirmed or still-paused update cleared restart grace", status)
		}
	}
	system := model.SystemInfo{OS: asset.OS, Arch: asset.Arch, SampledAt: time.Now()}
	if !g.receiveHealth(n, c, model.JSON(model.NodeHealth{System: system})) {
		t.Fatal("health rejected")
	}
	read := func() model.NodeActivitySnapshot {
		t.Helper()
		var pool model.PoolActivitySnapshot
		if err := (testAdmin{URL: s.URL, Key: superKey}).JSON(t.Context(), "GET", "/v1/status", nil, &pool); err != nil || len(pool.Nodes) != 1 {
			t.Fatal("status", err)
		}
		return pool.Nodes[0]
	}
	if read().Status != "update" {
		t.Fatal("maintenance was not displayed")
	}
	if !g.receiveUpdateStatus(n, c, model.JSON(update.Status{ID: d.ID, State: "applied", Software: current})) {
		t.Fatal("resumed status rejected")
	}
	if got := read(); got.Status != "summary" || got.ActiveCount != 0 {
		t.Fatal("completed update did not restore idle health", got)
	}
	if !g.receiveHealth(n, c, model.JSON(model.NodeHealth{ActiveCount: 2, System: system})) {
		t.Fatal("busy health rejected")
	}
	if got := read(); got.Status != "summary" || got.ActiveCount != 2 {
		t.Fatal("completed update did not preserve actual busy health", got)
	}
	g.mu.Lock()
	cleared := g.nodes[id.ID].UpdateUntil.IsZero()
	g.mu.Unlock()
	if !cleared {
		t.Fatal("completed update retained restart grace")
	}
	_ = ws.CloseNow()
	s.Close()
	_ = g.Close()
	reopened, err := New(filepath.Dir(g.path), commonKey)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	if !reopened.nodes[id.ID].UpdateUntil.IsZero() {
		t.Fatal("gateway restart restored cleared update grace")
	}
}
