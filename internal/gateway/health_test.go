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
	health := model.NodeHealth{ActiveCount: 3, Leased: true, System: model.SystemInfo{SampledAt: time.Now()}}
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
	}
	if got := read(b); got.Status != "online" || got.ActiveCount != 0 || got.ID != bi.ID {
		t.Fatal("cross-fleet health disclosed", got)
	}
	g.mu.Lock()
	c := g.peers[ai.ID]
	c.healthAt = time.Now().Add(-model.NodeHealthTTL - time.Second)
	n := g.nodes[ai.ID]
	g.mu.Unlock()
	if got := read(a); got.Status != "online" || got.ActiveCount != 0 || got.Leased {
		t.Fatal("stale health appeared live", got)
	}
	for _, invalid := range [][]byte{[]byte(`{"activeCount":-1}`), []byte(`{"task":"private"}`), []byte(strings.Repeat(" ", model.MaxNodeHealthBytes+1)), []byte(`{} {}`)} {
		if g.receiveHealth(n, c, invalid) {
			t.Fatal("invalid health accepted")
		}
	}
	if g.receiveHealth(n, &connection{}, model.JSON(health)) {
		t.Fatal("superseded connection replaced health")
	}
}
