package transport

import (
	"context"
	"net"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/koltyakov/control/internal/gateway"
	"github.com/koltyakov/control/internal/identity"
	"github.com/koltyakov/control/internal/model"
)

// freezingProxy forwards TCP connections until freeze, which silently stops
// existing connections without closing them, like an abandoned network path.
// Connections accepted afterwards forward normally.
type freezingProxy struct {
	listener net.Listener
	target   string
	mu       sync.Mutex
	frozen   chan struct{}
}

func newFreezingProxy(t *testing.T, target string) *freezingProxy {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &freezingProxy{listener: listener, target: target, frozen: make(chan struct{})}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go p.forward(t, conn)
		}
	}()
	return p
}

func (p *freezingProxy) forward(t *testing.T, client net.Conn) {
	upstream, err := net.Dial("tcp", p.target)
	if err != nil {
		_ = client.Close()
		return
	}
	p.mu.Lock()
	frozen := p.frozen
	p.mu.Unlock()
	copyUntilFrozen := func(dst, src net.Conn) {
		buf := make([]byte, 32<<10)
		for {
			n, err := src.Read(buf)
			select {
			case <-frozen:
				// Hold both sockets open without delivering anything.
				<-t.Context().Done()
				return
			default:
			}
			if n > 0 {
				if _, werr := dst.Write(buf[:n]); werr != nil {
					return
				}
			}
			if err != nil {
				_ = dst.Close()
				return
			}
		}
	}
	go copyUntilFrozen(upstream, client)
	copyUntilFrozen(client, upstream)
}

// freeze abandons current connections; later connections use a fresh path.
func (p *freezingProxy) freeze() {
	p.mu.Lock()
	defer p.mu.Unlock()
	close(p.frozen)
	p.frozen = make(chan struct{})
}

func TestReconnectAfterAbandonedGatewayPathProbesStaleRegistration(t *testing.T) {
	t.Parallel()
	const token = "stale-path-test-token-123456"
	g, err := gateway.New(t.TempDir(), token)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = g.Close() })
	server := httptest.NewServer(g.Handler())
	t.Cleanup(server.Close)
	proxy := newFreezingProxy(t, server.Listener.Addr().String())
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	id, err := identity.Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	p := New(Config{Gateway: "http://" + proxy.listener.Addr().String(), Token: token, Identity: id, Node: model.Node{ID: id.ID, Name: "roamer", PublicKey: id.Public}}, func(string, net.Conn) {})
	p.networkSnapshot = func() (string, error) { return "unchanged", nil }
	if err := p.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close() })
	<-p.GatewayConnected()

	// The old socket can no longer deliver a close frame to the gateway, which
	// still holds the identity's registration when the node reconnects.
	proxy.freeze()
	started := time.Now()
	p.resetNetwork()
	for p.Stats().GatewayReconnects == 0 {
		if ctx.Err() != nil {
			t.Fatal("node did not reconnect after abandoning its gateway path")
		}
		time.Sleep(50 * time.Millisecond)
	}
	// Without an early probe the stale registration persists for up to one
	// heartbeat interval plus its timeout, 15 seconds.
	if elapsed := time.Since(started); elapsed > 8*time.Second {
		t.Fatalf("reconnect took %v; stale registration was not probed promptly", elapsed)
	} else {
		t.Logf("reconnected after %v", elapsed)
	}
}

// A path that silently stops delivering, without any local route change, is
// detected from heartbeat silence and the node reconnects on its own.
func TestHeartbeatSilenceClosesFrozenGatewayPath(t *testing.T) {
	t.Parallel()
	const token = "frozen-path-test-token-123456"
	g, err := gateway.New(t.TempDir(), token)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = g.Close() })
	server := httptest.NewServer(g.Handler())
	t.Cleanup(server.Close)
	proxy := newFreezingProxy(t, server.Listener.Addr().String())
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	id, err := identity.Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	p := New(Config{Gateway: "http://" + proxy.listener.Addr().String(), Token: token, Identity: id, Node: model.Node{ID: id.ID, Name: "frozen", PublicKey: id.Public}}, func(string, net.Conn) {})
	p.networkSnapshot = func() (string, error) { return "unchanged", nil }
	// Slightly more than one heartbeat interval keeps the test short.
	p.readIdle = 6 * time.Second
	if err := p.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close() })
	<-p.GatewayConnected()
	if !p.gatewayHeartbeat.Load() {
		t.Fatal("gateway did not negotiate heartbeats")
	}
	proxy.freeze()
	for p.Stats().GatewayReconnects == 0 {
		if ctx.Err() != nil {
			t.Fatal("node never noticed the frozen gateway path")
		}
		time.Sleep(50 * time.Millisecond)
	}
	if p.Stats().GatewayIdleCloses == 0 {
		t.Fatal("reconnect was not caused by the idle watchdog")
	}
}
