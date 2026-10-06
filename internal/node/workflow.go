package node

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/koltyakov/control/internal/identity"
	"github.com/koltyakov/control/internal/model"
)

type WorkflowStep = model.WorkflowStep

func (n *Node) workflowRun(ctx context.Context, args json.RawMessage, e Execution) (any, error) {
	var workflow struct {
		Steps []WorkflowStep `json:"steps"`
	}
	if err := json.Unmarshal(args, &workflow); err != nil {
		return nil, err
	}
	if len(workflow.Steps) == 0 || len(workflow.Steps) > 100 {
		return nil, errors.New("workflow requires 1..100 steps")
	}
	known := map[string]bool{}
	for _, step := range workflow.Steps {
		if step.Name == "" || known[step.Name] {
			return nil, errors.New("workflow step names must be nonempty and unique")
		}
		known[step.Name] = true
	}
	for _, step := range workflow.Steps {
		if step.Task.Capability == "workflow.run" {
			return nil, errors.New("nested workflows are not supported")
		}
		for _, dep := range step.Needs {
			if !known[dep] {
				return nil, fmt.Errorf("unknown dependency %s", dep)
			}
		}
		for _, input := range step.Inputs {
			if !known[input.Step] || input.Artifact < 0 {
				return nil, errors.New("invalid workflow input")
			}
		}
	}
	// Validate the complete graph before submitting any side effects.
	ordered := []WorkflowStep{}
	sorted := map[string]bool{}
	for len(ordered) < len(workflow.Steps) {
		progress := false
		for _, step := range workflow.Steps {
			if sorted[step.Name] {
				continue
			}
			ready := true
			for _, dep := range step.Needs {
				ready = ready && sorted[dep]
			}
			for _, input := range step.Inputs {
				ready = ready && sorted[input.Step]
			}
			if ready {
				ordered = append(ordered, step)
				sorted[step.Name] = true
				progress = true
			}
		}
		if !progress {
			return nil, errors.New("workflow dependency cycle")
		}
	}
	results := map[string]model.Task{}
	for _, step := range ordered {
		for _, input := range step.Inputs {
			prior := results[input.Step]
			if input.Artifact >= len(prior.Artifacts) {
				return results, fmt.Errorf("missing artifact from %s", input.Step)
			}
			a := prior.Artifacts[input.Artifact]
			var granted model.Artifact
			if err := n.Call(ctx, a.Node, "artifacts.grant", map[string]any{"id": a.ID, "target": step.Target, "outputIndex": input.Artifact}, &granted); err != nil {
				return results, err
			}
			step.Task.Inputs = append(step.Task.Inputs, model.Input{Artifact: granted, Path: input.Path})
		}
		if step.Task.ID == "" {
			step.Task.ID = identity.NewID()
		}
		_, _ = fmt.Fprintf(e.Log, "step %s: target=%s task=%s\n", step.Name, step.Target, step.Task.ID)
		var task model.Task
		if err := n.Call(ctx, step.Target, "tasks.start", step.Task, &task); err != nil {
			return results, fmt.Errorf("submit %s: %w", step.Name, err)
		}
		task, err := n.WaitTask(ctx, step.Target, task.ID)
		if err != nil {
			cancelCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			_ = n.Call(cancelCtx, step.Target, "tasks.cancel", map[string]any{"id": step.Task.ID}, nil)
			cancel()
			return results, err
		}
		results[step.Name] = task
		if task.State != "succeeded" {
			return results, fmt.Errorf("step %s: %s", step.Name, task.Error)
		}
	}
	return results, nil
}
