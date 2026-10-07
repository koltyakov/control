package node

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/koltyakov/control/internal/identity"
	"github.com/koltyakov/control/internal/model"
)

type WorkflowStep = model.WorkflowStep

// maxWorkflowParallel bounds concurrently running workflow steps.
const maxWorkflowParallel = 16

func (n *Node) workflowRun(ctx context.Context, args json.RawMessage, e Execution) (any, error) {
	var workflow struct {
		Steps []WorkflowStep `json:"steps"`
		// MaxParallel runs up to this many ready steps at once. Zero or one keeps
		// strictly sequential execution in dependency order.
		MaxParallel int `json:"maxParallel"`
	}
	if err := json.Unmarshal(args, &workflow); err != nil {
		return nil, err
	}
	if len(workflow.Steps) == 0 || len(workflow.Steps) > 100 {
		return nil, errors.New("workflow requires 1..100 steps")
	}
	if workflow.MaxParallel == 0 {
		workflow.MaxParallel = 1
	}
	if workflow.MaxParallel < 1 || workflow.MaxParallel > maxWorkflowParallel {
		return nil, fmt.Errorf("maxParallel must be 1..%d", maxWorkflowParallel)
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
			if ready(step, sorted) {
				ordered = append(ordered, step)
				sorted[step.Name] = true
				progress = true
			}
		}
		if !progress {
			return nil, errors.New("workflow dependency cycle")
		}
	}
	// Launch ready steps in dependency order, up to the parallel limit. After a
	// failure no further step starts; steps already running finish on their own
	// rather than being cancelled mid-effect.
	type outcome struct {
		name string
		task model.Task
		err  error
	}
	results := map[string]model.Task{}
	succeeded, started := map[string]bool{}, map[string]bool{}
	outcomes := make(chan outcome)
	running := 0
	var failure error
	for {
		for _, step := range ordered {
			if failure != nil || running >= workflow.MaxParallel {
				break
			}
			if started[step.Name] || !ready(step, succeeded) {
				continue
			}
			started[step.Name] = true
			running++
			// Pass upstream results by value so steps never share the map.
			upstream := map[string]model.Task{}
			for _, input := range step.Inputs {
				upstream[input.Step] = results[input.Step]
			}
			go func() {
				task, err := n.runWorkflowStep(ctx, step, upstream, e.Log)
				outcomes <- outcome{step.Name, task, err}
			}()
		}
		if running == 0 {
			break
		}
		o := <-outcomes
		running--
		if o.task.ID != "" {
			results[o.name] = o.task
		}
		if o.err != nil {
			if failure == nil {
				failure = o.err
			}
			continue
		}
		succeeded[o.name] = true
	}
	return results, failure
}

func ready(step WorkflowStep, completed map[string]bool) bool {
	for _, dep := range step.Needs {
		if !completed[dep] {
			return false
		}
	}
	for _, input := range step.Inputs {
		if !completed[input.Step] {
			return false
		}
	}
	return true
}

func (n *Node) runWorkflowStep(ctx context.Context, step WorkflowStep, upstream map[string]model.Task, log io.Writer) (model.Task, error) {
	for _, input := range step.Inputs {
		prior := upstream[input.Step]
		if input.Artifact >= len(prior.Artifacts) {
			return model.Task{}, fmt.Errorf("missing artifact from %s", input.Step)
		}
		a := prior.Artifacts[input.Artifact]
		var granted model.Artifact
		if err := n.Call(ctx, a.Node, "artifacts.grant", map[string]any{"id": a.ID, "target": step.Target, "outputIndex": input.Artifact}, &granted); err != nil {
			return model.Task{}, err
		}
		step.Task.Inputs = append(step.Task.Inputs, model.Input{Artifact: granted, Path: input.Path})
	}
	if step.Task.ID == "" {
		step.Task.ID = identity.NewID()
	}
	_, _ = fmt.Fprintf(log, "step %s: target=%s task=%s\n", step.Name, step.Target, step.Task.ID)
	var task model.Task
	if err := n.Call(ctx, step.Target, "tasks.start", step.Task, &task); err != nil {
		return model.Task{}, fmt.Errorf("submit %s: %w", step.Name, err)
	}
	task, err := n.WaitTask(ctx, step.Target, task.ID)
	if err != nil {
		cancelCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		_ = n.Call(cancelCtx, step.Target, "tasks.cancel", map[string]any{"id": step.Task.ID}, nil)
		cancel()
		return model.Task{}, err
	}
	if task.State != "succeeded" {
		return task, fmt.Errorf("step %s: %s", step.Name, task.Error)
	}
	return task, nil
}
