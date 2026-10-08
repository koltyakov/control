package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gofrs/flock"
	"github.com/koltyakov/control/internal/gateway"
	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/node"
	"github.com/koltyakov/control/internal/store"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func fixedTunnelAddress(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := l.Addr().String()
	_ = l.Close()
	return address
}

func waitPersistent(t *testing.T, ctx context.Context, c Client, count int) []PersistentTunnelInfo {
	t.Helper()
	for {
		infos, err := c.PersistentTunnels(ctx)
		if err != nil {
			t.Fatal(err)
		}
		ready := len(infos) == count
		for _, info := range infos {
			ready = ready && info.State == "active"
		}
		if ready {
			return infos
		}
		select {
		case <-ctx.Done():
			t.Fatalf("tunnels did not become ready: %+v", infos)
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func checkTunnelEcho(t *testing.T, address string) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", address, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	// The first socket may need a new interactive lane and the five-second
	// direct-connect timeout before relay fallback. Binding alone is not readiness.
	_ = conn.SetDeadline(time.Now().Add(15 * time.Second))
	if _, err := conn.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	var reply [4]byte
	if _, err := io.ReadFull(conn, reply[:]); err != nil || string(reply[:]) != "ping" {
		t.Fatal("tunnel echo", reply, err)
	}
}

func TestPersistentTunnelsSurviveClientsServiceAndWorkerRestart(t *testing.T) {
	for _, relay := range []bool{false, true} {
		t.Run(fmt.Sprintf("relay=%v", relay), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 50*time.Second)
			defer cancel()
			const key = "persistent-tunnel-superuser-1234567890"
			g, err := gateway.New(t.TempDir(), "", gateway.Options{SuperuserKey: key})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = g.Close() }()
			gatewayServer := httptest.NewServer(g.Handler())
			defer gatewayServer.Close()
			admin := Admin{URL: gatewayServer.URL, Key: key}
			workerCfg := node.Config{Name: "worker", Gateway: gatewayServer.URL, Token: key, DataDir: t.TempDir(), WorkDir: t.TempDir(), RelayOnly: relay}
			worker, err := node.New(workerCfg)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = worker.Close() }()
			if err = worker.Start(ctx); err != nil {
				t.Fatal(err)
			}
			echo, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = echo.Close() }()
			go func() {
				for {
					conn, err := echo.Accept()
					if err != nil {
						return
					}
					go func() { defer func() { _ = conn.Close() }(); _, _ = io.Copy(conn, conn) }()
				}
			}()
			cfg := StandaloneConfig{Gateway: admin, StateDir: t.TempDir(), RelayOnly: relay, AccountRouting: true}
			root := t.TempDir()
			var serviceCancel context.CancelFunc
			var serviceDone chan error
			launch := func(_ context.Context, dir string) error {
				serviceCtx, stop := context.WithCancel(ctx)
				serviceCancel = stop
				serviceDone = make(chan error, 1)
				go func() { serviceDone <- RunTunnelService(serviceCtx, dir) }()
				return nil
			}
			defer func() {
				if serviceCancel != nil {
					serviceCancel()
					if err := <-serviceDone; err != nil {
						t.Error(err)
					}
				}
			}()
			newClient := func() Client { return (Client{}).WithStandalone(ctx, cfg).WithPersistentTunnels(root, launch) }
			c := newClient()
			defer func() { _ = c.Close() }()
			if empty, err := c.PersistentTunnels(ctx); err != nil || len(empty) != 0 || serviceCancel != nil {
				t.Fatal("listing empty state started a service", empty, err)
			}
			specs := []PersistentTunnelSpec{
				{ID: "forward", ForwardSpec: ForwardSpec{Node: "worker", Address: echo.Addr().String(), Listen: fixedTunnelAddress(t)}},
				{ID: "reverse", ForwardSpec: ForwardSpec{Node: "worker", Address: echo.Addr().String(), Listen: fixedTunnelAddress(t), Reverse: true}},
			}
			var accepted []PersistentTunnelInfo
			for _, spec := range specs {
				info, err := c.StartPersistentTunnel(ctx, spec)
				if err != nil {
					t.Fatal(err)
				}
				accepted = append(accepted, info)
				if !info.Expires.IsZero() || info.NodeID != worker.Identity.ID {
					t.Fatal("default expiry or unpinned target", info)
				}
			}
			waitPersistent(t, ctx, c, 2)
			_ = c.Close()
			monitor := newClient()
			defer func() { _ = monitor.Close() }()
			for _, spec := range specs {
				checkTunnelEcho(t, spec.Listen)
			}
			again, err := monitor.StartPersistentTunnel(ctx, specs[0])
			if err != nil || !again.Created.Equal(accepted[0].Created) {
				t.Fatal("idempotent start changed creation", again, err)
			}
			changed := specs[0]
			changed.Address = "127.0.0.1:1"
			if _, err := monitor.StartPersistentTunnel(ctx, changed); err == nil {
				t.Fatal("changed specification accepted")
			}
			pool, err := monitor.Dashboard(ctx, admin, model.PoolActivityQuery{})
			if err != nil || len(pool.Nodes) != 1 || pool.Nodes[0].RetainedTunnels == nil || *pool.Nodes[0].RetainedTunnels != (model.TunnelCounts{Forward: 1, Reverse: 1}) {
				t.Fatal("dashboard lost tunnel counts", pool, err)
			}
			// Stop the owner service. A different invocation restores the same IDs and ports.
			serviceCancel()
			if err := <-serviceDone; err != nil {
				t.Fatal(err)
			}
			serviceCancel = nil
			restored := waitPersistent(t, ctx, monitor, 2)
			if !restored[0].Created.Equal(accepted[0].Created) {
				t.Fatal("service restart replaced tunnel definition")
			}
			for _, spec := range specs {
				checkTunnelEcho(t, spec.Listen)
			}
			// A destination restart breaks the reverse multiplexer. Only listeners reconnect.
			_ = worker.Close()
			worker, err = node.New(workerCfg)
			if err != nil {
				t.Fatal(err)
			}
			if err = worker.Start(ctx); err != nil {
				t.Fatal(err)
			}
			// Wait for the old reverse forward to close and be replaced before probing.
			for {
				conn, err := net.DialTimeout("tcp", specs[1].Listen, 100*time.Millisecond)
				if err == nil {
					_ = conn.SetDeadline(time.Now().Add(500 * time.Millisecond))
					_, _ = conn.Write([]byte("ping"))
					var reply [4]byte
					_, err = io.ReadFull(conn, reply[:])
					_ = conn.Close()
					if err == nil && string(reply[:]) == "ping" {
						break
					}
				}
				select {
				case <-ctx.Done():
					t.Fatal("reverse tunnel did not restore after worker restart")
				case <-time.After(50 * time.Millisecond):
				}
			}
			checkTunnelEcho(t, specs[0].Listen)
			for _, spec := range specs {
				if err := monitor.DisposePersistentTunnel(ctx, spec.ID); err != nil {
					t.Fatal(err)
				}
			}
			if err := monitor.DisposePersistentTunnel(ctx, "forward"); err != nil {
				t.Fatal("dispose not idempotent", err)
			}
			waitPersistent(t, ctx, monitor, 0)
			serviceCancel()
			if err := <-serviceDone; err != nil {
				t.Fatal(err)
			}
			serviceCancel = nil
			waitPersistent(t, ctx, monitor, 0)
			for _, spec := range specs {
				// Reverse closure propagates to the worker after the local service
				// exits. Check eventual port release, not synchronous remote teardown.
				closedCtx, closedCancel := context.WithTimeout(ctx, 3*time.Second)
				defer closedCancel()
				for {
					conn, err := net.DialTimeout("tcp", spec.Listen, 100*time.Millisecond)
					if err != nil {
						break
					}
					_ = conn.Close()
					select {
					case <-closedCtx.Done():
						t.Fatalf("disposed listener %s remained bound after restart", spec.ID)
					case <-time.After(10 * time.Millisecond):
					}
				}
			}
		})
	}
}

