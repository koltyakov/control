package node

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/store"
)

// localNode runs tasks without a gateway connection.
func localNode(t *testing.T, configure func(*Config)) *Node {
	t.Helper()
	cfg := Config{Name: "local", Gateway: "http://127.0.0.1:1", Token: testToken, DataDir: t.TempDir(), WorkDir: t.TempDir()}
	if configure != nil {
		configure(&cfg)
	}
	return openLocalNode(t, cfg)
}

func openLocalNode(t *testing.T, cfg Config) *Node {
	t.Helper()
	n, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	n.ctx, n.cancel = context.WithCancel(context.Background())
	t.Cleanup(func() { _ = n.Close() })
	return n
}

func execSpec(t *testing.T, id, mode string) model.TaskSpec {
	return model.TaskSpec{ID: id, Capability: "exec.run", Args: model.JSON(executable(t, mode))}
}

func TestBlockingTaskGetWakesOnCompletion(t *testing.T) {
	n := localNode(t, nil)
	ctx := context.Background()
	brief, err := n.startTaskContext(ctx, "owner", execSpec(t, "brief", "brief"))
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	got, err := n.taskMethod(ctx, "owner", "tasks.get", model.JSON(map[string]any{"id": brief.ID, "waitSeconds": 20}))
	if err != nil || !got.(model.Task).Terminal() || time.Since(started) > 10*time.Second {
		t.Fatal("wait did not return on completion", got, err)
	}
	long, err := n.startTaskContext(ctx, "owner", execSpec(t, "long", "sleep"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = n.taskMethod(ctx, "owner", "tasks.cancel", model.JSON(map[string]any{"id": long.ID})) }()
	started = time.Now()
	got, err = n.taskMethod(ctx, "owner", "tasks.get", model.JSON(map[string]any{"id": long.ID, "waitSeconds": 1}))
	if elapsed := time.Since(started); err != nil || got.(model.Task).Terminal() || elapsed < 900*time.Millisecond || elapsed > 5*time.Second {
		t.Fatal("bounded wait", elapsed, got, err)
	}
	// The response must leave time to cross the transport before the deadline.
	deadline, cancel := context.WithTimeout(ctx, 1500*time.Millisecond)
	defer cancel()
	started = time.Now()
	if _, err = n.taskMethod(deadline, "owner", "tasks.get", model.JSON(map[string]any{"id": long.ID, "waitSeconds": 30})); err != nil || time.Since(started) > 1200*time.Millisecond {
		t.Fatal("wait ignored the request deadline", time.Since(started), err)
	}
	if _, err = n.taskMethod(ctx, "other", "tasks.get", model.JSON(map[string]any{"id": long.ID, "waitSeconds": 1})); err == nil {
		t.Fatal("another owner waited on a task")
	}
}

func TestConcurrentDuplicateSubmissionAcceptsOnce(t *testing.T) {
	n := localNode(t, nil)
	ctx := context.Background()
	results := make(chan model.Task, 8)
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			task, err := n.startTaskContext(ctx, "owner", execSpec(t, "duplicate", "brief"))
			if err != nil {
				t.Error(err)
			}
			results <- task
		})
	}
	wg.Wait()
	close(results)
	for task := range results {
		if task.ID != "duplicate" || task.Owner != "owner" {
			t.Fatal("duplicate returned a different task", task)
		}
	}
	if _, err := n.waitTask(ctx, "owner", "duplicate", 20*time.Second); err != nil {
		t.Fatal(err)
	}
	log, err := os.ReadFile(n.logPath("duplicate"))
	if err != nil || strings.Count(string(log), "brief") != 1 {
		t.Fatalf("task executed %d times: %q %v", strings.Count(string(log), "brief"), log, err)
	}
	if _, err := n.startTaskContext(ctx, "intruder", execSpec(t, "duplicate", "brief")); err == nil {
		t.Fatal("another owner reused a task ID")
	}
}

