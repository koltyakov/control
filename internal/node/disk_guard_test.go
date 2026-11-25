package node

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/koltyakov/control/internal/model"
)

// fakeDisk replaces the free-space probe and clock of one node.
type fakeDisk struct {
	mu         sync.Mutex
	work, data uint64
	err        error
	calls      int
	block      chan struct{}
	clock      time.Time
	dataDir    string
}

func installFakeDisk(n *Node, work, data uint64) *fakeDisk {
	f := &fakeDisk{work: work, data: data, clock: time.Unix(1_700_000_000, 0), dataDir: n.Config.DataDir}
	n.taskDisk.free, n.taskDisk.now = f.free, f.now
	return f
}

func (f *fakeDisk) free(path string) (uint64, error) {
	f.mu.Lock()
	f.calls++
	block := f.block
	f.mu.Unlock()
	if block != nil {
		<-block
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return 0, errors.New(f.err.Error() + " " + path)
	}
	if path == f.dataDir {
		return f.data, nil
	}
	return f.work, nil
}

func (f *fakeDisk) now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.clock
}

// set changes the free space and expires the cached reading.
func (f *fakeDisk) set(work, data uint64) {
	f.mu.Lock()
	f.work, f.data = work, data
	f.clock = f.clock.Add(diskGuardMaxAge + time.Second)
	f.mu.Unlock()
}

func (f *fakeDisk) probes() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func lowDiskNode(t *testing.T, work, data uint64) (*Node, *fakeDisk) {
	t.Helper()
	n := localNode(t, func(cfg *Config) { cfg.TaskReserveBytes = 1 << 20; cfg.TaskRetentionHours = 1 })
	return n, installFakeDisk(n, work, data)
}

func requireNotAccepted(t *testing.T, err error) {
	t.Helper()
	var rpc *model.RPCError
	if !errors.As(err, &rpc) || rpc.Code != diskInsufficientCode || rpc.Outcome != model.OutcomeNotAccepted {
		t.Fatalf("want %s/%s refusal, got %#v", diskInsufficientCode, model.OutcomeNotAccepted, err)
	}
	if !strings.Contains(rpc.Message, "taskReserveBytes") || !strings.Contains(rpc.Message, "minDiskBytes") {
		t.Fatalf("refusal does not say what to do: %s", rpc.Message)
	}
}

func TestTaskDiskGuardRefusesBelowReserve(t *testing.T) {
	for _, tc := range []struct{ name, which string }{{"workspace", "task workspace"}, {"data", "node data"}} {
		t.Run(tc.name, func(t *testing.T) {
			work, data := uint64(1<<30), uint64(1<<30)
			if tc.name == "workspace" {
				work = 4096
			} else {
				data = 4096
			}
			n, _ := lowDiskNode(t, work, data)
			ctx := context.Background()
			_, receipt, err := n.dispatchWithReceipt(ctx, n.Identity.ID, "tasks.start", model.JSON(execSpec(t, "refused", "brief")))
			requireNotAccepted(t, err)
			if !strings.Contains(err.Error(), tc.which) {
				t.Fatalf("refusal does not name the %s filesystem: %v", tc.which, err)
			}
			if receipt == nil || receipt.Status != "rejected" || receipt.TaskID != "refused" {
				t.Fatalf("receipt %+v does not prove the submission was not adopted", receipt)
			}
			n.mu.Lock()
			retained, pending, reserved, active := len(n.tasks), len(n.accepting), len(n.taskIDs), len(n.cancels)
			n.mu.Unlock()
			if retained+pending+reserved+active != 0 {
				t.Fatal("refusal left node state", retained, pending, reserved, active)
			}
			for _, path := range []string{n.taskPath("refused"), n.logPath("refused"), filepath.Join(n.Config.WorkDir, "tasks", "refused")} {
				if _, err := os.Stat(path); !os.IsNotExist(err) {
					t.Fatal("refusal left a file", path, err)
				}
			}
			if _, err := n.taskMethod(ctx, n.Identity.ID, "tasks.get", model.JSON(map[string]any{"id": "refused"})); err == nil {
				t.Fatal("refused task is visible")
			}
			// Synchronous workspace writes are refused; removal stays available.
			if _, err := fileOp(t, n, "files.write", map[string]any{"path": "a.txt", "data": "x", "encoding": "text"}); err != nil {
				requireNotAccepted(t, err)
			} else {
				t.Fatal("files.write ignored the reserve")
			}
			if _, err := os.Stat(filepath.Join(n.Config.WorkDir, "a.txt")); !os.IsNotExist(err) {
				t.Fatal("refused write created a file", err)
			}
			if err := os.WriteFile(filepath.Join(n.Config.WorkDir, "b.txt"), []byte("b"), 0600); err != nil {
				t.Fatal(err)
			}
			mustFileOp(t, n, "files.remove", map[string]any{"path": "b.txt"})
			diagnostics := n.diagnostics(ctx)["taskDisk"].(map[string]any)
			if diagnostics["state"] != "low" || diagnostics["refusing"] != true || diagnostics["reserveBytes"] != int64(1<<20) {
				t.Fatal("diagnostics do not report the refusal", diagnostics)
			}
		})
	}
}

