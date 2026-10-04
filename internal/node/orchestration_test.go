package node

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/koltyakov/control/internal/model"
)

func TestExclusiveLeasesAndQueue(t *testing.T) {
	source, _, consumer := cluster(t, true, func(i int, cfg *Config) { cfg.MaxTasks = 1 })
	var lease model.Lease
	call(t, source, "worker", "leases.acquire", map[string]any{"ttlSeconds": 60}, &lease)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := consumer.Call(ctx, "worker", "leases.acquire", map[string]any{}, nil); err == nil {
		t.Fatal("second lease succeeded")
	}
	spec := model.TaskSpec{ID: "leased", Capability: "exec.run", Args: model.JSON(executable(t, "sleep"))}
	if err := source.Call(ctx, "worker", "tasks.start", spec, nil); err == nil {
		t.Fatal("task without lease succeeded")
	}
	spec.LeaseID = lease.ID
	if err := consumer.Call(ctx, "worker", "tasks.start", spec, nil); err == nil {
		t.Fatal("other owner used lease")
	}
	var task model.Task
	call(t, source, "worker", "tasks.start", spec, &task)
	if err := source.Call(ctx, "worker", "leases.release", map[string]any{"id": lease.ID}, nil); err == nil {
		t.Fatal("released busy lease")
	}
	call(t, source, "worker", "tasks.cancel", map[string]any{"id": task.ID}, nil)
	if _, err := source.WaitTask(ctx, "worker", task.ID); err != nil {
		t.Fatal(err)
	}
	call(t, source, "worker", "leases.renew", map[string]any{"id": lease.ID}, nil)
	call(t, source, "worker", "leases.release", map[string]any{"id": lease.ID}, nil)
}

func TestWorkflowLocalStepDoesNotDeadlock(t *testing.T) {
	source, _, _ := cluster(t, true, func(i int, cfg *Config) { cfg.MaxTasks = 1 })
	steps := []WorkflowStep{
		{Name: "remote", Target: "worker", Needs: []string{"local"}, Task: model.TaskSpec{Capability: "files.list", Args: json.RawMessage(`{}`)}},
		{Name: "local", Target: "source", Task: model.TaskSpec{Capability: "files.list", Args: json.RawMessage(`{}`)}},
	}
	var task model.Task
	call(t, source, "source", "tasks.start", model.TaskSpec{ID: "workflow", Capability: "workflow.run", Args: model.JSON(map[string]any{"steps": steps})}, &task)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	finished, err := source.WaitTask(ctx, "source", task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if finished.State != "succeeded" {
		t.Fatalf("workflow: %+v", finished)
	}
	var result map[string]model.Task
	if err = json.Unmarshal(finished.Result, &result); err != nil {
		t.Fatal(err)
	}
	if result["local"].Created.After(result["remote"].Created) {
		t.Fatal("dependency order ignored")
	}
	steps[1].Needs = []string{"remote"}
	if err = source.Call(ctx, "source", "workflow.run", map[string]any{"steps": steps}, nil); err == nil {
		t.Fatal("dependency cycle accepted")
	}
}

func TestNodeDirectoryLock(t *testing.T) {
	source, _, _ := cluster(t, true, nil)
	if duplicate, err := New(source.Config); err == nil {
		_ = duplicate.Close()
		t.Fatal("same data directory opened twice")
	}
}

func TestTaskSurvivesSubmitterDisconnect(t *testing.T) {
	source, worker, _ := cluster(t, true, nil)
	var task model.Task
	call(t, source, "worker", "tasks.start", model.TaskSpec{ID: "survive-disconnect", Capability: "exec.run", Args: model.JSON(executable(t, "delayed"))}, &task)
	if err := source.Close(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		nodes, err := worker.Peer.Nodes(ctx)
		if err != nil {
			t.Fatal(err)
		}
		offline := false
		for _, peer := range nodes {
			if peer.ID == source.Identity.ID && !peer.Online {
				offline = true
			}
		}
		if offline {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-ticker.C:
		}
	}
	reconnected, err := New(source.Config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reconnected.Close() })
	if err = reconnected.Start(ctx); err != nil {
		t.Fatal(err)
	}
	finished, err := reconnected.WaitTask(ctx, "worker", task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if finished.State != "succeeded" {
		t.Fatalf("task interrupted by caller disconnect: %+v", finished)
	}
	if reconnected.Identity.ID != source.Identity.ID {
		t.Fatal("identity changed on restart")
	}
}

func TestNamedAndGlobalPermissionsCombine(t *testing.T) {
	_, worker, consumer := cluster(t, true, func(i int, cfg *Config) {
		if i == 0 {
			cfg.Allow = map[string][]string{"*": {"node.describe"}, "worker": {"files.list"}}
		}
	})
	call(t, worker, "source", "files.list", map[string]any{}, nil)
	call(t, consumer, "source", "node.describe", map[string]any{}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := consumer.Call(ctx, "source", "files.list", map[string]any{}, nil); err == nil {
		t.Fatal("unlisted caller inherited another node's permission")
	}
}
