package gateway

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/coder/websocket"
	"github.com/koltyakov/control/internal/identity"
	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/protocol"
	"github.com/koltyakov/control/internal/update"
)

func renamePacketFixture(t *testing.T) (*Gateway, testAdmin, model.Node) {
	t.Helper()
	g, server := installationFixture(t)
	user, admin := createTestUser(t, server, "rename-owner")
	id, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	n := model.Node{ID: id.ID, PublicKey: id.Public, UserID: user.ID, Name: "original"}
	g.mu.Lock()
	g.owners[n.ID], g.nodes[n.ID] = n.UserID, n
	err = g.persist()
	g.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	return g, admin, n
}

// No writer runs until the test explicitly consumes a packet, making queued
// and in-progress reservation checks independent of goroutine scheduling.
func renamePacketPeer(t *testing.T, g *Gateway, id, user string) *connection {
	t.Helper()
	c := budgetedConnection(g.admission, user)
	c.userID = user
	g.mu.Lock()
	g.peers[id] = c
	g.mu.Unlock()
	t.Cleanup(func() {
		g.mu.Lock()
		delete(g.peers, id)
		g.mu.Unlock()
		c.closeQueues()
	})
	return c
}

func TestMachineRenameNotificationPacketAccounting(t *testing.T) {
	g, admin, n := renamePacketFixture(t)
	first := renamePacketPeer(t, g, n.ID, n.UserID)
	second := renamePacketPeer(t, g, "same-fleet-client", n.UserID)
	second.client = true
	foreign := renamePacketPeer(t, g, "other-fleet", "other-user")
	if err := admin.JSON(t.Context(), http.MethodPatch, "/v1/fleet/nodes/"+n.ID, map[string]string{"name": "renamed"}, nil); err != nil {
		t.Fatal(err)
	}
	size := packetRetainedBytes(&protocol.Packet{Kind: "directory.changed", From: update.GatewaySender})
	if got := g.admission.packets.used.Load(); got != 2*size {
		t.Errorf("rename notifications reserved %d global bytes, want %d", got, 2*size)
	}
	if got := first.queues.account.used.Load(); got != 2*size {
		t.Errorf("rename notifications reserved %d account bytes, want %d", got, 2*size)
	}
	for _, c := range []*connection{first, second} {
		if c.queues.local.used.Load() != size || c.queued.Load() != 0 || len(c.out) != 1 {
			t.Errorf("notification accounting: local=%d payload=%d count=%d", c.queues.local.used.Load(), c.queued.Load(), len(c.out))
		}
	}
	if len(foreign.out) != 0 || foreign.queues.local.used.Load() != 0 || foreign.queues.account.used.Load() != 0 {
		t.Fatal("rename notification or budget crossed fleet boundary")
	}
	if g.reserveRestart(1) {
		g.releaseRestart()
		t.Error("queued rename notifications passed the restart fence")
	}
	p := <-first.out
	if p.Kind != "directory.changed" || p.From != update.GatewaySender {
		t.Fatal("rename notification changed", p)
	}
	// Dequeue and connection shutdown cannot release a packet owned by a write.
	first.closeQueues()
	if got := first.queues.local.used.Load(); got != size {
		t.Errorf("in-progress notification reservation = %d, want %d", got, size)
	}
	first.releasePacket(p)
	if got := g.admission.packets.used.Load(); got != size {
		t.Errorf("notification completion left %d bytes, want %d", got, size)
	}
	second.closeQueues()
	first.closeQueues()
	second.closeQueues()
	for _, c := range []*connection{first, second, foreign} {
		if c.queues.local.used.Load() != 0 || c.queues.account.used.Load() != 0 || c.queued.Load() != 0 {
			t.Errorf("drained notification accounting: local=%d account=%d payload=%d", c.queues.local.used.Load(), c.queues.account.used.Load(), c.queued.Load())
		}
	}
	if got := g.admission.packets.used.Load(); got != 0 {
		t.Fatalf("drained rename notifications left global packet bytes at %d", got)
	}
	if !g.reserveRestart(1) {
		t.Fatal("drained rename notifications still block the restart fence")
	}
	g.releaseRestart()
}