func TestTunnelServiceStartupWaitsForPreviousOwner(t *testing.T) {
	for _, outcome := range []string{"stop", "cancel", "ready"} {
		t.Run(outcome, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			cfg := StandaloneConfig{Gateway: Admin{URL: "http://gateway.invalid", Key: "test-key"}, StateDir: t.TempDir(), AccountRouting: true}
			root := t.TempDir()
			dir := filepath.Join(root, tunnelScope(cfg))
			if err := store.Write(filepath.Join(dir, "service.json"), cfg); err != nil {
				t.Fatal(err)
			}
			ownerLock := flock.New(filepath.Join(dir, "service.lock"))
			defer func() { _ = ownerLock.Close() }()
			if err := ownerLock.Lock(); err != nil {
				t.Fatal(err)
			}
			h := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, `{}`) }))
			defer h.Close()
			publish := func() error {
				return store.Write(filepath.Join(dir, "endpoint.json"), tunnelEndpoint{Address: strings.TrimPrefix(h.URL, "http://"), Token: "local-token"})
			}
			launched := make(chan struct{}, 1)
			launch := func(_ context.Context, dir string) error {
				launched <- struct{}{}
				lock := flock.New(filepath.Join(dir, "service.lock"))
				defer func() { _ = lock.Close() }()
				held, err := lock.TryLock()
				if err != nil {
					return err
				}
				if !held {
					return fmt.Errorf("previous owner still holds the service lock")
				}
				return publish()
			}
			c := (Client{}).WithStandalone(ctx, cfg).WithPersistentTunnels(root, launch)
			defer func() { _ = c.Close() }()
			done := make(chan error, 1)
			go func() { done <- c.ensureTunnelService(ctx, dir, false) }()
			select {
			case <-launched:
				t.Fatal("launched before the previous owner released its lock")
			case <-time.After(100 * time.Millisecond):
			}
			switch outcome {
			case "cancel":
				cancel()
			case "ready":
				if err := publish(); err != nil {
					t.Fatal(err)
				}
			case "stop":
				if err := ownerLock.Unlock(); err != nil {
					t.Fatal(err)
				}
			}
			select {
			case err := <-done:
				if outcome == "cancel" {
					if !errors.Is(err, context.Canceled) {
						t.Fatal("startup did not respect cancellation", err)
					}
				} else if err != nil {
					t.Fatal(err)
				}
				if outcome != "stop" {
					select {
					case <-launched:
						t.Fatal("launched despite cancelled startup or a healthy owner")
					default:
					}
				}
			case <-time.After(5 * time.Second):
				t.Fatal("startup did not finish")
			}
		})
	}
}