func TestTaskDiskGuardAcceptsAboveReserveAndWhenDisabled(t *testing.T) {
	t.Parallel()
	n, _ := lowDiskNode(t, 1<<30, 1<<30)
	ctx := context.Background()
	task, err := n.startTaskContext(ctx, "owner", execSpec(t, "ample", "brief"))
	if err != nil {
		t.Fatal(err)
	}
	if done, err := n.waitTask(ctx, "owner", task.ID, 20*time.Second); err != nil || done.State != "succeeded" {
		t.Fatal("task did not succeed", done, err)
	}
	if state := n.diagnostics(ctx)["taskDisk"].(map[string]any); state["state"] != "ok" || state["refusing"] != false {
		t.Fatal("diagnostics", state)
	}
	disabled := localNode(t, func(cfg *Config) { cfg.TaskReserveBytes = -1 })
	f := installFakeDisk(disabled, 0, 0)
	task, err = disabled.startTaskContext(ctx, "owner", execSpec(t, "unguarded", "brief"))
	if err != nil {
		t.Fatal("disabled guard refused work", err)
	}
	if _, err = disabled.waitTask(ctx, "owner", task.ID, 20*time.Second); err != nil {
		t.Fatal(err)
	}
	if f.probes() != 0 {
		t.Fatal("disabled guard probed the disk")
	}
	if state := disabled.diagnostics(ctx)["taskDisk"].(map[string]any); state["state"] != "disabled" || state["refusing"] != false {
		t.Fatal("diagnostics", state)
	}
}

func TestTaskDiskGuardUnknownProbeAllowsWork(t *testing.T) {
	t.Parallel()
	n, f := lowDiskNode(t, 0, 0)
	f.mu.Lock()
	f.err = errors.New("permission denied")
	f.mu.Unlock()
	ctx := context.Background()
	task, err := n.startTaskContext(ctx, "owner", execSpec(t, "unknown", "brief"))
	if err != nil {
		t.Fatal("failed probe refused work", err)
	}
	if _, err = n.waitTask(ctx, "owner", task.ID, 20*time.Second); err != nil {
		t.Fatal(err)
	}
	report := n.diagnostics(ctx)["taskDisk"].(map[string]any)
	work, _ := report["work"].(map[string]any)
	data, _ := report["data"].(map[string]any)
	if report["state"] != "unknown" || report["refusing"] != false || work == nil || data == nil {
		t.Fatal("diagnostics do not report the unknown probe", report)
	}
	for _, r := range []map[string]any{work, data} {
		message, _ := r["error"].(string)
		if !strings.Contains(message, "permission denied") || strings.Contains(message, n.Config.WorkDir) || strings.Contains(message, n.Config.DataDir) {
			t.Fatal("probe error missing or exposes a local path", message)
		}
	}
	// A probe that does not return within the wait is unknown too.
	block := make(chan struct{})
	f.mu.Lock()
	f.err, f.block = nil, block
	f.mu.Unlock()
	f.set(0, 0)
	started := time.Now()
	task, err = n.startTaskContext(ctx, "owner", execSpec(t, "hung", "brief"))
	if err != nil || time.Since(started) > 10*time.Second {
		t.Fatal("hung probe refused or stalled work", time.Since(started), err)
	}
	close(block)
	if _, err = n.waitTask(ctx, "owner", task.ID, 20*time.Second); err != nil {
		t.Fatal(err)
	}
}

func TestTaskDiskGuardCachesReadings(t *testing.T) {
	n, f := lowDiskNode(t, 1<<30, 1<<30)
	ctx := context.Background()
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Go(func() {
			if err := n.admitTaskDisk(ctx, "task"); err != nil {
				t.Error(i, err)
			}
		})
	}
	wg.Wait()
	if got := f.probes(); got != 2 {
		t.Fatalf("concurrent admissions made %d reads, want one probe of two filesystems", got)
	}
	f.mu.Lock()
	f.work = 0
	f.clock = f.clock.Add(diskGuardMaxAge)
	f.mu.Unlock()
	if err := n.admitTaskDisk(ctx, "task"); err != nil || f.probes() != 2 {
		t.Fatal("reading within the staleness bound was not reused", f.probes(), err)
	}
	f.mu.Lock()
	f.clock = f.clock.Add(time.Millisecond)
	f.mu.Unlock()
	requireNotAccepted(t, n.admitTaskDisk(ctx, "task"))
	if f.probes() != 4 {
		t.Fatal("stale reading was not refreshed", f.probes())
	}
	// Freeing space clears the refusal once the reading expires.
	f.set(1<<30, 1<<30)
	if err := n.admitTaskDisk(ctx, "task"); err != nil {
		t.Fatal("refusal outlived the staleness bound", err)
	}
}