func renamePacketSocket(t *testing.T) *websocket.Conn {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = ws.CloseNow() }()
		for {
			if _, _, err := ws.Read(r.Context()); err != nil {
				return
			}
		}
	}))
	t.Cleanup(server.Close)
	ws, _, err := websocket.Dial(t.Context(), "ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ws.CloseNow() })
	return ws
}

func TestMachineRenameNotificationRejectedQueueAccounting(t *testing.T) {
	for _, reason := range []string{"full", "global", "account", "local", "closed"} {
		t.Run(reason, func(t *testing.T) {
			g, admin, n := renamePacketFixture(t)
			c := renamePacketPeer(t, g, n.ID, n.UserID)
			c.ws = renamePacketSocket(t)
			switch reason {
			case "full":
				for range cap(c.out) {
					if !c.enqueue(&protocol.Packet{Kind: "answer"}) {
						t.Fatal("fill signaling queue")
					}
				}
			case "global":
				g.admission.packets.limit = 1
			case "account":
				c.queues.account.limit = 1
			case "local":
				c.queues.local.limit = 1
			case "closed":
				c.closeQueues()
			}
			before := g.admission.packets.used.Load()
			if err := admin.JSON(t.Context(), http.MethodPatch, "/v1/fleet/nodes/"+n.ID, map[string]string{"name": "renamed"}, nil); err != nil {
				t.Fatal("committed rename failed because its notification was rejected:", err)
			}
			if got := g.admission.packets.used.Load(); got != before || c.queues.account.used.Load() != before || c.queues.local.used.Load() != before {
				t.Errorf("rejected notification changed budgets: global=%d account=%d local=%d, want %d", got, c.queues.account.used.Load(), c.queues.local.used.Load(), before)
			}
			// Preserve the existing cache-invalidation policy: a peer that cannot
			// accept the hint is closed, so reconnect refreshes its directory.
			if err := c.ws.Write(t.Context(), websocket.MessageBinary, []byte("unexpected open connection")); err == nil {
				t.Error("peer retained a stale directory after notification rejection")
			}
			c.closeQueues()
			if g.admission.packets.used.Load() != 0 || c.queues.account.used.Load() != 0 || c.queues.local.used.Load() != 0 {
				t.Fatal("rejected notification leaked or underflowed a packet budget")
			}
		})
	}
}

func TestDirectoryChangedPacketWriteAccounting(t *testing.T) {
	for _, fail := range []bool{false, true} {
		name := "success"
		if fail {
			name = "failure"
		}
		t.Run(name, func(t *testing.T) {
			a := newAdmission(DefaultLimits())
			c := budgetedConnection(a, "owner")
			c.ws = renamePacketSocket(t)
			t.Cleanup(c.closeQueues)
			packet := &protocol.Packet{Kind: "directory.changed", From: update.GatewaySender}
			if !c.enqueue(packet) {
				t.Fatal("enqueue rename notification")
			}
			packet = <-c.out
			c.closeQueues()
			size := packetRetainedBytes(packet)
			if a.packets.used.Load() != size || c.queues.account.used.Load() != size || c.queues.local.used.Load() != size {
				t.Fatal("in-progress notification released before write completion")
			}
			if fail {
				if err := c.ws.CloseNow(); err != nil {
					t.Fatal(err)
				}
			}
			err := c.writeQueuedPacket(t.Context(), packet)
			if (err != nil) != fail {
				t.Fatal("unexpected notification write result:", err)
			}
			if a.packets.used.Load() != 0 || c.queues.account.used.Load() != 0 || c.queues.local.used.Load() != 0 || c.queued.Load() != 0 {
				t.Fatal("completed notification write leaked or underflowed a packet budget")
			}
			var failures int64
			if fail {
				failures = 1
			}
			if got := a.writeFailures.Load(); got != failures {
				t.Fatalf("write failures = %d, want %d", got, failures)
			}
		})
	}
}
