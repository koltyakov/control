package transport

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/koltyakov/control/internal/gateway"
	"github.com/koltyakov/control/internal/identity"
	"github.com/koltyakov/control/internal/model"
)

func TestGatewayRestartAndDirectSessionContinuity(t *testing.T) {
	for _, relay := range []bool{true, false} {
		t.Run(fmt.Sprintf("relay=%v", relay), func(t *testing.T) {
			const token = "reconnect-test-token-123456"
			dir := t.TempDir()
			g, err := gateway.New(dir, token)
			if err != nil {
				t.Fatal(err)
			}
			server := httptest.NewServer(g.Handler())
			t.Cleanup(server.Close)
			t.Cleanup(func() { _ = g.Close() })
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			makePeer := func(name string) *Peer {
				id, err := identity.Load(t.TempDir())
				if err != nil {
					t.Fatal(err)
				}
				p := New(Config{Gateway: server.URL, Token: token, Identity: id, Node: model.Node{ID: id.ID, Name: name, PublicKey: id.Public}, RelayOnly: relay}, func(remote string, conn net.Conn) { _, _ = io.Copy(conn, conn) })
				if err = p.Start(ctx); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = p.Close() })
				return p
			}
			a, _ := makePeer("alpha"), makePeer("beta")
			echo := func() {
				stream, err := a.Open(ctx, "beta")
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = stream.Close() }()
				_ = stream.SetDeadline(time.Now().Add(3 * time.Second))
				if _, err = stream.Write([]byte("ping")); err != nil {
					t.Fatal(err)
				}
				var b [4]byte
				if _, err = io.ReadFull(stream, b[:]); err != nil || string(b[:]) != "ping" {
					t.Fatalf("echo: %q %v", b, err)
				}
			}
			echo()
			if err = g.Close(); err != nil {
				t.Fatal(err)
			}
			server.Close()
			if !relay {
				echo()
			} // Cached direct sessions do not consult the gateway.
			restarted, err := gateway.New(dir, token)
			if err != nil {
				t.Fatal(err)
			}
			listener, err := net.Listen("tcp", strings.TrimPrefix(server.URL, "http://"))
			if err != nil {
				t.Fatal(err)
			}
			second := httptest.NewUnstartedServer(restarted.Handler())
			_ = second.Listener.Close()
			second.Listener = listener
			second.Start()
			t.Cleanup(second.Close)
			t.Cleanup(func() { _ = restarted.Close() })
			ticker := time.NewTicker(25 * time.Millisecond)
			defer ticker.Stop()
			for {
				nodes, err := a.Nodes(ctx)
				if err == nil && len(nodes) == 2 && nodes[0].Online && nodes[1].Online {
					break
				}
				select {
				case <-ctx.Done():
					t.Fatal("peers did not rejoin", ctx.Err())
				case <-ticker.C:
				}
			}
			echo()
		})
	}
}
