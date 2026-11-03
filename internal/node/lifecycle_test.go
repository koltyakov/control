package node

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/koltyakov/control/internal/buildinfo"
	"github.com/koltyakov/control/internal/gateway"
	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/store"
)

func TestMachineStateCoalescesHintsWithoutLosingConcurrentChanges(t *testing.T) {
	t.Parallel()
	const adminKey = "machine-state-hint-superuser-123456789"
	g, err := gateway.New(t.TempDir(), testToken, gateway.Options{SuperuserKey: adminKey})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = g.Close() }()
	handler := g.Handler()
	var reads atomic.Int32
	var blockNext atomic.Bool
	reading, release := make(chan struct{}), make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/node/state" {
			reads.Add(1)
			if blockNext.CompareAndSwap(true, false) {
				// Capture an old response before a second policy change commits.
				recorder := httptest.NewRecorder()
				handler.ServeHTTP(recorder, r)
				close(reading)
				select {
				case <-release:
				case <-r.Context().Done():
					return
				}
				w.WriteHeader(recorder.Code)
				_, _ = w.Write(recorder.Body.Bytes())
				return
			}
		}
		handler.ServeHTTP(w, r)
	}))
	defer server.Close()
	n, err := New(Config{Name: "hint-worker", Gateway: server.URL, Token: testToken, DataDir: t.TempDir(), WorkDir: t.TempDir(), MetricsIntervalSeconds: -1})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = n.Close() }()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	n.ctx, n.cancel = context.WithCancel(ctx)
	if err := n.Peer.Start(n.ctx); err != nil {
		t.Fatal(err)
	}
	eventually(t, ctx, func() bool { return len(n.Peer.MachineStateWake()) == 1 })
	if err := n.syncMachineState(ctx); err != nil {
		t.Fatal(err)
	}
	if len(n.Peer.MachineStateWake()) != 0 {
		t.Fatal("signed read retained an already-covered registration hint")
	}
	n.wg.Add(1)
	go func() { defer n.wg.Done(); n.runMachineState(n.ctx) }()
	// A post-connect read must not be repeated when the background loop starts.
	time.Sleep(100 * time.Millisecond)
	if got := reads.Load(); got != 1 {
		t.Fatalf("startup repeated the policy read: got %d requests, want 1", got)
	}
	setDisabled := func(disabled bool) {
		t.Helper()
		req, err := http.NewRequestWithContext(ctx, http.MethodPatch, server.URL+"/v1/fleet/nodes/"+n.Identity.ID, bytes.NewReader(model.JSON(map[string]bool{"disabled": disabled})))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+adminKey)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatal("policy change failed", resp.StatusCode)
		}
	}
	blockNext.Store(true)
	setDisabled(true)
	select {
	case <-reading:
	case <-ctx.Done():
		t.Fatal("policy hint did not trigger a read", ctx.Err())
	}
	setDisabled(false)
	eventually(t, ctx, func() bool { return len(n.Peer.MachineStateWake()) == 1 })
	close(release)
	eventually(t, ctx, func() bool {
		state := n.currentMachineState()
		return state.Revision == 2 && !state.Disabled
	})
}

func TestMachineStateStartupAndLegacyFallbackReadCounts(t *testing.T) {
	t.Parallel()
	g, err := gateway.New(t.TempDir(), testToken)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = g.Close() }()
	handler := g.Handler()
	var reads atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/node/state":
			reads.Add(1)
		case "/v1/auth":
			// Emulate a gateway without pushed policy hints.
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, r)
			var auth map[string]any
			if err := json.Unmarshal(recorder.Body.Bytes(), &auth); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			delete(auth, "machineStatePush")
			_ = json.NewEncoder(w).Encode(auth)
			return
		}
		handler.ServeHTTP(w, r)
	}))
	defer server.Close()
	n, err := New(Config{Name: "legacy-policy", Gateway: server.URL, Token: testToken, DataDir: t.TempDir(), WorkDir: t.TempDir(), MetricsIntervalSeconds: -1})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = n.Close() }()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if err := n.Start(ctx); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	if got := reads.Load(); got != 2 {
		t.Fatalf("startup made %d policy reads, want pre-connect and post-connect only", got)
	}
	if n.Peer.MachineStatePushed() {
		t.Fatal("legacy gateway negotiated policy hints")
	}
	eventually(t, ctx, func() bool { return reads.Load() == 3 })
}

