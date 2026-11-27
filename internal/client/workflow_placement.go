package client

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/koltyakov/control/internal/identity"
	"github.com/koltyakov/control/internal/model"
)

// maxWorkflowSteps matches the coordinator's plan limit, which applies to the
// resolved plan after fan-out.
const maxWorkflowSteps = 100

// ResolvedStep is one concrete step of a workflow plan whose select or each
// targets the client resolved before submission. Target is the machine ID the
// coordinator receives; Node is its name at resolution. From names the
// original step of a fan-out instance.
type ResolvedStep struct {
	Name   string `json:"name"`
	From   string `json:"from,omitempty"`
	Target string `json:"target"`
	Node   string `json:"node,omitempty"`
	TaskID string `json:"taskId"`
	When   string `json:"when,omitempty"`
}

// TaskResolution is a task ready to submit, or the task that already holds
// its ID.
type TaskResolution struct {
	// Spec is the specification to submit. With Existing it is the retained
	// specification, for display only.
	Spec model.TaskSpec
	// Steps describes a plan whose select/each targets were resolved, or the
	// retained plan of Existing. It is nil for any other task.
	Steps []ResolvedStep
	// Existing is the coordinator's task that already holds this ID with a
	// matching request digest. It is returned read-only: nothing is submitted
	// and no grant is prepared, because its plan is the coordinator's report,
	// not the caller's request.
	Existing *model.Task
}

// ResolveTask resolves the select and each step targets of a workflow.run
// task into a concrete plan with stable child task IDs and a digest of the
// original request. Other tasks, and plans without selectors, are returned
// unchanged with no steps.
//
// A task ID that the coordinator already holds is never resolved again and
// never resubmitted. Its retained task is returned as Existing only when its
// recorded request digest matches this request. A different digest, or none,
// as recorded by earlier clients and by plans that were concrete when
// submitted, is refused with nothing sent; tasks.get still reads the task.
// Resolution creates nothing: it only reads the coordinator's task and the
// fleet directory before any grant is prepared.
func (c Client) ResolveTask(ctx context.Context, target string, spec model.TaskSpec) (TaskResolution, error) {
	r := TaskResolution{Spec: spec}
	if spec.Capability != "workflow.run" {
		return r, nil
	}
	var plan model.WorkflowPlan
	if err := json.Unmarshal(spec.Args, &plan); err != nil {
		return r, notSent(fmt.Errorf("invalid workflow plan; nothing was submitted: %w", err))
	}
	if !plan.HasPlacement() {
		return r, nil
	}
	if spec.ID == "" {
		spec.ID = identity.NewID()
	}
	r.Spec = spec
	if !model.TaskIDPattern.MatchString(spec.ID) {
		return r, notSent(fmt.Errorf("invalid task ID %q: use 1-128 letters, digits, '_' or '-'; nothing was submitted", spec.ID))
	}
	if _, err := checkPlacements(plan); err != nil {
		return r, notSent(err)
	}
	digest, err := workflowRequestDigest(spec, plan)
	if err != nil {
		return r, notSent(fmt.Errorf("invalid workflow plan; nothing was submitted: %w", err))
	}
	retained, found, err := c.retainedWorkflow(ctx, target, spec.ID)
	if err != nil {
		return r, notSent(err)
	}
	if found {
		var held model.WorkflowPlan
		if err = json.Unmarshal(retained.Spec.Args, &held); err != nil || held.HasPlacement() {
			return r, notSent(fmt.Errorf("task %s holds no resolved workflow plan; choose a new task ID", spec.ID))
		}
		if held.RequestDigest == "" {
			return r, notSent(fmt.Errorf("cannot verify original workflow request for task %s: it has no request digest; inspect it with control task get %q %s", spec.ID, target, spec.ID))
		}
		if held.RequestDigest != digest {
			return r, notSent(fmt.Errorf("task ID %s already holds a different workflow request; inspect it with control task get %q %s", spec.ID, target, spec.ID))
		}
		return TaskResolution{Spec: retained.Spec, Steps: describeResolvedPlan(held, nil, nil), Existing: &retained}, nil
	}
	resolved, steps, err := c.resolveWorkflowPlan(ctx, plan, spec.ID)
	if err != nil {
		return r, notSent(err)
	}
	resolved.RequestDigest = digest
	r.Spec.Args, r.Steps = model.JSON(resolved), steps
	return r, nil
}

