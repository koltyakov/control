package node

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/koltyakov/control/internal/gateway"
	"github.com/koltyakov/control/internal/model"
)

func TestNodeStartupRecoversGatewayFailure(t *testing.T) {
	for _, path := range []string{"/v1/node/state", "/v1/auth", "/v1/connect"} {
		t.Run(path, func(t *testing.T) {
			g, err := gateway.New(t.TempDir(), testToken)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = g.Close() }()
			handler := g.Handler()
			var unavailable atomic.Bool
			unavailable.Store(true)
			var failures atomic.Int32
			failed := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == path && unavailable.Load() {
					if failures.Add(1) == 1 {
						close(failed)
					}
					http.Error(w, "temporarily unavailable", http.StatusServiceUnavailable)
					return
				}
				handler.ServeHTTP(w, r)
			}))
			defer server.Close()
			n, err := New(Config{Name: "worker", Gateway: server.URL, Token: testToken, DataDir: t.TempDir(), WorkDir: t.TempDir(), RelayOnly: true, MetricsIntervalSeconds: -1})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = n.Close() }()
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- n.Start(ctx) }()
			select {
			case <-failed:
			case <-ctx.Done():
				t.Fatal("startup did not reach the unavailable endpoint")
			}
			select {
			case err := <-done:
				t.Fatalf("startup exited instead of waiting for the gateway: %v", err)
			case <-time.After(100 * time.Millisecond):
			}
			unavailable.Store(false)
			select {
			case err := <-done:
				if err != nil {
					t.Fatal("startup did not recover", err)
				}
			case <-ctx.Done():
				t.Fatal("startup did not recover without a manual restart")
			}
			directory, err := n.Peer.Nodes(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if len(directory) != 1 || directory[0].ID != n.Identity.ID || !directory[0].Online {
				t.Fatalf("recovered node was not registered online: %+v", directory)
			}
		})
	}
}

