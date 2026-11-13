package node

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/store"
)

func TestCaseFoldedTaskIDsKeepOwnersAndRecoverySeparate(t *testing.T) {
	n := localNode(t, func(cfg *Config) { cfg.TaskRetentionHours = 1 })
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	type submission struct {
		task model.Task
		err  error
	}
	results := make(chan submission, 2)
	start := make(chan struct{})
	var submitters sync.WaitGroup
	for _, id := range []string{"Build", "build"} {
		spec := execSpec(t, id, "brief")
		submitters.Go(func() {
			<-start
			task, err := n.startTaskContext(ctx, id+"-owner", spec)
			results <- submission{task, err}
		})
	}
	close(start)
	submitters.Wait()
	close(results)
	var accepted model.Task
	rejected := 0
	for result := range results {
		if result.err != nil {
			if !strings.Contains(result.err.Error(), "case-fold") {
				t.Fatal("unexpected submission failure", result.err)
			}
			rejected++
		} else if accepted.ID != "" {
			t.Fatal("accepted differently cased IDs", accepted.ID, result.task.ID)
		} else {
			accepted = result.task
		}
	}
	if accepted.ID == "" || rejected != 1 {
		t.Fatal("expected exactly one accepted spelling", accepted, rejected)
	}
	finished, err := n.waitTask(ctx, accepted.Owner, accepted.ID, 10*time.Second)
	if err != nil || finished.State != "succeeded" {
		t.Fatal("accepted task failed", finished, err)
	}
	log, err := os.ReadFile(n.logPath(accepted.ID))
	if err != nil || strings.Count(string(log), "brief") != 1 {
		t.Fatal("task logs were mixed or task executed twice", string(log), err)
	}
	other := "Build"
	if accepted.ID == other {
		other = "build"
	}
	if _, err := n.taskMethod(ctx, other+"-owner", "tasks.logs", model.JSON(map[string]string{"id": accepted.ID})); err == nil {
		t.Fatal("another owner read the accepted task's log")
	}
	cfg := n.Config
	for _, pruned := range []bool{false, true} {
		if pruned {
			if err := n.pruneTasks(time.Now().Add(2 * time.Hour)); err != nil {
				t.Fatal(err)
			}
		}
		if err := n.Close(); err != nil {
			t.Fatal(err)
		}
		n = openLocalNode(t, cfg)
		if _, err := n.startTaskContext(ctx, other+"-owner", execSpec(t, other, "brief")); err == nil || !strings.Contains(err.Error(), "case-fold") {
			t.Fatal("restart lost case-fold protection", pruned, err)
		}
		again, err := n.startTaskContext(ctx, accepted.Owner, finished.Spec)
		if err != nil || again.Pruned != pruned || again.Created != finished.Created {
			t.Fatal("exact-ID reconciliation changed", again, err)
		}
		if !pruned {
			got, err := os.ReadFile(n.logPath(accepted.ID))
			if err != nil || !bytes.Equal(got, log) {
				t.Fatal("rejected spelling changed logs", err)
			}
		}
	}
}

func TestCaseFoldedTaskIDsCoverPendingAcceptance(t *testing.T) {
	n := localNode(t, nil)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	settled := make(chan struct{})
	n.mu.Lock()
	n.leasePending, n.leaseSettled = true, settled
	n.mu.Unlock()
	// Always release the acceptance before node cleanup, including on failure.
	defer func() {
		n.mu.Lock()
		n.leasePending = false
		close(settled)
		n.mu.Unlock()
	}()
	done := make(chan error, 1)
	first, second := execSpec(t, "Pending", "brief"), execSpec(t, "pending", "brief")
	go func() {
		_, err := n.startTaskContext(ctx, "first", first)
		done <- err
	}()
	eventually(t, ctx, func() bool {
		n.mu.Lock()
		defer n.mu.Unlock()
		return n.accepting[first.ID] != nil
	})
	request, stop := context.WithCancel(ctx)
	stop()
	if _, err := n.startTaskContext(request, "second", second); err == nil || !strings.Contains(err.Error(), "case-fold") {
		t.Fatal("pending acceptance did not reserve its case-folded ID", err)
	}
	// Force the reserved submission to fail, then verify its spelling is free.
	cancel()
	if err := <-done; err == nil {
		t.Fatal("blocked acceptance ignored cancellation")
	}
	n.mu.Lock()
	n.leasePending = false
	n.mu.Unlock()
	task, err := n.startTaskContext(t.Context(), "second", second)
	if err != nil {
		t.Fatal("failed acceptance leaked its case-folded reservation", err)
	}
	if _, err := n.waitTask(t.Context(), task.Owner, task.ID, 5*time.Second); err != nil {
		t.Fatal(err)
	}
}

