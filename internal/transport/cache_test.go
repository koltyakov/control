package transport

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/koltyakov/control/internal/gateway"
	"github.com/koltyakov/control/internal/identity"
	"github.com/koltyakov/control/internal/model"
)

func echoPeers(t *testing.T, ctx context.Context, url, token string, relay bool, names ...string) []*Peer {
	t.Helper()
	peers := []*Peer{}
	for _, name := range names {
		id, err := identity.Generate()
		if err != nil {
			t.Fatal(err)
		}
		p := New(Config{Gateway: url, Token: token, Identity: id, Node: model.Node{ID: id.ID, Name: name, PublicKey: id.Public}, RelayOnly: relay}, func(_ string, conn net.Conn) { _, _ = io.Copy(conn, conn) })
		if err := p.Start(ctx); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = p.Close() })
		peers = append(peers, p)
	}
	return peers
}

func waitFor(t *testing.T, ctx context.Context, check func() bool) {
	t.Helper()
	for !check() {
		select {
		case <-ctx.Done():
			t.Fatal("condition not reached")
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func TestLookupCacheEvictedByOfflineNotification(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	const token = "lookup-cache-test-token-1234567890"
	g, err := gateway.New(t.TempDir(), token)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = g.Close() }()
	s := httptest.NewServer(g.Handler())
	defer s.Close()
	peers := echoPeers(t, ctx, s.URL, token, true, "alpha", "beta")
	alpha, beta := peers[0], peers[1]
	for range 3 {
		if _, err := alpha.Resolve(ctx, beta.IdentityID()); err != nil {
			t.Fatal(err)
		}
	}
	if stats := alpha.Stats(); stats.LookupMisses != 1 || stats.LookupHits < 2 {
		t.Fatalf("identity lookups were not reused: %+v", stats)
	}
	// One directory read resolves every listed machine by name as well.
	if _, err := alpha.Lookup(ctx, "beta"); err != nil {
		t.Fatal(err)
	}
	_ = beta.Close()
	waitFor(t, ctx, func() bool {
		alpha.mu.Lock()
		defer alpha.mu.Unlock()
		_, byName := alpha.lookups["beta"]
		_, byID := alpha.lookups[beta.IdentityID()]
		return !byName && !byID && !alpha.members[beta.IdentityID()]
	})
	if _, err := alpha.Resolve(ctx, "beta"); err == nil {
		t.Fatal("offline machine resolved from a stale cache entry")
	}
}

func TestPeersNegotiateLargePackets(t *testing.T) {
	for _, tc := range []struct {
		name    string
		relay   bool
		legacy  bool
		packets int
	}{
		{"relay", true, false, largePacketSize},
		{"webrtc", false, false, largePacketSize},
		{"older-gateway", true, true, legacyPacketSize},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			const token = "large-packet-test-token-1234567890"
			g, err := gateway.New(t.TempDir(), token)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = g.Close() }()
			handler := g.Handler()
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tc.legacy && r.URL.Path == "/v1/auth" {
					_, _ = w.Write([]byte(`{"userId":"legacy","nodeHealth":true,"clientSessions":true,"clientOwners":true,"peerChannels":true,"delegation":true}`))
					return
				}
				handler.ServeHTTP(w, r)
			}))
			defer s.Close()
			peers := echoPeers(t, ctx, s.URL, token, tc.relay, "alpha", "beta")
			conn, err := peers[0].OpenLane(ctx, "beta", BulkLane)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = conn.Close() }()
			payload := bytes.Repeat([]byte("0123456789abcdef"), 64<<10)
			go func() { _, _ = conn.Write(payload) }()
			echoed := make([]byte, len(payload))
			_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
			if _, err := io.ReadFull(conn, echoed); err != nil || !bytes.Equal(echoed, payload) {
				t.Fatal("echo through negotiated packets failed", err)
			}
			for _, p := range peers {
				p.mu.Lock()
				for _, l := range p.links {
					if l.conn.packetSize != tc.packets {
						p.mu.Unlock()
						t.Fatalf("link uses %d-byte packets, want %d", l.conn.packetSize, tc.packets)
					}
				}
				p.mu.Unlock()
			}
		})
	}
}

func TestLeastLoadedPrefersIdleMachines(t *testing.T) {
	nodes := func() []model.Node {
		return []model.Node{{ID: "busy"}, {ID: "idle"}, {ID: "unknown"}}
	}
	for range 20 {
		if got := leastLoaded(nodes(), map[string]int{"busy": 3, "idle": 0}); got.ID != "idle" {
			t.Fatal("selected", got.ID)
		}
	}
	seen := map[string]bool{}
	for i := 0; i < 200 && len(seen) < 3; i++ {
		seen[leastLoaded(nodes(), nil).ID] = true
	}
	if len(seen) != 3 {
		t.Fatal("selection without load information is not spread", fmt.Sprint(seen))
	}
}
