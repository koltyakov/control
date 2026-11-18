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
	"github.com/koltyakov/control/internal/transport"
)

type WorkflowStep = model.WorkflowStep

func (n *Node) workflowRun(ctx context.Context, args json.RawMessage, e Execution) (any, error) {
	var workflow model.WorkflowPlan
	if err := json.Unmarshal(args, &workflow); err != nil {
		return nil, err
	}
	ordered, err := workflow.Ordered()
	if err != nil {
		return nil, err
	}
	if workflow.MaxParallel == 0 {
		workflow.MaxParallel = 1
	}
	// Reorder only unstarted ready steps, up to the parallel limit. After a
	// failure no further step starts; steps already running finish on their own
	// rather than being cancelled mid-effect.
	type outcome struct {
		name string
		task model.Task
		err  error
	}
	results := map[string]model.Task{}
	succeeded, started := map[string]bool{}, map[string]bool{}
	outcomes := make(chan outcome, workflow.MaxParallel)
	active := map[string]WorkflowStep{}
	running := 0
	var failure error
	record := func(o outcome) {
		running--
		delete(active, o.name)
		if o.task.ID != "" {
			results[o.name] = o.task
		}
		if o.err != nil {
			if failure == nil {
				failure = o.err
			}
		} else {
			succeeded[o.name] = true
		}
	}
	drain := func() {
		for {
			select {
			case o := <-outcomes:
				record(o)
			default:
				return
			}
		}
	}
	for {
		drain()
		var candidates []WorkflowStep
		if failure == nil && running < workflow.MaxParallel {
			candidates = workflowReadySteps(ordered, started, succeeded)
		}
		var reports map[string]*model.NodeConcurrency
		if workflow.MaxParallel > 1 && capacityCanReorder(candidates) {
			reports = workflowCapacityReports(ctx, candidates, n.workflowCapacity)
		}
		for len(candidates) > 0 && running < workflow.MaxParallel {
			// A failure may arrive during capacity discovery. Observe it before
			// starting more work, while allowing accepted children to finish.
			drain()
			if ctx.Err() != nil && failure == nil {
				failure = ctx.Err()
			}
			if failure != nil {
				break
			}
			index := 0
			if reports != nil {
				index = workflowNextStep(candidates, reports, active)
			}
			step := candidates[index]
			candidates = append(candidates[:index], candidates[index+1:]...)
			started[step.Name] = true
			active[step.Name] = step
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
			if failure == nil && len(started) < len(ordered) {
				continue
			}
			break
		}
		record(<-outcomes)
	}
	return results, failure
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
		// Never resubmit: the outcome may be uncertain. The stable ID lets the
		// orchestrator reconcile it.
		return model.Task{}, fmt.Errorf("submit %s (task %s on %s): %w", step.Name, step.Task.ID, step.Target, err)
	}
	if task.ID == "" {
		task.ID = step.Task.ID
	}
	observed, err := n.observeWorkflowChild(ctx, step, task, log)
	if err != nil {
		if ctx.Err() != nil {
			// Workflow cancellation, timeout or grant revocation stops its
			// children. Report the last observation with the child's identity.
			cancelCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			_ = n.Call(cancelCtx, step.Target, "tasks.cancel", map[string]any{"id": task.ID}, nil)
			cancel()
			return observed, err
		}
		// A permanent observation failure says nothing about the child. Keep
		// its last observed (non-terminal) record in the result and do not
		// cancel healthy accepted work. The workflow stops scheduling new steps.
		_, _ = fmt.Fprintf(log, "step %s: task %s on %s is unresolved: %v\n", step.Name, task.ID, step.Target, err)
		return observed, fmt.Errorf("step %s: task %s on %s is unresolved and was not cancelled; reconcile it before retrying: %w", step.Name, task.ID, step.Target, err)
	}
	if observed.State != "succeeded" {
		return observed, fmt.Errorf("step %s: %s", step.Name, observed.Error)
	}
	return observed, nil
}

// observeWorkflowChild waits for an accepted child to become terminal. Only
// tasks.get is repeated, and only after a locally detected stream or network
// failure; tasks.start and grants are never replayed. The workflow's own
// context and deadlines bound the wait. On error it returns the last
// successful observation, which keeps the child's identity.
func (n *Node) observeWorkflowChild(ctx context.Context, step WorkflowStep, last model.Task, log io.Writer) (model.Task, error) {
	target := step.Target
	const interval = 100 * time.Millisecond
	backoff := interval
	for {
		started := time.Now()
		var task model.Task
		err := n.Call(ctx, target, "tasks.get", map[string]any{"id": last.ID, "waitSeconds": int(maxTaskWait/time.Second) / 2}, &task)
		delay := interval - time.Since(started)
		switch {
		case err == nil && task.ID != last.ID:
			return last, fmt.Errorf("status for task %s returned task %q", last.ID, task.ID)
		case err == nil:
			last, backoff = task, interval
			if task.Terminal() {
				return task, nil
			}
		case transientObservation(ctx, err):
			_, _ = fmt.Fprintf(log, "step %s: status read for task %s on %s failed; retrying: %v\n", step.Name, last.ID, target, err)
			delay = backoff
			backoff = min(2*backoff, 5*time.Second)
		default:
			return last, err
		}
		if delay > 0 {
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return last, ctx.Err()
			case <-timer.C:
			}
		} else if err := ctx.Err(); err != nil {
			return last, err
		}
	}
}

// transientObservation reports whether a status read failed locally in a way
// that may clear on reconnect, so repeating the read-only request is
// meaningful. Delivery outcome alone is not enough. Errors decoded from a
// remote reply (auth, ownership, validation, protocol or missing-task errors)
// carry no local cause and are permanent. The caller's own cancellation or
// deadline is permanent too. Connection-specific classification of the local
// cause belongs to the transport.
func transientObservation(ctx context.Context, err error) bool {
	if err == nil || ctx.Err() != nil {
		return false
	}
	var rpc *model.RPCError
	if !errors.As(err, &rpc) || rpc.Cause == nil {
		return false
	}
	return transport.RetryableDelivery(rpc.Cause)
}