func TestTaskDiskGuardNeverBlocksAcceptedTasks(t *testing.T) {
	t.Parallel()
	n, f := lowDiskNode(t, 1<<30, 1<<30)
	ctx := context.Background()
	long, err := n.startTaskContext(ctx, "owner", execSpec(t, "long", "sleep"))
	if err != nil {
		t.Fatal(err)
	}
	brief, err := n.startTaskContext(ctx, "owner", execSpec(t, "brief", "brief"))
	if err != nil {
		t.Fatal(err)
	}
	f.set(0, 0)
	_, err = n.startTaskContext(ctx, "owner", execSpec(t, "late", "brief"))
	requireNotAccepted(t, err)
	if done, err := n.waitTask(ctx, "owner", brief.ID, 20*time.Second); err != nil || done.State != "succeeded" {
		t.Fatal("accepted task did not finish under the guard", done, err)
	}
	// Resubmitting an accepted ID reconciles it instead of being refused.
	again, err := n.startTaskContext(ctx, "owner", execSpec(t, "brief", "brief"))
	if err != nil || again.State != "succeeded" {
		t.Fatal("duplicate submission refused", again, err)
	}
	if _, err := n.taskMethod(ctx, "owner", "tasks.logs", model.JSON(map[string]any{"id": brief.ID})); err != nil {
		t.Fatal("observation refused", err)
	}
	if _, err := n.taskMethod(ctx, "owner", "tasks.cancel", model.JSON(map[string]any{"id": long.ID})); err != nil {
		t.Fatal(err)
	}
	if done, err := n.waitTask(ctx, "owner", long.ID, 20*time.Second); err != nil || done.State != "cancelled" {
		t.Fatal("cancellation blocked under the guard", done, err)
	}
	if err := n.pruneTasks(time.Now().Add(2 * time.Hour)); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{long.ID, brief.ID} {
		if _, err := os.Stat(n.taskPath(id)); !os.IsNotExist(err) {
			t.Fatal("retention blocked under the guard", id, err)
		}
		got, err := n.taskMethod(ctx, "owner", "tasks.get", model.JSON(map[string]any{"id": id}))
		if task, _ := got.(model.Task); err != nil || !task.Pruned {
			t.Fatal("tombstone not written under the guard", got, err)
		}
	}
}

// A child refused for low disk was provably never accepted, so the workflow
// settles it as failed and pre-planned cleanup still becomes ready.
func TestWorkflowSettlesLowDiskRefusal(t *testing.T) {
	n, r := cleanupNode(t)
	n.Config.TaskReserveBytes = 1 << 20
	installFakeDisk(n, 0, 0)
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	results, err := runCleanupWorkflow(ctx, n, []WorkflowStep{
		cleanupStep("work", false, ""),
		cleanupStep("cleanup", false, model.WorkflowWhenAlways, "work"),
	}, 1)
	if len(r.ran()) != 0 || len(results) != 0 {
		t.Fatal("refused children ran or were recorded", r.ran(), results)
	}
	var unresolved unresolvedStepError
	if err == nil || errors.As(err, &unresolved) || strings.Contains(err.Error(), "unknown") {
		t.Fatalf("refusal was treated as uncertain: %v", err)
	}
	// The cleanup step was submitted (and refused too), so work had settled.
	for _, want := range []string{"submit work", "submit cleanup", "below taskReserveBytes", "refused for low disk space"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("workflow error lacks %q: %v", want, err)
		}
	}
}

func TestTaskReserveConfig(t *testing.T) {
	for _, tc := range []struct {
		in, want int64
		ok       bool
	}{{0, 256 << 20, true}, {-1, -1, true}, {1, 1, true}, {-2, 0, false}} {
		cfg := Config{Name: "local", Gateway: "http://127.0.0.1:1", Token: testToken, DataDir: t.TempDir(), WorkDir: t.TempDir(), TaskReserveBytes: tc.in}
		err := cfg.defaults()
		if (err == nil) != tc.ok || (tc.ok && cfg.TaskReserveBytes != tc.want) {
			t.Fatal(tc.in, cfg.TaskReserveBytes, err)
		}
	}
	var cfg Config
	if err := json.Unmarshal([]byte(`{"taskReserveBytes": 1048576}`), &cfg); err != nil || cfg.TaskReserveBytes != 1<<20 {
		t.Fatal(cfg.TaskReserveBytes, err)
	}
}
