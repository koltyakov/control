package node

import (
	"bytes"
	"context"
	"os"
	"testing"
	"time"

	"github.com/koltyakov/control/internal/model"
)

func TestColdTaskReadDuringPrune(t *testing.T) {
	n := localNode(t, func(c *Config) { c.TaskRetentionHours = 1 })
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	spec := model.TaskSpec{ID: "cold", Capability: "files.list", Args: model.JSON(map[string]string{"path": "."})}
	if _, err := n.startTaskContext(ctx, "owner", spec); err != nil {
		t.Fatal(err)
	}
	got, err := n.waitTask(ctx, "owner", spec.ID, time.Minute)
	if err != nil || got.State != "succeeded" || len(got.Result) == 0 {
		t.Fatal(got, err)
	}
	cfg := n.Config
	if err = n.Close(); err != nil {
		t.Fatal(err)
	}
	n = openLocalNode(t, cfg)
	// Recovery kept the metadata and digest, but no copy of either payload.
	if n.coldTasks[spec.ID] == nil || n.tasks[spec.ID].Spec.Args != nil || n.tasks[spec.ID].Result != nil {
		t.Fatal("recovered payload resident")
	}
	if _, err = n.ownedTask("other", spec.ID); err == nil {
		t.Fatal("read another owner's terminal payload")
	}
	duplicate, err := n.startTaskContext(ctx, "owner", spec)
	if err != nil || !bytes.Equal(model.JSON(duplicate), model.JSON(got)) {
		t.Fatal("duplicate did not recover full outcome", err)
	}
	n.mu.Lock()
	reader := n.payloadLocked(n.tasks[spec.ID])
	n.mu.Unlock()
	done := make(chan error, 1)
	go func() { done <- n.pruneTasks(time.Now().Add(2 * time.Hour)) }()
	eventually(t, ctx, func() bool { n.mu.Lock(); defer n.mu.Unlock(); return n.tombstones[spec.ID].ID != "" })
	after, err := n.ownedTask("owner", spec.ID)
	if err != nil || !after.Pruned {
		t.Fatal("new reader did not see tombstone", err)
	}
	read, err := reader.load()
	if err != nil || !bytes.Equal(read.Result, got.Result) {
		t.Fatal("prune raced admitted reader", err)
	}
	if err = <-done; err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(n.taskPath(spec.ID)); !os.IsNotExist(err) {
		t.Fatal("record not removed", err)
	}
}
