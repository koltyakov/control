package node

import (
	"context"
	"testing"
	"time"

	"github.com/koltyakov/control/internal/model"
)

func TestMaintenanceWaitsForAcceptedTasksAndBlocksNewWork(t *testing.T) {
	source, worker, _ := cluster(t, true, func(i int, cfg *Config) { cfg.MaxTasks = 1 })
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	for _, id := range []string{"maintenance-running", "maintenance-queued"} {
		call(t, source, "worker", "tasks.start", model.TaskSpec{ID: id, Capability: "exec.run", Args: model.JSON(executable(t, "sleep"))}, nil)
	}
	if worker.pauseForUpdate() {
		t.Fatal("paused with accepted unfinished tasks")
	}
	for _, id := range []string{"maintenance-running", "maintenance-queued"} {
		call(t, source, "worker", "tasks.cancel", map[string]string{"id": id}, nil)
	}
	eventually(t, ctx, func() bool { return worker.pauseForUpdate() })
	defer worker.work.Resume()
	call(t, source, "worker", "node.describe", map[string]any{}, nil)
	blocked, stop := context.WithTimeout(ctx, 50*time.Millisecond)
	err := source.Call(blocked, "worker", "files.write", map[string]any{"path": "blocked", "data": "YQ=="}, nil)
	stop()
	if err == nil {
		t.Fatal("accepted new work while reserved")
	}
	worker.work.Resume()
	call(t, source, "worker", "files.write", map[string]any{"path": "resumed", "data": "YQ=="}, nil)
}