// workflowRequestDigest identifies an unresolved workflow task request. It is
// "sha256:" and the hex SHA-256 of the canonical JSON of the whole task
// specification, with its ID, as the client decodes it: object keys sorted,
// number literals kept, tags normalized, and any caller-supplied digest
// removed. Re-encoding the same request yields the same digest; any change to
// steps, selectors, child tasks, timeout, inputs, outputs, lease or tags
// yields another.
func workflowRequestDigest(spec model.TaskSpec, plan model.WorkflowPlan) (string, error) {
	plan.RequestDigest = ""
	// Normalize the copy used for comparison, including tags on child tasks.
	plan.Steps = slices.Clone(plan.Steps)
	for i := range plan.Steps {
		child := &plan.Steps[i]
		tags, err := model.NormalizeTaskTags(child.Task.Tags)
		if err != nil {
			return "", fmt.Errorf("step %s: %w", child.Name, err)
		}
		child.Task.Tags = tags
	}
	spec.Args = model.JSON(plan)
	tags, err := model.NormalizeTaskTags(spec.Tags)
	if err != nil {
		return "", err
	}
	spec.Tags = tags
	decoder := json.NewDecoder(bytes.NewReader(model.JSON(spec)))
	decoder.UseNumber()
	var canonical any
	if err := decoder.Decode(&canonical); err != nil {
		return "", err
	}
	sum := sha256.Sum256(model.JSON(canonical))
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// StartResolvedTask submits a resolution from ResolveTask. A resolution that
// found an existing task submits nothing, prepares no grant and returns that
// task, as a duplicate submission of an accepted task would.
func (c Client) StartResolvedTask(ctx context.Context, target string, r TaskResolution, result any) (string, error) {
	if r.Existing != nil {
		return r.Spec.ID, existingTaskResult(*r.Existing, result)
	}
	spec := r.Spec
	if spec.ID == "" {
		spec.ID = identity.NewID()
	}
	if !model.TaskIDPattern.MatchString(spec.ID) {
		return "", model.OperationError(fmt.Errorf("invalid task ID %q: use 1-128 letters, digits, '_' or '-'; nothing was submitted", spec.ID), "not_sent")
	}
	// Refuse unsupported tags here, unwrapped, because nothing was sent.
	if err := c.requireTaskTags(ctx, target, "tasks.start", spec); err != nil {
		return "", err
	}
	ctx = context.WithValue(ctx, taskTagsCheckedKey{}, true)
	if err := c.Call(ctx, target, "tasks.start", spec, result); err != nil {
		return spec.ID, &TaskSubmitError{ID: spec.ID, Err: err}
	}
	return spec.ID, nil
}

// existingTaskResult decodes a read-only answer about an already accepted
// task into a call result.
func existingTaskResult(value, result any) error {
	if result == nil {
		return nil
	}
	return json.Unmarshal(model.JSON(value), result)
}

// existingTaskCheck answers a preflight of a request whose task already
// exists, as a coordinator reports a duplicate ID, without sending anything.
func existingTaskCheck(task model.Task) model.TaskCheck {
	return model.TaskCheck{Advisory: true, OK: true, TaskID: task.ID, Note: model.TaskCheckAdvisory, Checks: []model.TaskCheckItem{{
		Name: "id", Status: model.TaskCheckPass,
		Message: fmt.Sprintf("task %s already exists for this workflow request (state %s); submission returns it without resubmitting", task.ID, task.State),
	}}}
}

// notSent reports a resolution failure. Nothing was submitted, whatever the
// delivery outcome of the discovery request that failed.
func notSent(err error) error {
	message := err.Error()
	if !strings.Contains(message, "nothing was submitted") {
		message += "; nothing was submitted"
	}
	return &model.RPCError{Code: "operation_failed", Outcome: "not_sent", Message: message, Cause: err}
}

// retainedWorkflow reads an earlier submission of this task ID. Only the
// destination's own not-found reply proves there is none; any other failure
// stops resolution, because fresh resolution could choose other machines.
func (c Client) retainedWorkflow(ctx context.Context, target, id string) (model.Task, bool, error) {
	var task model.Task
	err := c.Call(ctx, target, "tasks.get", map[string]any{"id": id}, &task)
	if err != nil {
		var rpc *model.RPCError
		if errors.As(err, &rpc) && errors.Is(err, model.ErrReplied) && rpc.Message == workerTaskNotFound {
			return task, false, nil
		}
		return task, false, fmt.Errorf("check whether task %s already exists before resolving workflow targets: %w", id, err)
	}
	// A reply for another ID is not a record of this one.
	if task.ID != id || task.Spec.ID != id {
		return task, false, fmt.Errorf("check whether task %s already exists: the destination answered with task %q (specification %q)", id, task.ID, task.Spec.ID)
	}
	if task.Pruned || task.Spec.Capability != "workflow.run" || len(task.Spec.Args) == 0 {
		return task, false, fmt.Errorf("task ID %s was already used and its workflow plan is not retained; choose a new task ID", id)
	}
	return task, true, nil
}

// resolveWorkflowCall resolves selectors in a direct workflow.run call or a
// workflow.run task submission. Anything else, and any plan without
// selectors, is returned unchanged. A task submission whose ID is already
// held returns that task instead, which the caller must not submit.
func (c Client) resolveWorkflowCall(ctx context.Context, target, method string, params json.RawMessage) (json.RawMessage, *model.Task, error) {
	switch method {
	case "workflow.run":
		var plan model.WorkflowPlan
		if err := json.Unmarshal(params, &plan); err != nil {
			return nil, nil, notSent(fmt.Errorf("invalid workflow plan; nothing was submitted: %w", err))
		}
		if !plan.HasPlacement() {
			return params, nil, nil
		}
		// A synchronous call has no task ID to reconcile, so IDs it does not
		// name are fresh for every call.
		resolved, _, err := c.resolveWorkflowPlan(ctx, plan, "")
		if err != nil {
			return nil, nil, notSent(err)
		}
		return model.JSON(resolved), nil, nil
	case "tasks.start", model.TaskCheckMethod:
		// A check resolves the plan exactly as its submission would, using
		// the same read-only lookups, so it reports on concrete targets.
		var spec model.TaskSpec
		if json.Unmarshal(params, &spec) != nil || spec.Capability != "workflow.run" {
			return params, nil, nil
		}
		r, err := c.ResolveTask(ctx, target, spec)
		if err != nil || r.Steps == nil {
			return params, nil, err
		}
		return model.JSON(r.Spec), r.Existing, nil
	}
	return params, nil, nil
}

// resolveWorkflowPlan turns select and each targets into concrete steps.
// parent derives stable child IDs for a task; "" assigns fresh ones.
func (c Client) resolveWorkflowPlan(ctx context.Context, plan model.WorkflowPlan, parent string) (model.WorkflowPlan, []ResolvedStep, error) {
	fanned, err := checkPlacements(plan)
	if err != nil {
		return plan, nil, err
	}
	// Discover every selector before building anything, so the step limit is
	// checked against the complete expansion.
	matches := make([][]model.NodeSelection, len(plan.Steps))
	cache := map[string][]model.NodeSelection{}
	total := 0
	for i, step := range plan.Steps {
		total++
		if step.Placement == nil {
			continue
		}
		q := step.Placement.Selector()
		if step.Placement.Select != nil {
			var selected model.NodeSelection
			if err = c.selectNode(ctx, "", q, &selected); err != nil {
				return plan, nil, fmt.Errorf("workflow step %s: select: %w", step.Name, err)
			}
			if selected.ID == "" {
				return plan, nil, fmt.Errorf("workflow step %s: select returned no machine", step.Name)
			}
			matches[i] = []model.NodeSelection{selected}
			continue
		}
		key := string(model.JSON(q))
		found, ok := cache[key]
		if !ok {
			params := struct {
				model.NodeSelector
				All bool `json:"all"`
			}{q, true}
			if found, err = c.selectAll(ctx, "", q, params); err != nil {
				return plan, nil, fmt.Errorf("workflow step %s: each: %w", step.Name, err)
			}
			cache[key] = found
		}
		if len(found) == 0 {
			return plan, nil, fmt.Errorf("workflow step %s: each matched no eligible machine", step.Name)
		}
		matches[i] = found
		total += len(found) - 1
	}
	if total > maxWorkflowSteps {
		var counts []string
		for i, step := range plan.Steps {
			if fanned[step.Name] {
				counts = append(counts, fmt.Sprintf("%s on %d machines", step.Name, len(matches[i])))
			}
		}
		return plan, nil, fmt.Errorf("workflow expands to %d steps (%s); at most %d steps are allowed after fan-out", total, strings.Join(counts, ", "), maxWorkflowSteps)
	}
	names := map[string]string{}
	from := map[string]string{}
	instances := map[string][]string{}
	steps := make([]model.WorkflowStep, 0, total)
	for i, step := range plan.Steps {
		if step.Placement == nil {
			steps = append(steps, step)
			continue
		}
		if step.Placement.Select != nil {
			selected := matches[i][0]
			step.Target, step.Placement = selected.ID, nil
			names[selected.ID] = selected.Name
			steps = append(steps, step)
			continue
		}
		labels := machineLabels(matches[i])
		for j, machine := range matches[i] {
			instance := step
			instance.Name = step.Name + "@" + labels[j]
			instance.Target, instance.Placement = machine.ID, nil
			instance.Needs = slices.Clone(step.Needs)
			instance.Inputs = slices.Clone(step.Inputs)
			if step.Task.ID != "" {
				// Explicit IDs stay stable per machine without exceeding
				// the task ID length limit.
				instance.Task.ID = identity.ID([]byte(step.Task.ID + "\x00each\x00" + machine.ID))
			}
			names[machine.ID] = machine.Name
			from[instance.Name] = step.Name
			instances[step.Name] = append(instances[step.Name], instance.Name)
			steps = append(steps, instance)
		}
	}
	for i := range steps {
		if len(steps[i].Needs) == 0 {
			continue
		}
		var needs []string
		for _, dep := range steps[i].Needs {
			if expanded, ok := instances[dep]; ok {
				needs = append(needs, expanded...)
			} else {
				needs = append(needs, dep)
			}
		}
		steps[i].Needs = needs
	}
	for i := range steps {
		if steps[i].Task.ID != "" {
			continue
		}
		// The same derivation as grant preparation, so the printed plan and
		// the submitted plan agree on every route.
		steps[i].Task.ID = identity.NewID()
		if parent != "" {
			steps[i].Task.ID = identity.ID([]byte(parent + "\x00" + steps[i].Name))
		}
	}
	resolved := model.WorkflowPlan{Steps: steps, MaxParallel: plan.MaxParallel}
	if _, err = resolved.Ordered(); err != nil {
		return plan, nil, fmt.Errorf("resolved workflow plan: %w", err)
	}
	return resolved, describeResolvedPlan(resolved, names, from), nil
}

// checkPlacements validates selector use before any discovery and returns the
// fanned-out step names.
func checkPlacements(plan model.WorkflowPlan) (map[string]bool, error) {
	if len(plan.Steps) == 0 || len(plan.Steps) > maxWorkflowSteps {
		return nil, errors.New("workflow requires 1..100 steps")
	}
	fanned := map[string]bool{}
	seen := map[string]bool{}
	for _, step := range plan.Steps {
		if step.Name == "" || seen[step.Name] {
			return nil, errors.New("workflow step names must be nonempty and unique")
		}
		seen[step.Name] = true
		p := step.Placement
		if p == nil {
			continue
		}
		if step.Target != "" || (p.Select == nil) == (p.Each == nil) {
			return nil, fmt.Errorf(`workflow step %s: target must be a machine name or ID, {"select": selector} or {"each": selector}`, step.Name)
		}
		if err := p.Selector().Validate(); err != nil {
			return nil, fmt.Errorf("workflow step %s: %w", step.Name, err)
		}
		if p.Each != nil {
			fanned[step.Name] = true
		}
	}
	for _, step := range plan.Steps {
		for _, input := range step.Inputs {
			if fanned[input.Step] {
				return nil, fmt.Errorf("workflow step %s takes an artifact input from step %s, which fans out to one step per machine, so the source is ambiguous; use needs for ordering, or give the producing step one target", step.Name, input.Step)
			}
		}
	}
	return fanned, nil
}

// machineLabels names fan-out instances by machine name, falling back to the
// machine ID when a name is missing or shared by several matches.
func machineLabels(matches []model.NodeSelection) []string {
	count := map[string]int{}
	for _, m := range matches {
		count[m.Name]++
	}
	labels := make([]string, len(matches))
	for i, m := range matches {
		labels[i] = m.Name
		if m.Name == "" || count[m.Name] > 1 {
			labels[i] = m.ID
		}
	}
	return labels
}

func describeResolvedPlan(plan model.WorkflowPlan, names, from map[string]string) []ResolvedStep {
	steps := make([]ResolvedStep, len(plan.Steps))
	for i, step := range plan.Steps {
		steps[i] = ResolvedStep{Name: step.Name, From: from[step.Name], Target: step.Target, Node: names[step.Target], TaskID: step.Task.ID, When: step.When}
	}
	return steps
}