func TestPersistentTTLValidationIsolationAndDurability(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	const nodeID = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/nodes" {
			_ = json.NewEncoder(w).Encode([]model.Node{{ID: nodeID, Name: "worker", Online: true, ClientOwners: true, ClientSessions: true, InstructionDelegation: true}})
			return
		}
		http.Error(w, "offline", http.StatusServiceUnavailable)
	}))
	defer server.Close()
	dir := t.TempDir()
	cfg := StandaloneConfig{Gateway: Admin{URL: server.URL, Key: "test-key"}, StateDir: t.TempDir(), AccountRouting: true}
	m, err := newTunnelService(ctx, dir, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer m.close()
	spec := PersistentTunnelSpec{ID: "ttl", TTLSeconds: 1, ForwardSpec: ForwardSpec{Node: "worker", Address: "127.0.0.1:80", Listen: fixedTunnelAddress(t)}}
	info, err := m.create(ctx, spec)
	if err != nil || info.Expires.Sub(info.Created) != time.Second {
		t.Fatal(info, err)
	}
	again, err := m.create(ctx, spec)
	if err != nil || !again.Expires.Equal(info.Expires) {
		t.Fatal("TTL renewed on idempotent start", again, err)
	}
	for _, bad := range []PersistentTunnelSpec{
		{ID: "../bad", ForwardSpec: spec.ForwardSpec},
		{ID: "zero", ForwardSpec: ForwardSpec{Node: "worker", Address: "127.0.0.1:80", Listen: "127.0.0.1:0"}},
		{ID: "bad-ttl", TTLSeconds: -1, ForwardSpec: spec.ForwardSpec},
	} {
		if _, err := m.create(ctx, bad); err == nil {
			t.Fatal("invalid specification accepted", bad)
		}
	}
	h := httptest.NewServer(m.handler("local-token"))
	defer h.Close()
	resp, err := http.Get(h.URL + "/tunnels")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatal("unauthenticated tunnel management", resp.StatusCode)
	}
	if tunnelScope(cfg) == tunnelScope(StandaloneConfig{Gateway: Admin{URL: cfg.Gateway.URL, Key: "another-account"}}) {
		t.Fatal("login scopes collide")
	}
	for len(m.list()) != 0 {
		select {
		case <-ctx.Done():
			t.Fatal("TTL did not expire")
		case <-time.After(20 * time.Millisecond):
		}
	}
	m.close()
	// Persisted expiry remains effective even if shutdown raced the expiry sweep.
	restored, err := newTunnelService(ctx, dir, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.close()
	if len(restored.list()) != 0 {
		t.Fatal("expired tunnel restored")
	}
	if err := restored.dispose("ttl"); err != nil {
		t.Fatal(err)
	}
	// An expiry sweep holding an old entry cannot dispose a newly reused ID.
	spec.ID, spec.TTLSeconds = "replacement", 0
	if _, err := restored.create(ctx, spec); err != nil {
		t.Fatal(err)
	}
	obsolete := restored.entry(info)
	defer obsolete.cancel()
	if err := restored.disposeEntry(spec.ID, obsolete); err != nil || len(restored.list()) != 1 {
		t.Fatal("stale expiry disposed replacement", err)
	}
	if err := restored.dispose(spec.ID); err != nil {
		t.Fatal(err)
	}
	// A failed durable write must not publish a listener definition.
	blocked := filepath.Join(t.TempDir(), "not-a-directory")
	if err = os.WriteFile(blocked, nil, 0600); err != nil {
		t.Fatal(err)
	}
	restored.mu.Lock()
	restored.dir = blocked
	restored.mu.Unlock()
	spec.ID, spec.TTLSeconds = "write-fails", 0
	if _, err := restored.create(ctx, spec); err == nil || len(restored.list()) != 0 {
		t.Fatal("failed commit published a tunnel", err)
	}
}

