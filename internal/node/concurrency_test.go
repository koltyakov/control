package node

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/koltyakov/control/internal/model"
)

func assertPool(t *testing.T, got model.ConcurrencyPool, limit, inUse, queued int) {
	t.Helper()
	if got.Limit != limit || got.InUse != inUse || got.Queued == nil || *got.Queued != queued {
		t.Fatalf("pool = %+v, queued=%v; want limit=%d inUse=%d queued=%d", got, got.Queued, limit, inUse, queued)
	}
}

func TestProviderPools(t *testing.T) {
	n := localNode(t, func(cfg *Config) {
		cfg.RPA = rpaCommand(t, "success")
		cfg.Desktop = &DesktopConfig{Command: Command{Command: "unused-desktop-helper"}}
	})
	report := n.concurrencySnapshot()
	assertPool(t, report.Execution, 4, 0, 0)
	assertPool(t, report.Operations, 8, 0, 0)
	if report.Desktop == nil {
		t.Fatal("missing desktop pool")
	}
	assertPool(t, *report.Desktop, 1, 0, 0)
	for _, method := range []string{"files.list", "files.read", "files.write", "http.request", "network.wake"} {
		slots, phase := n.providerSlots(method)
		if slots != n.operationsPool || phase != "waiting for operation slot" {
			t.Fatal("operation uses execution capacity", method)
		}
	}
	for _, method := range []string{"rpa.run", "desktop.run"} {
		pool, phase := n.providerSlots(method)
		if pool != n.desktopPool || phase != "waiting for desktop slot" {
			t.Fatal("desktop operation uses general I/O capacity", method)
		}
	}
	for _, method := range []string{"exec.run", "agent.run", "mcp.call", "mcp.request", "custom.provider"} {
		if slots, _ := n.providerSlots(method); slots != n.executionPool {
			t.Fatal("unrestricted provider bypasses execution bound", method)
		}
	}
	for _, method := range []string{"workflow.run", "peers.call"} {
		if slots, _ := n.providerSlots(method); slots != nil {
			t.Fatal("coordination occupies a child slot", method)
		}
	}
	custom := localNode(t, func(cfg *Config) {
		cfg.Providers = map[string]Command{
			"rpa.run":     {Command: "unused-custom-helper"},
			"desktop.run": {Command: "unused-custom-helper"},
		}
	})
	for _, method := range []string{"rpa.run", "desktop.run"} {
		if slots, _ := custom.providerSlots(method); slots != custom.executionPool {
			t.Fatal("custom provider with an optional built-in name bypassed execution bound", method)
		}
	}
	for _, limit := range []int{-1, 257} {
		cfg := Config{Name: "invalid", Gateway: "http://localhost", Token: testToken, DataDir: t.TempDir(), WorkDir: t.TempDir(), MaxOperations: limit}
		if err := cfg.defaults(); err == nil {
			t.Fatal("invalid operation limit accepted", limit)
		}
	}
}