func TestStartupRejectsCaseFoldedTaskHistoryWithoutChangingFiles(t *testing.T) {
	for _, source := range []string{"task", "tombstone", "tasks"} {
		t.Run(source, func(t *testing.T) {
			n := localNode(t, nil)
			cfg := n.Config
			if err := n.Close(); err != nil {
				t.Fatal(err)
			}
			task := recentTask("Build", time.Now())
			task.State = "running"
			first := taskTombstone{ID: "Build", Owner: task.Owner, Spec: specDigest(task.Spec), State: "failed", Pruned: time.Now()}
			second := first
			second.ID = "build"
			second.Spec = specDigest(model.TaskSpec{ID: second.ID})
			journal := append(model.JSON(second), '\n')
			paths := []string{n.tombstonePath()}
			if source != "tombstone" {
				if err := store.Write(n.taskPath(task.ID), task); err != nil {
					t.Fatal(err)
				}
				paths = append(paths, n.taskPath(task.ID))
			}
			switch source {
			case "tasks":
				lower := *task
				lower.ID, lower.Spec.ID = "build", "build"
				if err := store.Write(n.taskPath(lower.ID), lower); err != nil {
					t.Fatal(err)
				}
				upperInfo, err := os.Stat(n.taskPath(task.ID))
				if err != nil {
					t.Fatal(err)
				}
				lowerInfo, err := os.Stat(n.taskPath(lower.ID))
				if err != nil {
					t.Fatal(err)
				}
				if os.SameFile(upperInfo, lowerInfo) {
					t.Skip("case-insensitive filesystem cannot store two distinct task records")
				}
				paths = append(paths, n.taskPath(lower.ID))
				journal = nil
			case "tombstone":
				journal = append(append(model.JSON(first), '\n'), journal...)
			}
			if err := store.Bytes(n.tombstonePath(), journal, 0600); err != nil {
				t.Fatal(err)
			}
			before := map[string][]byte{}
			for _, path := range paths {
				var err error
				before[path], err = os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
			}
			opened, err := New(cfg)
			if err == nil {
				_ = opened.Close()
				t.Fatal("opened conflicting case-folded history")
			}
			if !strings.Contains(err.Error(), "case-fold") {
				t.Fatal("missing collision recovery diagnostic", err)
			}
			for path, original := range before {
				got, err := os.ReadFile(path)
				if err != nil || !bytes.Equal(got, original) {
					t.Fatal("startup changed conflicting history", filepath.Base(path), err)
				}
			}
		})
	}
}

func TestCaseFoldedTaskIDsExpireWithCompletedTombstones(t *testing.T) {
	n := localNode(t, nil)
	cfg := n.Config
	if err := n.Close(); err != nil {
		t.Fatal(err)
	}
	stone := taskTombstone{ID: "Expired", Owner: "owner", Spec: specDigest(model.TaskSpec{ID: "Expired"}), State: "failed", Pruned: time.Now().Add(-tombstoneTTL - time.Hour)}
	stone.Checksum = stone.checksum()
	if err := store.Bytes(n.tombstonePath(), append(model.JSON(stone), '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	n = openLocalNode(t, cfg)
	task, err := n.startTaskContext(t.Context(), "new-owner", execSpec(t, "expired", "brief"))
	if err != nil {
		t.Fatal("expired tombstone retained its folded ID", err)
	}
	if finished, err := n.waitTask(t.Context(), task.Owner, task.ID, 5*time.Second); err != nil || finished.State != "succeeded" {
		t.Fatal("new task failed after expired tombstone", finished, err)
	}
}
