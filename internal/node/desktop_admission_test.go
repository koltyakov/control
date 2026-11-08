package node

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gofrs/flock"
	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/transport/transporttest"
)

func TestProviderPoolQueueCancellation(t *testing.T) {
	pool := newProviderPool(1)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	release, err := acquireProviderSlot(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	// An already-cancelled request must not enter the queue or consume a slot.
	stopped, stop := context.WithCancel(ctx)
	stop()
	if _, err := acquireProviderSlot(stopped, pool); err == nil {
		t.Fatal("admitted an already-cancelled request")
	}
	assertPool(t, pool.snapshot(), 1, 1, 0)
	for range 3 {
		waiting, cancelWait := context.WithCancel(ctx)
		done := make(chan error, 1)
		go func() {
			release, err := acquireProviderSlot(waiting, pool)
			if err == nil {
				release()
			}
			done <- err
		}()
		eventually(t, ctx, func() bool { return *pool.snapshot().Queued == 1 })
		cancelWait()
		select {
		case err := <-done:
			if err == nil {
				t.Fatal("cancelled waiter acquired the occupied slot")
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		assertPool(t, pool.snapshot(), 1, 1, 0)
	}
}

func TestProviderPoolQueuedAdmission(t *testing.T) {
	pool := newProviderPool(1)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	first, err := acquireProviderSlot(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	held := true
	defer func() {
		if held {
			first()
		}
	}()
	ready := make(chan error, 1)
	finish := make(chan struct{})
	finished := make(chan struct{})
	defer func() {
		cancel()
		close(finish)
		<-finished
	}()
	go func() {
		defer close(finished)
		release, err := acquireProviderSlot(ctx, pool)
		ready <- err
		if err == nil {
			select {
			case <-finish:
			case <-ctx.Done():
			}
			release()
		}
	}()
	eventually(t, ctx, func() bool { return *pool.snapshot().Queued == 1 })
	assertPool(t, pool.snapshot(), 1, 1, 1)
	first()
	held = false
	select {
	case err := <-ready:
		if err != nil {
			t.Fatal(err)
		}
		assertPool(t, pool.snapshot(), 1, 1, 0)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}

func TestDesktopWaitersDoNotBlockFiles(t *testing.T) {
	n := localNode(t, func(cfg *Config) {
		cfg.MaxTasks, cfg.MaxOperations = 1, 1
		cfg.Desktop = desktopCommand(t, "echo")
	})
	n.desktopLockPath = filepath.Join(t.TempDir(), "shared-desktop.lock")
	lock := flock.New(n.desktopLockPath)
	defer func() { _ = lock.Close() }()
	if ok, err := lock.TryLock(); err != nil || !ok {
		t.Fatal("hold fake desktop lock", err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	request := model.JSON(map[string]any{"command": "status"})
	active, stopActive := context.WithCancel(ctx)
	defer stopActive()
	activeDone := make(chan error, 1)
	go func() { activeDone <- n.Call(active, "", "desktop.run", request, nil) }()
	eventually(t, ctx, func() bool { return n.desktopPool.snapshot().InUse == 1 })
	queuedTask, err := n.startTaskContext(ctx, n.Identity.ID, model.TaskSpec{ID: "queued-desktop", Capability: "desktop.run", Args: request})
	if err != nil || queuedTask.Phase != "waiting for desktop slot" {
		t.Fatal("desktop task admission", queuedTask, err)
	}
	waiting, stopWaiting := context.WithCancel(ctx)
	defer stopWaiting()
	waitingDone := make(chan error, 1)
	go func() { waitingDone <- n.Call(waiting, "", "desktop.run", request, nil) }()
	eventually(t, ctx, func() bool { return *n.desktopPool.snapshot().Queued == 2 })
	if n.pauseForUpdate() {
		t.Fatal("desktop admission did not protect maintenance")
	}
	if _, err := n.leaseMethod(n.Identity.ID, "leases.acquire", json.RawMessage(`{}`)); err == nil {
		t.Fatal("exclusive lease ignored desktop work")
	}
	// The shared desktop lock remains held throughout these file calls.
	files, stopFiles := context.WithTimeout(ctx, 2*time.Second)
	defer stopFiles()
	for _, method := range []string{"files.write", "files.read", "files.list"} {
		params := map[string]any{"path": "responsive.txt", "data": "b2s=", "truncate": true}
		if method == "files.list" {
			params = map[string]any{}
		}
		if err := n.Call(files, "", method, params, nil); err != nil {
			t.Fatal("desktop waiters blocked", method, err)
		}
	}
	var description struct {
		Concurrency model.NodeConcurrency `json:"concurrency"`
	}
	if err := n.Call(ctx, "", "node.describe", map[string]any{}, &description); err != nil || description.Concurrency.Desktop == nil {
		t.Fatal("desktop description missing", err)
	}
	assertPool(t, *description.Concurrency.Desktop, 1, 1, 2)
	assertPool(t, description.Concurrency.Operations, 1, 0, 0)
	var activity model.NodeActivitySnapshot
	if err := n.Call(ctx, "", "activities.list", map[string]any{}, &activity); err != nil || activity.Concurrency == nil || activity.Concurrency.Desktop == nil {
		t.Fatal("desktop activity report missing", err)
	}
	assertPool(t, *activity.Concurrency.Desktop, 1, 1, 2)
	stopWaiting()
	select {
	case err := <-waitingDone:
		if err == nil {
			t.Fatal("cancelled desktop waiter succeeded")
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if _, err := n.taskMethod(ctx, n.Identity.ID, "tasks.cancel", model.JSON(map[string]string{"id": queuedTask.ID})); err != nil {
		t.Fatal(err)
	}
	finished, err := n.waitTask(ctx, n.Identity.ID, queuedTask.ID, 3*time.Second)
	if err != nil || finished.State != "cancelled" {
		t.Fatal("queued desktop cancellation", finished, err)
	}
	assertPool(t, n.desktopPool.snapshot(), 1, 1, 0)
	stopActive()
	select {
	case err := <-activeDone:
		if err == nil {
			t.Fatal("desktop lock waiter succeeded while lock was held")
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	assertPool(t, n.desktopPool.snapshot(), 1, 0, 0)
	if err := lock.Unlock(); err != nil {
		t.Fatal(err)
	}
	if err := n.Call(ctx, "", "desktop.run", request, nil); err != nil {
		t.Fatal("desktop capacity leaked after cancellation", err)
	}
	assertPool(t, n.desktopPool.snapshot(), 1, 0, 0)
}

func TestDesktopAdmissionPreservesCrossProfileLock(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	trace := filepath.Join(t.TempDir(), "trace")
	lockPath := filepath.Join(t.TempDir(), "desktop.lock")
	done := make(chan error, 2)
	for range 2 {
		n := localNode(t, func(cfg *Config) { cfg.Desktop = desktopCommand(t, "serial") })
		n.desktopLockPath = lockPath
		n.Config.Desktop.Env["CONTROL_TEST_DESKTOP_TRACE"] = trace
		go func() {
			done <- n.Call(ctx, "", "desktop.run", map[string]any{"command": "status"}, nil)
		}()
	}
	for range 2 {
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	data, err := os.ReadFile(trace)
	if err != nil || string(data) != "start\nend\nstart\nend\n" {
		t.Fatalf("native desktop invocations overlapped across profiles: %q, %v", data, err)
	}
}

func TestPeerDesktopPoolVisibility(t *testing.T) {
	for _, relay := range []bool{true, false} {
		t.Run(fmt.Sprintf("relay=%v", relay), func(t *testing.T) {
			source, worker, _ := cluster(t, relay, func(i int, cfg *Config) {
				cfg.MaxOperations = 1
				if i == 1 {
					cfg.Desktop = desktopCommand(t, "echo")
				}
			}, transporttest.Loopback())
			worker.desktopLockPath = filepath.Join(t.TempDir(), "desktop.lock")
			lock := flock.New(worker.desktopLockPath)
			defer func() { _ = lock.Close() }()
			if ok, err := lock.TryLock(); err != nil || !ok {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 25*time.Second)
			defer cancel()
			for _, id := range []string{"desktop-one", "desktop-two"} {
				call(t, source, "worker", "tasks.start", model.TaskSpec{ID: id, Capability: "desktop.run", Args: model.JSON(map[string]any{"command": "status"})}, nil)
			}
			eventually(t, ctx, func() bool { return *worker.desktopPool.snapshot().Queued == 1 })
			files, stopFiles := context.WithTimeout(ctx, 2*time.Second)
			err := testCall(t, source, files, "worker", "files.list", map[string]any{}, nil)
			stopFiles()
			if err != nil {
				t.Fatal("desktop waiters blocked remote files", err)
			}
			var pool model.PoolActivitySnapshot
			call(t, source, "", "activities.pool", model.PoolActivityQuery{Nodes: []string{"worker"}}, &pool)
			if len(pool.Nodes) != 1 || pool.Nodes[0].Concurrency == nil || pool.Nodes[0].Concurrency.Desktop == nil {
				t.Fatal("aggregated pool visibility missing", pool)
			}
			assertPool(t, *pool.Nodes[0].Concurrency.Desktop, 1, 1, 1)
			assertPool(t, pool.Nodes[0].Concurrency.Operations, 1, 0, 0)
			mode := "webrtc"
			if relay {
				mode = "relay"
			}
			if got := testConnections(t, source)[worker.Identity.ID]; got != mode {
				t.Fatalf("transport = %q, want %q", got, mode)
			}
			for _, id := range []string{"desktop-one", "desktop-two"} {
				call(t, source, "worker", "tasks.cancel", map[string]string{"id": id}, nil)
				if _, err := testWaitTask(t, source, ctx, "worker", id); err != nil {
					t.Fatal(err)
				}
			}
			eventually(t, ctx, func() bool {
				return worker.desktopPool.snapshot().InUse == 0 && *worker.desktopPool.snapshot().Queued == 0
			})
		})
	}
}
