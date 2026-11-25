package node

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/koltyakov/control/internal/model"
)

// A coordinator never resolves select or each targets itself. A plan that
// reaches it unresolved, for example through the local API without a current
// client, is refused before any step starts, including steps with concrete
// targets and cleanup steps.
func TestWorkflowCoordinatorRejectsUnresolvedPlacement(t *testing.T) {
	t.Parallel()
	n, r := cleanupNode(t)
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	for _, mode := range []string{"select", "each"} {
		step := cleanupStep("placed", false, "")
		selector := &model.NodeSelector{Capability: "test.cleanup"}
		step.Placement = &model.WorkflowPlacement{}
		if mode == "select" {
			step.Placement.Select = selector
		} else {
			step.Placement.Each = selector
		}
		steps := []WorkflowStep{cleanupStep("local", false, ""), step, cleanupStep("cleanup", false, model.WorkflowWhenAlways, "local")}
		if _, err := runCleanupWorkflow(ctx, n, steps, 2); !errors.Is(err, model.ErrUnresolvedPlacement) {
			t.Fatalf("%s: coordinator accepted an unresolved plan: %v", mode, err)
		}
		var task model.Task
		err := n.Call(ctx, "", "tasks.start", model.TaskSpec{ID: "unresolved-" + mode, Capability: "workflow.run", Args: model.JSON(map[string]any{"steps": steps})}, &task)
		if err == nil {
			task, err = waitPlacementTask(ctx, n, task.ID)
		}
		if err == nil && task.State == "succeeded" {
			t.Fatalf("%s: unresolved workflow task succeeded", mode)
		}
	}
	if ran := r.ran(); len(ran) != 0 {
		t.Fatal("steps ran from an unresolved plan", ran)
	}
}

func waitPlacementTask(ctx context.Context, n *Node, id string) (model.Task, error) {
	for {
		var task model.Task
		if err := n.Call(ctx, "", "tasks.get", map[string]any{"id": id, "waitSeconds": 5}, &task); err != nil || task.Terminal() {
			return task, err
		}
	}
}