func TestLeaseAcquireWaitsForReservedAcceptance(t *testing.T) {
	n := localNode(t, nil)
	n.mu.Lock()
	n.accepting["pending"] = &acceptance{done: make(chan struct{})}
	n.mu.Unlock()
	if _, err := n.leaseMethod("owner", "leases.acquire", model.JSON(map[string]any{})); err == nil || !strings.Contains(err.Error(), "active tasks") {
		t.Fatal("lease granted while a task acceptance was being written", err)
	}
	n.mu.Lock()
	delete(n.accepting, "pending")
	n.mu.Unlock()
	if _, err := n.leaseMethod("owner", "leases.acquire", model.JSON(map[string]any{})); err != nil {
		t.Fatal(err)
	}
}

func TestRetentionPrunesTerminalTasksAndKeepsTombstones(t *testing.T) {
	cfg := Config{Name: "local", Gateway: "http://127.0.0.1:1", Token: testToken, DataDir: t.TempDir(), WorkDir: t.TempDir(), TaskRetentionHours: 1, MaxRetainedTasks: 2}
	n := openLocalNode(t, cfg)
	now := time.Now().UTC()
	spec := func(id string) model.TaskSpec {
		return model.TaskSpec{ID: id, Capability: "exec.run", Args: model.JSON(map[string]any{"command": "true"}), TimeoutSeconds: 3600}
	}
	add := func(id, state string, updated time.Time) {
		task := &model.Task{ID: id, Owner: "owner", Spec: spec(id), State: state, Created: updated, Updated: updated}
		if err := store.Write(n.taskPath(id), task); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(n.logPath(id), []byte("log"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(cfg.WorkDir, "tasks", id), 0700); err != nil {
			t.Fatal(err)
		}
		n.mu.Lock()
		n.tasks[id] = task
		n.mu.Unlock()
	}
	add("aged", "failed", now.Add(-2*time.Hour))
	for i := range 4 {
		add(fmt.Sprintf("recent-%d", i), "succeeded", now.Add(time.Duration(i-10)*time.Minute))
	}
	add("running", "running", now.Add(-3*time.Hour))
	n.mu.Lock()
	n.cancels["running"] = func() {}
	n.mu.Unlock()
	if err := n.pruneTasks(now); err != nil {
		t.Fatal(err)
	}
	n.mu.Lock()
	delete(n.cancels, "running")
	remaining := len(n.tasks)
	n.mu.Unlock()
	if remaining != 3 {
		t.Fatalf("retained %d tasks, want 2 newest terminal and the running task", remaining)
	}
	for _, id := range []string{"aged", "recent-0", "recent-1"} {
		for _, path := range []string{n.taskPath(id), n.logPath(id), filepath.Join(cfg.WorkDir, "tasks", id)} {
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatal("pruned task left", path, err)
			}
		}
		got, err := n.taskMethod(context.Background(), "owner", "tasks.get", model.JSON(map[string]any{"id": id}))
		if task, _ := got.(model.Task); err != nil || !task.Pruned || !task.Terminal() {
			t.Fatal("pruned task not reconcilable", got, err)
		}
	}
	for _, id := range []string{"recent-2", "recent-3", "running"} {
		if _, err := os.Stat(n.taskPath(id)); err != nil {
			t.Fatal("retained task removed", id, err)
		}
	}
	if err := n.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := openLocalNode(t, cfg)
	// A retried submission of a pruned ID returns its outcome; it never runs again.
	again, err := reopened.startTaskContext(context.Background(), "owner", spec("recent-0"))
	if err != nil || !again.Pruned || again.State != "succeeded" {
		t.Fatal("pruned submission was not deduplicated", again, err)
	}
	if _, err := os.Stat(reopened.taskPath("recent-0")); !os.IsNotExist(err) {
		t.Fatal("pruned task executed again", err)
	}
	changed := spec("recent-0")
	changed.Args = model.JSON(map[string]any{"command": "false"})
	if _, err := reopened.startTaskContext(context.Background(), "owner", changed); err == nil {
		t.Fatal("pruned ID reused with a different specification")
	}
}

