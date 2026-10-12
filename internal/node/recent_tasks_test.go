package node

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/store"
)

func recentTask(id string, finished time.Time) *model.Task {
	return &model.Task{ID: id, Owner: "owner", Spec: model.TaskSpec{ID: id, Capability: "exec.run", Args: model.JSON(map[string]string{"secret": "private-arguments"})}, Result: model.JSON("private-result"), State: "succeeded", Created: finished.Add(-time.Second), Updated: finished}
}

func TestRecentTaskIndexBoundsMetadataAndMergesOperations(t *testing.T) {
	n := localNode(t, nil)
	now := time.Now().UTC()
	n.mu.Lock()
	for i := range 100 {
		task := recentTask(fmt.Sprintf("done-%03d", i), now.Add(time.Duration(i-100)*time.Second))
		n.tasks[task.ID] = task
		n.recordRecentTaskLocked(task)
	}
	// Out-of-order insertion and timestamp ties must have deterministic order.
	for _, id := range []string{"tie-b", "tie-a"} {
		task := recentTask(id, now)
		n.tasks[id] = task
		n.recordRecentTaskLocked(task)
		n.recordRecentTaskLocked(task) // Duplicate publication does not duplicate rows.
	}
	n.tasks["running"] = &model.Task{ID: "running", State: "running", Created: now}
	n.cancels["running"] = func() {}
	// Completion is published before the task releases its cancel function.
	n.cancels["tie-a"] = func() {}
	count := len(n.recentTasks)
	n.mu.Unlock()
	if count != maxRecentActivities {
		t.Fatalf("recent index has %d entries", count)
	}
	h := n.beginActivity(context.Background(), "operation", "http.request", "owner", "peer")
	h.finish(nil)
	snapshot := n.activitySnapshot(model.ActivityQuery{Recent: 5})
	if snapshot.ActiveCount != 1 || snapshot.Active[0].ID != "running" || len(snapshot.Recent) != 5 {
		t.Fatalf("unexpected snapshot: %+v", snapshot)
	}
	if snapshot.Recent[0].ID != h.info.ID || snapshot.Recent[1].ID != "tie-a" || snapshot.Recent[2].ID != "tie-b" || snapshot.Recent[3].ID != "done-099" {
		t.Fatalf("recent ordering: %+v", snapshot.Recent)
	}
	if data := string(model.JSON(snapshot)); strings.Contains(data, "private-") {
		t.Fatal("recent index exposed task arguments or results")
	}
	snapshot.Recent[1].State = "modified"
	if got := n.activitySnapshot(model.ActivityQuery{Recent: 5}); got.Recent[1].State != "succeeded" {
		t.Fatal("snapshot shares mutable index storage")
	}
	if got := n.activitySnapshot(model.ActivityQuery{}); len(got.Recent) != 0 || got.ActiveCount != 1 {
		t.Fatal("recent=0 changed active task observation")
	}
}

func TestRecentTaskIndexLoadsInterruptedTasksAndRefillsAfterPrune(t *testing.T) {
	n := localNode(t, func(cfg *Config) { cfg.TaskRetentionHours = 1; cfg.MaxRetainedTasks = -1 })
	now := time.Now().UTC()
	// The newest 64 are old enough to prune. One older task is protected by
	// a delegation, so removing indexed tasks must refill from retained history.
	for i := range 80 {
		task := recentTask(fmt.Sprintf("done-%03d", i), now.Add(-3*time.Hour+time.Duration(i)*time.Second))
		if err := store.Write(n.taskPath(task.ID), task); err != nil {
			t.Fatal(err)
		}
	}
	pending := recentTask("unfinished", now)
	pending.State = "queued"
	if err := store.Write(n.taskPath(pending.ID), pending); err != nil {
		t.Fatal(err)
	}
	cfg := n.Config
	if err := n.Close(); err != nil {
		t.Fatal(err)
	}
	n = openLocalNode(t, cfg)
	n.delegations["protected"] = &delegationState{grant: model.Delegation{Method: "tasks.start", Params: model.JSON(model.TaskSpec{ID: "done-000"})}}
	before := n.activitySnapshot(model.ActivityQuery{Recent: 64})
	if len(before.Recent) != 64 || before.Recent[0].ID != "unfinished" || before.Recent[0].State != "interrupted" {
		t.Fatalf("startup did not rebuild recent metadata: %+v", before.Recent)
	}
	if err := n.pruneTasks(now); err != nil {
		t.Fatal(err)
	}
	after := n.activitySnapshot(model.ActivityQuery{Recent: 64})
	if len(after.Recent) != 2 || after.Recent[0].ID != "unfinished" || after.Recent[1].ID != "done-000" {
		t.Fatalf("pruning did not refill retained history: %+v", after.Recent)
	}
}

func TestRecentTaskPublicationConcurrentWithSnapshots(t *testing.T) {
	n := localNode(t, nil)
	now := time.Now().UTC()
	n.mu.Lock()
	for i := range 100 {
		id := fmt.Sprintf("task-%03d", i)
		n.tasks[id] = &model.Task{ID: id, State: "running", Created: now}
		n.cancels[id] = func() {}
	}
	n.mu.Unlock()
	var wg sync.WaitGroup
	wg.Go(func() {
		for i := range 100 {
			id := fmt.Sprintf("task-%03d", i)
			if _, err := n.updateTask(id, func(task *model.Task) { task.State, task.Updated = "succeeded", time.Now().UTC() }); err != nil {
				t.Error(err)
				return
			}
		}
	})
	defer wg.Wait()
	for range 100 {
		snapshot := n.activitySnapshot(model.ActivityQuery{Recent: 64})
		for _, a := range snapshot.Active {
			if a.State != "running" {
				t.Fatal("terminal task exposed as active")
			}
		}
	}
	wg.Wait()
	if got := n.activitySnapshot(model.ActivityQuery{Recent: 64}); len(got.Recent) != 64 || got.ActiveCount != 0 {
		t.Fatal("completion publication lost activity metadata")
	}
}

func TestRecentTaskIndexIncludesFailedCompletionPersistence(t *testing.T) {
	n := localNode(t, nil)
	task := recentTask("failed-write", time.Now().UTC())
	task.State = "running"
	n.tasks[task.ID] = task
	n.cancels[task.ID] = func() {}
	// Atomic replacement cannot replace a directory with the task record.
	if err := os.MkdirAll(n.taskPath(task.ID), 0700); err != nil {
		t.Fatal(err)
	}
	n.finishTask(task.ID, n.ctx, nil, nil, nil)
	snapshot := n.activitySnapshot(model.ActivityQuery{Recent: 5})
	if snapshot.ActiveCount != 0 || len(snapshot.Recent) != 1 || snapshot.Recent[0].State != "failed" {
		t.Fatal("failed persistence was missing or reported as successful", snapshot)
	}
}