func TestStartupReadsRetirementAfterConnectionRejection(t *testing.T) {
	t.Parallel()
	for _, retired := range []bool{false, true} {
		t.Run(fmt.Sprint("retired=", retired), func(t *testing.T) {
			t.Parallel()
			var rejected atomic.Bool
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/v1/node/state":
					state := model.MachineState{}
					if retired && rejected.Load() {
						state = model.MachineState{Revision: 1, Disabled: true, Unregistered: true}
					}
					_ = json.NewEncoder(w).Encode(state)
				case "/v1/auth":
					// Retirement can revoke the credential after the initial policy
					// read, including while startup waits through a gateway outage.
					rejected.Store(true)
					http.Error(w, "credential revoked", http.StatusUnauthorized)
				default:
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			n, err := New(Config{Name: "startup-retirement", Gateway: server.URL, Token: testToken, DataDir: t.TempDir(), WorkDir: t.TempDir(), MetricsIntervalSeconds: -1})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = n.Close() }()
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			err = n.Start(ctx)
			if !retired {
				if err == nil || !strings.Contains(err.Error(), "401") || n.Unregistered() {
					t.Fatal("ordinary authentication rejection changed retirement state", err)
				}
				return
			}
			if !errors.Is(err, ErrUnregistered) || !n.Unregistered() {
				t.Fatal("connection rejection hid retirement policy", err)
			}
			var state model.MachineState
			if err := store.Read(filepath.Join(n.Config.DataDir, "machine-state.json"), &state); err != nil || !state.Unregistered {
				t.Fatal("retirement was not persisted for uninstall", state, err)
			}
			if err := n.Close(); err != nil {
				t.Fatal(err)
			}
			server.Close()
			restarted, err := New(n.Config)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = restarted.Close() }()
			if err := restarted.Start(ctx); !errors.Is(err, ErrUnregistered) {
				t.Fatal("persisted retirement required gateway access", err)
			}
		})
	}
}

