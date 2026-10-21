package node

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/store"
)

func TestCorruptInteriorTombstoneFailsClosed(t *testing.T) {
	n := localNode(t, nil)
	task := recentTask("protected-id", time.Now())
	stone := taskTombstone{ID: task.ID, Owner: task.Owner, Spec: specDigest(task.Spec), Capability: task.Spec.Capability, State: task.State, Pruned: time.Now()}
	stone.Checksum = stone.checksum()
	valid := append(model.JSON(stone), '\n')
	for _, bad := range [][]byte{[]byte("{broken}\n"), bytes.Replace(valid, []byte("protected-id"), []byte("tampered-id"), 1)} {
		journal := append(append(append([]byte{}, valid...), bad...), valid...)
		if err := store.Bytes(n.tombstonePath(), journal, 0600); err != nil {
			t.Fatal(err)
		}
		if err := n.loadTombstones(); err == nil {
			t.Fatal("accepted corrupted interior record")
		}
		after, err := os.ReadFile(n.tombstonePath())
		if err != nil || !bytes.Equal(after, journal) {
			t.Fatal("damaged journal was changed", err)
		}
	}
}

func TestTaskCleanupRetriesAcrossRestart(t *testing.T) {
	n := localNode(t, func(c *Config) { c.TaskRetentionHours = 1 })
	task := recentTask("cleanup", time.Now().Add(-2*time.Hour))
	n.tasks[task.ID] = task
	if err := store.Write(n.taskPath(task.ID), task); err != nil {
		t.Fatal(err)
	}
	// A nonempty directory where a log file belongs makes removal fail on all OSes.
	if err := os.MkdirAll(n.logPath(task.ID), 0700); err != nil {
		t.Fatal(err)
	}
	blocker := filepath.Join(n.logPath(task.ID), "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := n.pruneTasks(time.Now()); err == nil {
		t.Fatal("cleanup failure hidden")
	}
	if !n.tombstones[task.ID].CleanupPending {
		t.Fatal("lost cleanup intent")
	}
	cfg := n.Config
	_ = n.Close()
	n = openLocalNode(t, cfg)
	if !n.tombstones[task.ID].CleanupPending {
		t.Fatal("restart lost cleanup intent")
	}
	if err := os.Remove(blocker); err != nil {
		t.Fatal(err)
	}
	if err := n.pruneTasks(time.Now()); err != nil {
		t.Fatal(err)
	}
	if n.tombstones[task.ID].CleanupPending {
		t.Fatal("cleanup did not finish")
	}
	if _, err := os.Stat(n.logPath(task.ID)); !os.IsNotExist(err) {
		t.Fatal("log remains", err)
	}
}

func TestTaskPagesAndArtifactResults(t *testing.T) {
	n := localNode(t, nil)
	large := model.JSON(strings.Repeat("x", 2<<20))
	for i := range 205 {
		id := fmt.Sprintf("task-%03d", i)
		task := recentTask(id, time.Now())
		task.Result = large
		n.tasks[id] = task
	}
	hidden := recentTask("task-hidden", time.Now())
	hidden.Owner = "other"
	n.tasks[hidden.ID] = hidden
	cursor, count := "", 0
	for {
		page, err := n.taskPage(t.Context(), "owner", cursor, 50)
		if err != nil {
			t.Fatal(err)
		}
		if len(model.JSON(page)) > 100<<10 {
			t.Fatal("page leaked bulk data")
		}
		for _, task := range page.Tasks {
			if task.ID <= cursor || task.ID == hidden.ID {
				t.Fatal("page ordering or ownership", task)
			}
			cursor = task.ID
			count++
		}
		if page.NextCursor == "" {
			break
		}
		if page.NextCursor != cursor {
			t.Fatal("bad cursor")
		}
	}
	if count != 205 {
		t.Fatal("lost tasks", count)
	}
	if _, err := n.taskMethod(t.Context(), "owner", "tasks.list", model.JSON(map[string]any{})); err == nil {
		t.Fatal("unbounded legacy list")
	}
	ctx := context.WithValue(t.Context(), activityContextKey{}, activityContext{owner: "owner", task: "task-000"})
	result := strings.Repeat("z", 2<<20)
	n.finishTask("task-000", ctx, result, nil, nil)
	task := n.tasks["task-000"]
	if task.ResultArtifact == nil || len(task.Result) != 0 || len(task.Artifacts) != 0 {
		t.Fatal("large result not separated from outputs")
	}
	var out bytes.Buffer
	if err := n.Download(t.Context(), *task.ResultArtifact, 0, &out); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out.Bytes(), model.JSON(result)) {
		t.Fatal("result content changed")
	}
}

func TestArtifactBudgetPinsAndStagingCleanup(t *testing.T) {
	n := localNode(t, func(c *Config) { c.MaxArtifactBytes = 1024; c.PartialRetentionHours = 1 })
	if _, err := n.importArtifact(t.Context(), strings.NewReader(strings.Repeat("x", 2048)), "too-big"); err == nil {
		t.Fatal("quota exceeded")
	}
	if n.artifactUsed != 0 {
		t.Fatal("failed import leaked bytes", n.artifactUsed)
	}
	a, err := n.importArtifact(t.Context(), strings.NewReader("content"), "small")
	if err != nil {
		t.Fatal(err)
	}
	unpin, err := n.pinArtifact(t.Context(), a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = n.artifactMethod(t.Context(), n.Identity.ID, "artifacts.delete", model.JSON(map[string]string{"id": a.ID})); err == nil {
		t.Fatal("deleted active reader")
	}
	unpin()
	if _, err = n.artifactMethod(t.Context(), n.Identity.ID, "artifacts.delete", model.JSON(map[string]string{"id": a.ID})); err != nil {
		t.Fatal(err)
	}
	if n.artifactUsed != 0 {
		t.Fatal("deletion failed accounting", n.artifactUsed)
	}
	orphan := filepath.Join(n.Config.DataDir, "artifacts", ".import-abandoned")
	if err = os.WriteFile(orphan, []byte("stale"), 0600); err != nil {
		t.Fatal(err)
	}
	n.artifactUsed += 5
	old := time.Now().Add(-2 * time.Hour)
	if err = os.Chtimes(orphan, old, old); err != nil {
		t.Fatal(err)
	}
	if err = n.pruneArtifacts(t.Context(), time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(orphan); !os.IsNotExist(err) || n.artifactUsed != 0 {
		t.Fatal("orphan not reclaimed", err, n.artifactUsed)
	}
}

func TestArtifactCleanupReclaimsRedundantPartialOnly(t *testing.T) {
	n := localNode(t, func(c *Config) { c.PartialRetentionHours = 1 })
	a, err := n.importArtifact(t.Context(), strings.NewReader("retained"), "output")
	if err != nil {
		t.Fatal(err)
	}
	task := recentTask("retained-task", time.Now())
	task.Artifacts = []model.Artifact{a}
	n.tasks[task.ID] = task
	path := n.artifactPath(a.ID) + ".partial"
	if err = os.WriteFile(path, []byte("partial"), 0600); err != nil {
		t.Fatal(err)
	}
	n.artifactUsed += 7
	old := time.Now().Add(-2 * time.Hour)
	if err = os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	if err = n.pruneArtifacts(t.Context(), time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("redundant staging retained", err)
	}
	if _, err = os.Stat(n.artifactPath(a.ID)); err != nil || n.artifactUsed != a.Size {
		t.Fatal("published blob lost", err, n.artifactUsed)
	}
}