func TestNodeStartupPolicyFailure(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		local  bool
	}{
		{"unauthorized", http.StatusUnauthorized, "invalid proof", false},
		{"forbidden", http.StatusForbidden, "forbidden", false},
		{"invalid JSON", http.StatusOK, "not JSON", false},
		{"persistence failure", http.StatusOK, `{"revision":1,"disabled":true}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var queries, connects atomic.Int32
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/node/state" {
					connects.Add(1)
				}
				queries.Add(1)
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer s.Close()
			n, err := New(Config{Name: "worker", Gateway: s.URL, Token: testToken, DataDir: t.TempDir(), WorkDir: t.TempDir(), MetricsIntervalSeconds: -1})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = n.Close() }()
			if tc.local {
				if err := os.Mkdir(filepath.Join(n.Config.DataDir, "machine-state.json"), 0700); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			if err := n.Start(ctx); err == nil || errors.Is(err, context.DeadlineExceeded) || queries.Load() != 1 || connects.Load() != 0 {
				t.Fatalf("permanent policy error did not fail closed: queries=%d, connects=%d, error=%v", queries.Load(), connects.Load(), err)
			}
		})
	}
}

func TestNodeStartupAppliesPolicyAfterOutage(t *testing.T) {
	for _, retired := range []bool{false, true} {
		t.Run(map[bool]string{false: "disabled", true: "unregistered"}[retired], func(t *testing.T) {
			g, err := gateway.New(t.TempDir(), testToken)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = g.Close() }()
			handler := g.Handler()
			var queries, connects atomic.Int32
			state := model.MachineState{Revision: 1, Disabled: true, Unregistered: retired}
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v1/node/state" {
					if queries.Add(1) == 1 {
						http.Error(w, "temporarily unavailable", http.StatusServiceUnavailable)
						return
					}
					_ = json.NewEncoder(w).Encode(state)
					return
				}
				connects.Add(1)
				handler.ServeHTTP(w, r)
			}))
			defer s.Close()
			n, err := New(Config{Name: "worker", Gateway: s.URL, Token: testToken, DataDir: t.TempDir(), WorkDir: t.TempDir(), RelayOnly: true, MetricsIntervalSeconds: -1})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = n.Close() }()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			err = n.Start(ctx)
			if retired {
				if !errors.Is(err, ErrUnregistered) || connects.Load() != 0 {
					t.Fatalf("retired node tried to register: connects=%d, error=%v", connects.Load(), err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if n.currentMachineState() != state {
				t.Fatal("startup did not apply recovered policy")
			}
			if release, err := n.work.Enter(ctx); err == nil || !strings.Contains(err.Error(), "disabled") {
				if release != nil {
					release()
				}
				t.Fatal("recovered policy allowed execution", err)
			}
			var saved model.MachineState
			data, err := os.ReadFile(filepath.Join(n.Config.DataDir, "machine-state.json"))
			if err != nil || json.Unmarshal(data, &saved) != nil || saved != state {
				t.Fatalf("recovered policy was not durable: %+v, %v", saved, err)
			}
		})
	}
}

func TestNodeStartupRecoversPolicyRequestTimeout(t *testing.T) {
	g, err := gateway.New(t.TempDir(), testToken)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = g.Close() }()
	handler := g.Handler()
	var queries atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/node/state" && queries.Add(1) == 1 {
			// A real per-request timeout must not cancel the node's lifetime.
			_, _ = io.Copy(io.Discard, r.Body)
			<-r.Context().Done()
			return
		}
		handler.ServeHTTP(w, r)
	}))
	defer s.Close()
	n, err := New(Config{Name: "worker", Gateway: s.URL, Token: testToken, DataDir: t.TempDir(), WorkDir: t.TempDir(), RelayOnly: true, MetricsIntervalSeconds: -1})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = n.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := n.Start(ctx); err != nil || queries.Load() < 2 || n.ctx.Err() != nil {
		t.Fatalf("request timeout prevented recovery: queries=%d, error=%v", queries.Load(), err)
	}
}

func TestNodeStartupCancellationWhileGatewayUnavailable(t *testing.T) {
	for _, path := range []string{"/v1/node/state", "/v1/auth"} {
		t.Run(path, func(t *testing.T) {
			g, err := gateway.New(t.TempDir(), testToken)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = g.Close() }()
			handler := g.Handler()
			failed := make(chan struct{}, 1)
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == path {
					select {
					case failed <- struct{}{}:
					default:
					}
					http.Error(w, "temporarily unavailable", http.StatusServiceUnavailable)
					return
				}
				handler.ServeHTTP(w, r)
			}))
			defer s.Close()
			n, err := New(Config{Name: "worker", Gateway: s.URL, Token: testToken, DataDir: t.TempDir(), WorkDir: t.TempDir(), RelayOnly: true, MetricsIntervalSeconds: -1})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = n.Close() }()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- n.Start(ctx) }()
			select {
			case <-failed:
			case <-ctx.Done():
				t.Fatal("startup did not reach the unavailable endpoint")
			}
			cancel()
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) {
					t.Fatal("startup did not propagate cancellation", err)
				}
			case <-time.After(time.Second):
				t.Fatal("startup retry did not stop promptly")
			}
		})
	}
}

func TestNodeStartupRechecksPolicyAfterConnectionWait(t *testing.T) {
	g, err := gateway.New(t.TempDir(), testToken)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = g.Close() }()
	handler := g.Handler()
	var connects, queries atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/auth" && connects.Add(1) == 1 {
			http.Error(w, "temporarily unavailable", http.StatusServiceUnavailable)
			return
		}
		if r.URL.Path == "/v1/node/state" {
			state := model.MachineState{}
			if queries.Add(1) > 1 {
				state.Revision, state.Disabled = 1, true
			}
			_ = json.NewEncoder(w).Encode(state)
			return
		}
		handler.ServeHTTP(w, r)
	}))
	defer s.Close()
	n, err := New(Config{Name: "worker", Gateway: s.URL, Token: testToken, DataDir: t.TempDir(), WorkDir: t.TempDir(), RelayOnly: true, MetricsIntervalSeconds: -1})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = n.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := n.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if !n.currentMachineState().Disabled {
		t.Fatal("startup retained the policy fetched before the connection outage")
	}
	if release, err := n.work.Enter(ctx); err == nil {
		release()
		t.Fatal("startup admitted work before refreshing policy")
	}
}

func TestNodeStartupBlocksWorkDuringPolicyRefresh(t *testing.T) {
	g, err := gateway.New(t.TempDir(), testToken)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = g.Close() }()
	handler := g.Handler()
	var unavailable atomic.Bool
	unavailable.Store(true)
	var queries atomic.Int32
	failed := make(chan struct{}, 1)
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/node/state" {
			if queries.Add(1) > 1 && unavailable.Load() {
				select {
				case failed <- struct{}{}:
				default:
				}
				http.Error(w, "temporarily unavailable", http.StatusServiceUnavailable)
				return
			}
			_ = json.NewEncoder(w).Encode(model.MachineState{})
			return
		}
		handler.ServeHTTP(w, r)
	}))
	defer s.Close()
	n, err := New(Config{Name: "worker", Gateway: s.URL, Token: testToken, DataDir: t.TempDir(), WorkDir: t.TempDir(), RelayOnly: true, MetricsIntervalSeconds: -1})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = n.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- n.Start(ctx) }()
	select {
	case <-failed:
	case <-ctx.Done():
		t.Fatal("startup did not reach the post-connection policy check")
	}
	if release, err := n.work.Enter(ctx); err == nil {
		release()
		t.Fatal("connected peer admitted work before startup policy validation")
	}
	unavailable.Store(false)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("startup did not finish after policy recovery")
	}
	release, err := n.work.Enter(ctx)
	if err != nil {
		t.Fatal("recovered enabled node retained its startup admission block", err)
	}
	release()
}