func TestArtifactOwnersScopeListingAndDeletion(t *testing.T) {
	n := localNode(t, nil)
	as := func(owner string) context.Context {
		return context.WithValue(context.Background(), activityContextKey{}, activityContext{owner: owner})
	}
	a, err := n.importArtifact(as("alice"), bytes.NewReader([]byte("shared content")), "a.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = n.importArtifact(as("bob"), bytes.NewReader([]byte("shared content")), "b.txt"); err != nil {
		t.Fatal(err)
	}
	list := func(owner string) []model.Artifact {
		value, err := n.artifactMethod(as(owner), owner, "artifacts.list", model.JSON(map[string]any{}))
		if err != nil {
			t.Fatal(err)
		}
		return value.([]model.Artifact)
	}
	if len(list("alice")) != 1 || len(list("bob")) != 1 || len(list("carol")) != 0 || len(list(n.Identity.ID)) != 1 {
		t.Fatal("artifact listing ignored owners")
	}
	remove := func(owner string) error {
		_, err := n.artifactMethod(as(owner), owner, "artifacts.delete", model.JSON(map[string]any{"id": a.ID}))
		return err
	}
	if remove("carol") == nil {
		t.Fatal("non-owner deleted an artifact")
	}
	if err = remove("alice"); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(n.artifactPath(a.ID)); err != nil || len(list("alice")) != 0 || len(list("bob")) != 1 {
		t.Fatal("one owner's deletion removed shared content", err)
	}
	if err = remove("bob"); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(n.artifactPath(a.ID)); !os.IsNotExist(err) {
		t.Fatal("last owner's deletion kept content", err)
	}
	// Records written before owners were tracked remain shared.
	legacy, err := n.importArtifact(as("alice"), bytes.NewReader([]byte("legacy")), "legacy.txt")
	if err != nil {
		t.Fatal(err)
	}
	if err = store.Write(n.artifactPath(legacy.ID)+".json", legacy); err != nil {
		t.Fatal(err)
	}
	if len(list("carol")) != 1 {
		t.Fatal("legacy artifact hidden")
	}
	n.artifactMu.Lock()
	transfers := len(n.transfers)
	n.artifactMu.Unlock()
	if transfers != 0 {
		t.Fatal("transfer locks retained", transfers)
	}
}

func TestWorkflowRunsIndependentStepsInParallel(t *testing.T) {
	source, _, _ := cluster(t, true, nil)
	steps := []WorkflowStep{
		{Name: "a", Target: "worker", Task: execSpec(t, "", "delayed")},
		{Name: "b", Target: "worker", Task: execSpec(t, "", "delayed")},
		{Name: "c", Target: "worker", Needs: []string{"a", "b"}, Task: execSpec(t, "", "brief")},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var task model.Task
	call(t, source, "source", "tasks.start", model.TaskSpec{ID: "parallel", Capability: "workflow.run", Args: model.JSON(map[string]any{"steps": steps, "maxParallel": 2})}, &task)
	finished, err := testWaitTask(t, source, ctx, "source", task.ID)
	if err != nil || finished.State != "succeeded" {
		t.Fatal("workflow", finished, err)
	}
	var result map[string]model.Task
	if err = json.Unmarshal(finished.Result, &result); err != nil {
		t.Fatal(err)
	}
	if gap := result["a"].Created.Sub(result["b"].Created).Abs(); gap > 700*time.Millisecond {
		t.Fatal("independent steps did not run concurrently", gap)
	}
	if result["c"].Created.Before(result["a"].Updated) || result["c"].Created.Before(result["b"].Updated) {
		t.Fatal("dependent step started before its dependencies finished")
	}
	failing := []WorkflowStep{
		{Name: "bad", Target: "worker", Task: model.TaskSpec{Capability: "exec.run", Args: model.JSON(map[string]any{"command": filepath.Join(t.TempDir(), "missing")})}},
		{Name: "after", Target: "worker", Needs: []string{"bad"}, Task: execSpec(t, "", "brief")},
	}
	call(t, source, "source", "tasks.start", model.TaskSpec{ID: "failing", Capability: "workflow.run", Args: model.JSON(map[string]any{"steps": failing, "maxParallel": 2})}, &task)
	finished, err = testWaitTask(t, source, ctx, "source", task.ID)
	if err != nil || finished.State != "failed" {
		t.Fatal("failing workflow", finished, err)
	}
	result = nil
	if err = json.Unmarshal(finished.Result, &result); err != nil || len(result) != 1 || result["bad"].State != "failed" {
		t.Fatal("a step started after its dependency failed", result, err)
	}
	if err = testCall(t, source, ctx, "source", "workflow.run", map[string]any{"steps": steps, "maxParallel": 99}, nil); err == nil {
		t.Fatal("unbounded parallelism accepted")
	}
}
