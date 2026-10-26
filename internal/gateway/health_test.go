package gateway

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/koltyakov/control/internal/identity"
	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/protocol"
	"google.golang.org/protobuf/proto"
)

func TestHealthIdentityFleetAndFreshness(t *testing.T) {
	g, s := installationFixture(t)
	alice, a := createTestUser(t, s, "alice")
	bob, b := createTestUser(t, s, "bob")
	ai, err := identity.Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	bi, err := identity.Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	wa := rawPeer(t, s.URL, a.Key, "worker", alice.ID, ai)
	wb := rawPeer(t, s.URL, b.Key, "worker", bob.ID, bi)
	if wa == nil || wb == nil {
		t.Fatal("registration failed")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	health := model.NodeHealth{ActiveCount: 3, Leased: true, System: model.SystemInfo{SampledAt: time.Now()},
		Work: &model.WorkCounts{Tasks: 1, Transfers: 1}, Tunnels: &model.TunnelCounts{Forward: 1},
		ConnectionRoutes: &model.ConnectionRouteCounts{Direct: 2, Relay: 1}, LeaseExpires: time.Now().Add(time.Minute).UTC()}
	usage, used := 42.0, uint64(2<<30)
	health.System.GPUs = []model.GPUInfo{{Name: "NVIDIA Test", Vendor: "NVIDIA", Kind: model.GPUDiscrete, UsagePercent: &usage, MemoryTotalBytes: 8 << 30, MemoryUsedBytes: &used}}
	packet, err := proto.Marshal(&protocol.Packet{Kind: "node.health", From: bi.ID, Data: model.JSON(health)})
	if err != nil {
		t.Fatal(err)
	}
	if err = wa.Write(ctx, websocket.MessageBinary, packet); err != nil {
		t.Fatal(err)
	}
	for {
		g.mu.Lock()
		received := g.peers[ai.ID].health != nil
		g.mu.Unlock()
		if received {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("health not received")
		case <-time.After(10 * time.Millisecond):
		}
	}
	read := func(c testAdmin) model.NodeActivitySnapshot {
		t.Helper()
		var pool model.PoolActivitySnapshot
		if err := c.JSON(ctx, "GET", "/v1/status", nil, &pool); err != nil || len(pool.Nodes) != 1 {
			t.Fatal("status", err)
		}
		return pool.Nodes[0]
	}
	if got := read(a); got.Status != "summary" || got.ActiveCount != 3 || !got.Leased || got.ID != ai.ID {
		t.Fatal("sender identity not used", got)
	} else if got.Work == nil || *got.Work != *health.Work || got.Tunnels == nil || got.Tunnels.Forward != 1 ||
		got.DirectSessions != 2 || got.RelaySessions != 1 || !got.LeaseExpires.Equal(health.LeaseExpires) {
		t.Fatal("aggregate work and connection counts not reported", got)
	} else if got.System == nil || len(got.System.GPUs) != 1 || got.System.GPUs[0].Kind != model.GPUDiscrete || got.System.GPUs[0].UsagePercent == nil || *got.System.GPUs[0].UsagePercent != 42 || got.System.GPUs[0].MemoryUsedBytes == nil || *got.System.GPUs[0].MemoryUsedBytes != used {
		t.Fatal("GPU metrics not reported to owner", got)
	}
	if got := read(b); got.Status != "online" || got.ActiveCount != 0 || got.ID != bi.ID {
		t.Fatal("cross-fleet health disclosed", got)
	}
	g.mu.Lock()
	c := g.peers[ai.ID]
	c.healthAt = time.Now().Add(-model.NodeHealthTTL - time.Second)
	n := g.nodes[ai.ID]
	g.publishObservations(ai.ID)
	g.mu.Unlock()
	if got := read(a); got.Status != "online" || got.ActiveCount != 0 || got.Leased {
		t.Fatal("stale health appeared live", got)
	}
	for _, invalid := range [][]byte{[]byte(`{"activeCount":-1}`), []byte(`{"work":{"tasks":-1}}`), []byte(`{"connectionRoutes":{"relay":-1}}`), []byte(strings.Repeat(" ", model.MaxNodeHealthBytes+1)), []byte(`{} {}`)} {
		if g.receiveHealth(n, c, invalid) {
			t.Fatal("invalid health accepted")
		}
	}
	for _, gpu := range []model.GPUInfo{{Name: "GPU", Kind: "bad-kind"}, {Name: "GPU", UsagePercent: new(101.0)}, {Name: "GPU", MemoryTotalBytes: 1, MemoryUsedBytes: new(uint64(2))}, {Name: strings.Repeat("x", 129)}} {
		invalid := health
		invalid.System.GPUs = []model.GPUInfo{gpu}
		if g.receiveHealth(n, c, model.JSON(invalid)) {
			t.Fatal("invalid GPU metrics accepted", gpu)
		}
	}
	tooMany := health
	tooMany.System.GPUs = make([]model.GPUInfo, model.MaxGPUs+1)
	for i := range tooMany.System.GPUs {
		tooMany.System.GPUs[i].Name = "GPU"
	}
	if g.receiveHealth(n, c, model.JSON(tooMany)) {
		t.Fatal("too many GPU records accepted")
	}
	// A newer node's additional fields are ignored, not retained or rejected.
	if !g.receiveHealth(n, c, []byte(`{"activeCount":2,"task":"private","system":{"futureMetric":1}}`)) {
		t.Fatal("health from a newer node disconnected it")
	}
	g.mu.Lock()
	retained := string(model.JSON(c.health))
	g.mu.Unlock()
	if strings.Contains(retained, "private") || strings.Contains(retained, "futureMetric") || c.health.ActiveCount != 2 {
		t.Fatal("unknown health fields retained", retained)
	}
	if g.receiveHealth(n, &connection{}, model.JSON(health)) {
		t.Fatal("superseded connection replaced health")
	}
}