func TestMachineLifecycleBlocksSchedulingAndStopsNode(t *testing.T) {
	t.Parallel()
	for _, relay := range []bool{false, true} {
		t.Run(fmt.Sprint("relay=", relay), func(t *testing.T) {
			t.Parallel()
			const adminKey = "machine-lifecycle-superuser-123456789"
			g, err := gateway.New(t.TempDir(), testToken, gateway.Options{SuperuserKey: adminKey})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = g.Close() }()
			server := httptest.NewServer(g.Handler())
			defer server.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
			defer cancel()
			request := func(method, path string, body, out any) int {
				t.Helper()
				req, err := http.NewRequestWithContext(ctx, method, server.URL+path, bytes.NewReader(model.JSON(body)))
				if err != nil {
					t.Fatal(err)
				}
				req.Header.Set("Authorization", "Bearer "+adminKey)
				resp, err := http.DefaultClient.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = resp.Body.Close() }()
				if out != nil && resp.StatusCode < 300 {
					if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
						t.Fatal(err)
					}
				}
				return resp.StatusCode
			}
			newNode := func(name string) *Node {
				n, err := New(Config{Name: name, Gateway: server.URL, Token: testToken, DataDir: t.TempDir(), WorkDir: t.TempDir(), RelayOnly: relay, MetricsIntervalSeconds: -1, Labels: map[string]string{"role": name}})
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = n.Close() })
				n.Peer.SetSoftware(buildinfo.Info{Version: "v-node-test", OS: runtime.GOOS, Arch: runtime.GOARCH})
				return n
			}
			source, worker := newNode("source"), newNode("worker")
			attachTestOrchestrator(t, source, prepareTestOrchestrator(t, ctx, server.URL, adminKey, "legacy", relay))
			started, release := make(chan struct{}, 1), make(chan struct{})
			if err := worker.Register(provider{cap: model.Capability{Name: "probe", InputSchema: model.JSON(map[string]any{"type": "object"})}, run: func(ctx context.Context, _ json.RawMessage, _ Execution) (any, error) {
				select {
				case started <- struct{}{}:
				default:
				}
				select {
				case <-release:
					return "done", nil
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			}}); err != nil {
				t.Fatal(err)
			}
			stopped := make(chan struct{})
			worker.SetShutdown(func() { close(stopped) })
			for _, n := range []*Node{source, worker} {
				if err := n.Start(ctx); err != nil {
					t.Fatal(err)
				}
			}
			var directory []model.Node
			eventually(t, ctx, func() bool {
				request("GET", "/v1/nodes", nil, &directory)
				for _, n := range directory {
					if n.ID == worker.Identity.ID {
						return n.Managed
					}
				}
				return false
			})
			spec := model.TaskSpec{ID: "durable-probe", Capability: "probe", Args: model.JSON(map[string]any{})}
			var task model.Task
			if err := testCall(t, source, ctx, "worker", "tasks.start", spec, &task); err != nil {
				t.Fatal(err)
			}
			select {
			case <-started:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			path := "/v1/fleet/nodes/" + worker.Identity.ID
			if code := request("PATCH", path, map[string]bool{"disabled": true}, nil); code != 200 {
				t.Fatal("disable", code)
			}
			// Well inside the 30-second pushed poll, so this proves the hint arrived.
			pushed, cancelPushed := context.WithTimeout(ctx, 10*time.Second)
			eventually(t, pushed, func() bool { return worker.currentMachineState().Disabled })
			cancelPushed()
			if !worker.Peer.MachineStatePushed() {
				t.Fatal("worker did not negotiate pushed machine state")
			}
			if err := testCall(t, source, ctx, "", "nodes.select", map[string]any{"labels": map[string]string{"role": "worker"}}, nil); err == nil {
				t.Fatal("disabled worker selected")
			}
			if err := testCall(t, source, ctx, "worker", "probe", map[string]any{}, nil); err == nil || !strings.Contains(err.Error(), "disabled") {
				t.Fatal("cached peer session bypassed disable", err)
			}
			if err := testCall(t, source, ctx, "worker", "tasks.start", spec, &task); err != nil || task.Terminal() {
				t.Fatal("disabled machine lost accepted/idempotent task", err)
			}
			spec.ID = "new-probe"
			if err := testCall(t, source, ctx, "worker", "tasks.start", spec, nil); err == nil {
				t.Fatal("disabled worker accepted another task")
			}
			close(release)
			if completed, err := testWaitTask(t, source, ctx, "worker", "durable-probe"); err != nil || completed.State != "succeeded" {
				t.Fatal("disable interrupted accepted work", err)
			}
			if code := request("PATCH", path, map[string]bool{"disabled": false}, nil); code != 200 {
				t.Fatal("enable", code)
			}
			eventually(t, ctx, func() bool {
				var chosen model.Node
				return testCall(t, source, ctx, "", "nodes.select", map[string]any{"labels": map[string]string{"role": "worker"}}, &chosen) == nil && chosen.ID == worker.Identity.ID
			})
			if err := testCall(t, source, ctx, "worker", "probe", map[string]any{}, nil); err != nil {
				t.Fatal("enabled worker rejected work", err)
			}
			if code := request("DELETE", path+"?stop=true", nil, nil); code != 204 {
				t.Fatal("unregister", code)
			}
			select {
			case <-stopped:
			case <-ctx.Done():
				t.Fatal("unregister did not stop node")
			}
			if err := worker.Close(); err != nil {
				t.Fatal(err)
			}
			request("GET", "/v1/nodes", nil, &directory)
			for _, n := range directory {
				if n.ID == worker.Identity.ID {
					t.Fatal("disconnect resurrected removed registration")
				}
			}
			restarted, err := New(worker.Config)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = restarted.Close() }()
			if err := restarted.Start(ctx); !errors.Is(err, ErrUnregistered) {
				t.Fatal("unregistered node restarted", err)
			}
		})
	}
}