func TestMCPPersistentTunnelManagement(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cfg := StandaloneConfig{Gateway: Admin{URL: "http://gateway.invalid", Key: "secret-account-key"}, StateDir: t.TempDir(), AccountRouting: true}
	root := t.TempDir()
	dir := filepath.Join(root, tunnelScope(cfg))
	if err := store.Write(filepath.Join(dir, "service.json"), cfg); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var calls []string
	h := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer local-token" {
			t.Error("missing local authentication")
		}
		mu.Lock()
		calls = append(calls, r.Method+" "+r.URL.Path)
		mu.Unlock()
		switch r.URL.Path {
		case "/health":
			_, _ = io.WriteString(w, `{}`)
		case "/tunnels":
			if r.Method == http.MethodGet {
				_, _ = io.WriteString(w, `[]`)
			} else {
				var spec PersistentTunnelSpec
				_ = json.NewDecoder(r.Body).Decode(&spec)
				_ = json.NewEncoder(w).Encode(PersistentTunnelInfo{PersistentTunnelSpec: spec, State: "starting"})
			}
		default:
			_, _ = io.WriteString(w, `{"disposed":true}`)
		}
	}))
	defer h.Close()
	if err := store.Write(filepath.Join(dir, "endpoint.json"), tunnelEndpoint{Address: strings.TrimPrefix(h.URL, "http://"), Token: "local-token"}); err != nil {
		t.Fatal(err)
	}
	c := (Client{}).WithStandalone(ctx, cfg).WithPersistentTunnels(root, nil)
	defer func() { _ = c.Close() }()
	st, ct := mcp.NewInMemoryTransports()
	ss, err := c.MCPServer().Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ss.Close() }()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cs.Close() }()
	for name, args := range map[string]any{
		"control_tunnel_start":   map[string]any{"id": "qa", "node": "worker", "address": "127.0.0.1:5173", "listen": "127.0.0.1:15173", "reverse": true},
		"control_tunnel_list":    map[string]any{},
		"control_tunnel_dispose": map[string]any{"id": "qa"},
	} {
		result, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
		if err != nil || result.IsError {
			t.Fatal(name, result, err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	for _, expected := range []string{"POST /tunnels", "GET /tunnels", "DELETE /tunnels/qa"} {
		if !strings.Contains(strings.Join(calls, "\n"), expected) {
			t.Fatal("MCP did not route", expected, calls)
		}
	}
}