func TestOperationPoolBoundsTasksAndCalls(t *testing.T) {
	n := localNode(t, func(cfg *Config) { cfg.MaxTasks, cfg.MaxOperations = 1, 1 })
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	started := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
	}))
	defer server.Close()
	requestCtx, stopRequest := context.WithCancel(ctx)
	defer stopRequest()
	done := make(chan error, 1)
	go func() {
		done <- n.Call(requestCtx, "", "http.request", map[string]string{"url": server.URL}, nil)
	}()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if n.pauseForUpdate() {
		t.Fatal("maintenance ignored an active operation")
	}
	if _, err := n.leaseMethod(n.Identity.ID, "leases.acquire", model.JSON(map[string]any{})); err == nil {
		t.Fatal("lease ignored an active synchronous operation")
	}
	queued, err := n.startTaskContext(ctx, n.Identity.ID, model.TaskSpec{ID: "queued-operation", Capability: "files.list", Args: model.JSON(map[string]any{})})
	if err != nil {
		t.Fatal(err)
	}
	if queued.Phase != "waiting for operation slot" {
		t.Fatal("operation queue phase missing", queued)
	}
	blocked, stop := context.WithCancel(ctx)
	defer stop()
	blockedDone := make(chan error, 1)
	go func() { blockedDone <- n.Call(blocked, "", "files.list", map[string]any{}, nil) }()
	eventually(t, ctx, func() bool { return *n.operationsPool.snapshot().Queued == 2 })
	var description struct {
		Concurrency model.NodeConcurrency `json:"concurrency"`
	}
	if err := n.Call(ctx, "", "node.describe", map[string]any{}, &description); err != nil {
		t.Fatal(err)
	}
	assertPool(t, description.Concurrency.Operations, 1, 1, 2)
	var snapshot model.NodeActivitySnapshot
	if err := n.Call(ctx, "", "activities.list", map[string]any{}, &snapshot); err != nil || snapshot.Concurrency == nil {
		t.Fatal("activity pool visibility missing", err)
	}
	assertPool(t, snapshot.Concurrency.Operations, 1, 1, 2)
	stop()
	select {
	case err := <-blockedDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatal("synchronous call bypassed shared operation limit", err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	assertPool(t, n.operationsPool.snapshot(), 1, 1, 1)
	// A saturated operation pool must not prevent an independent command.
	if _, err := n.startTaskContext(ctx, n.Identity.ID, execSpec(t, "independent-command", "brief")); err != nil {
		t.Fatal(err)
	}
	command, err := n.waitTask(ctx, n.Identity.ID, "independent-command", 5*time.Second)
	if err != nil || command.State != "succeeded" {
		t.Fatal("operations blocked execution", command, err)
	}
	if _, err := n.taskMethod(ctx, n.Identity.ID, "tasks.cancel", model.JSON(map[string]string{"id": queued.ID})); err != nil {
		t.Fatal(err)
	}
	finished, err := n.waitTask(ctx, n.Identity.ID, queued.ID, 5*time.Second)
	if err != nil || finished.State != "cancelled" || n.operationsPool.snapshot().InUse != 1 {
		t.Fatal("queued cancellation affected the running operation", finished, err)
	}
	assertPool(t, n.operationsPool.snapshot(), 1, 1, 0)
	// Returned observations do not share mutable queue counters with the node.
	assertPool(t, snapshot.Concurrency.Operations, 1, 1, 2)
	stopRequest()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancelled operation succeeded")
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if err := n.Call(ctx, "", "files.list", map[string]any{}, nil); err != nil {
		t.Fatal("operation capacity leaked after cancellation", err)
	}
	eventually(t, ctx, func() bool { return n.pauseForUpdate() })
	n.work.Resume()
}

func TestPeerWorkerMultitasking(t *testing.T) {
	for _, relay := range []bool{true, false} {
		t.Run(fmt.Sprintf("relay=%v", relay), func(t *testing.T) {
			source, worker, _ := cluster(t, relay, func(i int, cfg *Config) {
				cfg.MaxTasks, cfg.MaxOperations = 2, 2
				if i == 1 {
					cfg.RPA = rpaCommand(t, "ok")
				}
			})
			worker.rpaLockPath = filepath.Join(t.TempDir(), "rpa.lock")
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			for _, id := range []string{"background-one", "background-two"} {
				call(t, source, "worker", "tasks.start", execSpec(t, id, "sleep"), nil)
			}
			eventually(t, ctx, func() bool { return worker.executionPool.snapshot().InUse == 2 })
			// These must finish while every execution slot remains occupied.
			for _, method := range []string{"files.write", "files.read", "files.list"} {
				params := map[string]any{"path": "parallel.txt", "data": "b2s="}
				if method == "files.list" {
					params = map[string]any{}
				}
				if err := testCall(t, source, ctx, "worker", method, params, nil); err != nil {
					t.Fatal(method, err)
				}
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("ok")) }))
			defer server.Close()
			if err := testCall(t, source, ctx, "worker", "http.request", map[string]string{"url": server.URL}, nil); err != nil {
				t.Fatal("commands blocked HTTP", err)
			}
			call(t, source, "worker", "rpa.run", map[string]any{"actions": []map[string]any{{"type": "inspect"}}}, nil)
			call(t, source, "worker", "tasks.start", model.TaskSpec{ID: "independent-files", Capability: "files.list", Args: model.JSON(map[string]any{})}, nil)
			finished, err := testWaitTask(t, source, ctx, "worker", "independent-files")
			if err != nil || finished.State != "succeeded" {
				t.Fatal("commands blocked an operation task", finished, err)
			}
			var description struct {
				Concurrency model.NodeConcurrency `json:"concurrency"`
			}
			call(t, source, "worker", "node.describe", map[string]any{}, &description)
			assertPool(t, description.Concurrency.Execution, 2, 2, 0)
			if description.Concurrency.Operations.Limit != 2 {
				t.Fatal("discovery does not report operation capacity", description)
			}
			mode := "webrtc"
			if relay {
				mode = "relay"
			}
			if got := testConnections(t, source)[worker.Identity.ID]; got != mode {
				t.Fatalf("transport = %q, want %q", got, mode)
			}
			for _, id := range []string{"background-one", "background-two"} {
				call(t, source, "worker", "tasks.cancel", map[string]string{"id": id}, nil)
				if _, err := testWaitTask(t, source, ctx, "worker", id); err != nil {
					t.Fatal(err)
				}
			}
			// Independent pools do not weaken explicitly requested exclusivity.
			var lease model.Lease
			call(t, source, "worker", "leases.acquire", map[string]any{}, &lease)
			if err := testCall(t, source, ctx, "worker", "files.list", map[string]any{}, nil); err == nil {
				t.Fatal("operation bypassed exclusive lease")
			}
			call(t, source, "worker", "tasks.start", model.TaskSpec{ID: "leased-files", Capability: "files.list", Args: json.RawMessage(`{}`), LeaseID: lease.ID}, nil)
			finished, err = testWaitTask(t, source, ctx, "worker", "leased-files")
			if err != nil || finished.State != "succeeded" {
				t.Fatal("leased operation failed", finished, err)
			}
			call(t, source, "worker", "leases.release", map[string]string{"id": lease.ID}, nil)
		})
	}
}
