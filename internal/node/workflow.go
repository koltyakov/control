package node

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"time"

	"github.com/koltyakov/control/internal/identity"
	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/transport"
)

type WorkflowStep = model.WorkflowStep

// Coordinator view of one step. Settled states are final and known.
type workflowStepState uint8

const (
	workflowPending workflowStepState = iota
	workflowRunning
	workflowSucceeded
	// workflowFailed is a terminal unsuccessful child, or a step that was
	// definitely never submitted.
	workflowFailed
	// workflowUnresolved means the child's submission or observation outcome is
	// unknown. It never settles during this run.
	workflowUnresolved
	// workflowNotStarted is an always step whose artifact input step did not
	// succeed, so its input cannot be staged.
	workflowNotStarted
)

// unresolvedStepError marks a child whose acceptance or terminal state is
// unknown. Cleanup steps depending on it, even transitively, never start.
type unresolvedStepError struct{ err error }

func (e unresolvedStepError) Error() string { return e.err.Error() }
func (e unresolvedStepError) Unwrap() error { return e.err }

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
	// failure no further success step starts; steps already running finish on
	// their own rather than being cancelled mid-effect. Always (cleanup) steps
	// start once their dependencies settle, whatever the outcome, unless the
	// workflow's own context ended. Every step, target, task ID and grant is
	// fixed by the validated plan; nothing is retried or resubmitted.
	type outcome struct {
		name string
		task model.Task
		err  error
	}
	byName := map[string]WorkflowStep{}
	state := map[string]workflowStepState{}
	for _, step := range ordered {
		byName[step.Name] = step
	}
	results := map[string]model.Task{}
	succeeded := map[string]bool{}
	outcomes := make(chan outcome, workflow.MaxParallel)
	active := map[string]WorkflowStep{}
	running := 0
	// failure is any step failure or the workflow's own stop; it ends normal
	// scheduling. primary is the first success-step failure (or stop) and is
	// reported before cleanup failures so they never mask it.
	var failure, primary error
	var cleanup []error
	fail := func(step WorkflowStep, err error) {
		if failure == nil {
			failure = err
		}
		if step.Always() {
			cleanup = append(cleanup, err)
		} else if primary == nil {
			primary = err
		}
	}
	stop := func() {
		if failure == nil {
			failure = ctx.Err()
		}
		if primary == nil {
			primary = ctx.Err()
		}
	}
	record := func(o outcome) {
		running--
		delete(active, o.name)
		if o.task.ID != "" {
			results[o.name] = o.task
		}
		var unresolved unresolvedStepError
		switch {
		case o.err == nil:
			state[o.name] = workflowSucceeded
			succeeded[o.name] = true
		case errors.As(o.err, &unresolved):
			state[o.name] = workflowUnresolved
		default:
			state[o.name] = workflowFailed
		}
		if o.err != nil {
			fail(byName[o.name], o.err)
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
	// settled reports whether a step's outcome is final and known: it finished,
	// or it will never start and nothing it depends on is unresolved.
	settled := func(memo map[string]bool) func(string) bool {
		var check func(string) bool
		check = func(name string) bool {
			if known, ok := memo[name]; ok {
				return known
			}
			result := false
			switch state[name] {
			case workflowSucceeded, workflowFailed, workflowNotStarted:
				result = true
			case workflowPending:
				// A pending always step may still start; a pending success step
				// never starts after a failure.
				step := byName[name]
				if !step.Always() && failure != nil {
					result = true
					for _, dep := range step.Dependencies() {
						if !check(dep) {
							result = false
							break
						}
					}
				}
			}
			memo[name] = result
			return result
		}
		return check
	}
	ready := func() []WorkflowStep {
		if ctx.Err() != nil {
			// Cancellation, timeout or revocation withdraws authority to run.
			// Report it when it prevented any step from starting.
			for _, step := range ordered {
				if state[step.Name] == workflowPending {
					stop()
					break
				}
			}
			return nil
		}
		// An always step whose dependencies settled but whose artifact input
		// step did not succeed is reported as not started. That settles it,
		// which can release later always steps, so repeat until stable.
		for changed := true; changed; {
			changed = false
			check := settled(map[string]bool{})
			for _, step := range ordered {
				if state[step.Name] != workflowPending || !step.Always() || !workflowDepsSettled(step, check) {
					continue
				}
				for _, input := range step.Inputs {
					if !succeeded[input.Step] {
						state[step.Name], changed = workflowNotStarted, true
						err := fmt.Errorf("cleanup step %s (task %s on %s) not started: its input from step %s is unavailable because that step did not succeed", step.Name, step.Task.ID, step.Target, input.Step)
						_, _ = fmt.Fprintf(e.Log, "%v\n", err)
						cleanup = append(cleanup, err)
						break
					}
				}
				if changed {
					break
				}
			}
		}
		check := settled(map[string]bool{})
		return workflowReadyWhere(ordered, func(step WorkflowStep) bool {
			if state[step.Name] != workflowPending {
				return false
			}
			if step.Always() {
				return workflowDepsSettled(step, check)
			}
			return failure == nil && step.Ready(succeeded)
		})
	}
	for {
		drain()
		var candidates []WorkflowStep
		if running < workflow.MaxParallel {
			candidates = ready()
		}
		var reports map[string]*model.NodeConcurrency
		if workflow.MaxParallel > 1 && capacityCanReorder(candidates) {
			reports = workflowCapacityReports(ctx, candidates, n.workflowCapacity)
		}
		for len(candidates) > 0 && running < workflow.MaxParallel {
			// A failure may arrive during capacity discovery. Observe it before
			// starting more work, while allowing accepted children to finish.
			drain()
			if ctx.Err() != nil {
				stop()
				break
			}
			if failure != nil {
				// Settled dependencies stay settled, so ready cleanup steps
				// remain eligible; success steps do not.
				candidates = slices.DeleteFunc(candidates, func(step WorkflowStep) bool { return !step.Always() })
				if len(candidates) == 0 {
					break
				}
			}
			index := 0
			if reports != nil {
				index = workflowNextStep(candidates, reports, active)
			}
			step := candidates[index]
			candidates = append(candidates[:index], candidates[index+1:]...)
			state[step.Name] = workflowRunning
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
			if len(ready()) > 0 {
				continue
			}
			break
		}
		record(<-outcomes)
	}
	for _, step := range ordered {
		if !step.Always() || state[step.Name] != workflowPending {
			continue
		}
		reason := "its dependencies did not settle"
		if ctx.Err() != nil {
			reason = fmt.Sprintf("the workflow was cancelled, timed out or lost its authority (%v)", ctx.Err())
		} else if blocker := workflowUnresolvedUpstream(step, byName, state); blocker != "" {
			reason = fmt.Sprintf("the outcome of step %s is unknown; reconcile it", blocker)
		}
		err := fmt.Errorf("cleanup step %s (task %s on %s) not started: %s", step.Name, step.Task.ID, step.Target, reason)
		_, _ = fmt.Fprintf(e.Log, "%v\n", err)
		cleanup = append(cleanup, err)
	}
	return results, errors.Join(append([]error{primary}, cleanup...)...)
}

func workflowDepsSettled(step WorkflowStep, settled func(string) bool) bool {
	for _, dep := range step.Dependencies() {
		if !settled(dep) {
			return false
		}
	}
	return true
}

// workflowUnresolvedUpstream names the first unresolved step a pending step
// depends on, directly or transitively.
func workflowUnresolvedUpstream(step WorkflowStep, byName map[string]WorkflowStep, state map[string]workflowStepState) string {
	seen := map[string]bool{}
	queue := step.Dependencies()
	for len(queue) > 0 {
		name := queue[0]
		queue = queue[1:]
		if seen[name] {
			continue
		}
		seen[name] = true
		if state[name] == workflowUnresolved {
			return name
		}
		if state[name] == workflowPending {
			queue = append(queue, byName[name].Dependencies()...)
		}
	}
	return ""
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
		if ctx.Err() != nil {
			// The worker may have accepted the child before cancellation
			// interrupted its reply. Cancel by the stable ID; an unknown ID
			// is harmless and nothing is resubmitted.
			n.cancelWorkflowChild(ctx, step.Target, step.Task.ID)
		}
		// Never resubmit: the outcome may be uncertain. The stable ID lets the
		// orchestrator reconcile it. Only a request that was never sent is a
		// known outcome, as is an explicit refusal before acceptance; cleanup
		// steps never start behind an uncertain one.
		err = fmt.Errorf("submit %s (task %s on %s): %w", step.Name, step.Task.ID, step.Target, err)
		var rpc *model.RPCError
		if !errors.As(err, &rpc) || (rpc.Outcome != "not_sent" && rpc.Outcome != model.OutcomeNotAccepted) {
			err = unresolvedStepError{err}
		}
		return model.Task{}, err
	}
	if task.ID == "" {
		task.ID = step.Task.ID
	}
	observed, err := n.observeWorkflowChild(ctx, step, task, log)
	if err != nil {
		if ctx.Err() != nil {
			// Workflow cancellation, timeout or grant revocation stops its
			// children. Report the last observation with the child's identity.
			n.cancelWorkflowChild(ctx, step.Target, task.ID)
			return observed, err
		}
		// A permanent observation failure says nothing about the child. Keep
		// its last observed (non-terminal) record in the result and do not
		// cancel healthy accepted work. The workflow stops scheduling new steps.
		_, _ = fmt.Fprintf(log, "step %s: task %s on %s is unresolved: %v\n", step.Name, task.ID, step.Target, err)
		return observed, unresolvedStepError{fmt.Errorf("step %s: task %s on %s is unresolved and was not cancelled; reconcile it before retrying: %w", step.Name, task.ID, step.Target, err)}
	}
	if observed.State != "succeeded" {
		return observed, fmt.Errorf("step %s: %s", step.Name, observed.Error)
	}
	return observed, nil
}

// cancelWorkflowChild makes one bounded best-effort cancel after the
// workflow's own context ended. Cancellation is idempotent.
func (n *Node) cancelWorkflowChild(ctx context.Context, target, id string) {
	cancelCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	_ = n.Call(cancelCtx, target, "tasks.cancel", map[string]any{"id": id}, nil)
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
