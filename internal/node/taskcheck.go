package node

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/koltyakov/control/internal/clipboard"
	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/upload"
)

// Bounds for a check answer: every message, and how many problems one check
// lists before summarizing the rest.
const (
	taskCheckMessageBytes = 512
	taskCheckProblems     = 3
)

// checkTask answers tasks.check (D181). It follows the order of tasks.start
// admission but only reads node state. It reserves no task ID, writes nothing,
// enters no maintenance or provider admission, adopts or renews no grant,
// records no activity, starts no provider or MCP session, creates no workspace
// and contacts no artifact source, child destination or peers.call target.
// The dispatch authorization of tasks.check already required the tasks.start
// permission; the capability is authorized below exactly as tasks.start does.
func (n *Node) checkTask(ctx context.Context, caller, owner string, args json.RawMessage) (model.TaskCheck, error) {
	var spec model.TaskSpec
	if err := json.Unmarshal(args, &spec); err != nil {
		return model.TaskCheck{}, fmt.Errorf("invalid task specification: %w", err)
	}
	report := model.TaskCheck{Advisory: true, TaskID: spec.ID, Note: model.TaskCheckAdvisory}
	add := func(name, status, message string) {
		report.Checks = append(report.Checks, model.TaskCheckItem{Name: name, Status: status, Message: bounded(message, taskCheckMessageBytes)})
	}
	skipRest := func(message string, names ...string) {
		for _, name := range names {
			add(name, model.TaskCheckSkip, message)
		}
	}
	normalized, digest, specErr := checkedTaskSpec(spec)
	if specErr != nil {
		add("spec", model.TaskCheckFail, specErr.Error())
	} else {
		add("spec", model.TaskCheckPass, "")
	}
	_, known := n.providers[spec.Capability]
	capability := func() {
		if known {
			add("capability", model.TaskCheckPass, "")
		} else {
			add("capability", model.TaskCheckFail, fmt.Sprintf("unknown capability %q", spec.Capability))
		}
	}
	// Checks after authorization and the ID, in report order.
	later := []string{"args"}
	if spec.Capability == "workflow.run" || spec.Capability == "peers.call" {
		later = append(later, "coordination")
	}
	later = append(later, "inputs", "outputs", "lease", "admission")
	// Without authority for the capability, tasks.start reveals nothing else.
	// Capability names are discovery metadata, so only that is still reported.
	if err := n.authorize(ctx, caller, spec.Capability); err != nil {
		add("authorization", model.TaskCheckFail, err.Error())
		capability()
		skipRest("requires authority for this capability", append([]string{"id"}, later...)...)
		return finishTaskCheck(report), nil
	}
	add("authorization", model.TaskCheckPass, "")
	status, message, existing := n.checkTaskID(spec.ID, owner, digest)
	add("id", status, message)
	if existing {
		// tasks.start returns the matching task before capability lookup,
		// admission and lease checks, even if its provider was since removed.
		skipRest("an existing task with this ID and specification is returned without starting new work", append([]string{"capability"}, later...)...)
		return finishTaskCheck(report), nil
	}
	capability()
	if !known {
		add("args", model.TaskCheckSkip, "unknown capability")
	} else if specErr != nil {
		add("args", model.TaskCheckSkip, "invalid specification")
	} else {
		status, message = validateTaskArgs(n.providers[spec.Capability].Capability().InputSchema, normalized.Args)
		add("args", status, message)
	}
	switch spec.Capability {
	case "workflow.run":
		status, message = checkWorkflowPlan(normalized.Args)
		add("coordination", status, message)
	case "peers.call":
		status, message = checkPeerCall(normalized.Args)
		add("coordination", status, message)
	}
	status, message = n.checkTaskInputs(spec.Inputs)
	add("inputs", status, message)
	status, message = checkTaskOutputs(spec.Outputs)
	add("outputs", status, message)
	status, message, err := n.checkTaskLease(ctx, owner, spec.LeaseID)
	if err != nil {
		return model.TaskCheck{}, err
	}
	add("lease", status, message)
	status, message = n.checkTaskAdmission(ctx)
	add("admission", status, message)
	if known {
		report.Pools = n.taskCheckPools(spec)
	}
	return finishTaskCheck(report), nil
}

// authorizeTaskCheckGrant applies the capability authority of a tasks.start
// grant to an exact tasks.check grant, which instruction-only destinations
// require. The grant binds this specification and is single-use like other
// synchronous instructions. It cannot authorize tasks.start, and a tasks.start
// grant cannot authorize a check, so a check never uses submission authority.
func (n *Node) authorizeTaskCheckGrant(ctx context.Context, caller string, params json.RawMessage) error {
	var spec model.TaskSpec
	if err := json.Unmarshal(params, &spec); err != nil {
		return err
	}
	if spec.Capability == "workflow.run" || spec.Capability == "peers.call" {
		return errors.New("delegated tasks cannot create further coordination, so they cannot be checked under a grant")
	}
	return n.authorize(ctx, caller, spec.Capability)
}

func finishTaskCheck(report model.TaskCheck) model.TaskCheck {
	report.OK = true
	for _, check := range report.Checks {
		if check.Status == model.TaskCheckFail {
			report.OK = false
		}
	}
	return report
}

// checkedTaskSpec repeats the normalization and bounds of startTaskContext,
// except for generating a missing ID, and returns the idempotency digest.
// TestTaskCheckDigestMatchesAcceptance keeps the two in step.
func checkedTaskSpec(spec model.TaskSpec) (model.TaskSpec, string, error) {
	if len(spec.Args) == 0 {
		spec.Args = model.JSON(map[string]any{})
	}
	var normalized any
	decoder := json.NewDecoder(bytes.NewReader(spec.Args))
	decoder.UseNumber()
	if err := decoder.Decode(&normalized); err != nil {
		return spec, "", err
	}
	spec.Args = model.JSON(normalized)
	if spec.TimeoutSeconds == 0 {
		spec.TimeoutSeconds = 3600
	}
	tags, err := model.NormalizeTaskTags(spec.Tags)
	if err != nil {
		return spec, "", err
	}
	spec.Tags = tags
	encoded := model.JSON(spec)
	if len(spec.Args) > 1<<20 || len(spec.Inputs) > 128 || len(spec.Outputs) > 128 || len(encoded) > 2<<20 {
		return spec, "", errors.New("task specification exceeds limits: 1 MiB arguments, 128 inputs/outputs, 2 MiB total; use artifacts for bulk data")
	}
	if spec.TimeoutSeconds < 1 || spec.TimeoutSeconds > 86400 {
		return spec, "", errors.New("task timeout must be 1..86400 seconds")
	}
	sum := sha256.Sum256(encoded)
	return spec, hex.EncodeToString(sum[:]), nil
}

// checkTaskID reads the same records as reserveTaskIDLocked and
// submittedLocked, without reserving the ID or loading a cold payload.
func (n *Node) checkTaskID(id, owner, digest string) (status, message string, existing bool) {
	if id == "" {
		return model.TaskCheckPass, "no ID: one is generated at submission; assign one first so an uncertain outcome can be reconciled by ID", false
	}
	if !safeID.MatchString(id) {
		return model.TaskCheckFail, "invalid task ID: use 1-128 ASCII letters, digits, '_' or '-'", false
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if prior := n.taskIDs[strings.ToLower(id)]; prior != "" && prior != id {
		return model.TaskCheckFail, "task ID has a case-fold conflict with a retained or pending task ID", false
	}
	conflict := "task ID already used with a different owner or specification"
	if old := n.tasks[id]; old != nil {
		if old.Owner != owner || digest == "" || n.taskDigestLocked(old) != digest {
			return model.TaskCheckFail, conflict, false
		}
		return model.TaskCheckPass, fmt.Sprintf("task %s already exists with this owner and specification (state %s); submission returns it", id, old.State), true
	}
	if pruned, ok := n.tombstones[id]; ok {
		if pruned.Owner != owner || digest == "" || pruned.Spec != digest {
			return model.TaskCheckFail, conflict, false
		}
		return model.TaskCheckPass, fmt.Sprintf("task %s already exists with this owner and specification (state %s, pruned); submission returns it", id, pruned.State), true
	}
	if n.accepting[id] != nil {
		return model.TaskCheckWarn, "another submission of this ID is being accepted now; a submission waits for it, then returns that task or rejects a different owner or specification", false
	}
	return model.TaskCheckPass, "", false
}

// validateTaskArgs checks arguments against the advertised schema with the
// existing JSON Schema dependency. It resolves no remote references, so a
// schema needing one is reported as unchecked.
func validateTaskArgs(schema, args json.RawMessage) (string, string) {
	if len(schema) == 0 {
		return model.TaskCheckUnchecked, "the capability advertises no input schema"
	}
	var s jsonschema.Schema
	if err := json.Unmarshal(schema, &s); err != nil {
		return model.TaskCheckUnchecked, "the advertised input schema could not be parsed: " + err.Error()
	}
	resolved, err := s.Resolve(nil)
	if err != nil {
		return model.TaskCheckUnchecked, "the advertised input schema could not be resolved locally: " + err.Error()
	}
	var value any
	if err := json.Unmarshal(args, &value); err != nil {
		return model.TaskCheckFail, err.Error()
	}
	if err := resolved.Validate(value); err != nil {
		return model.TaskCheckFail, "arguments do not match the advertised input schema; acceptance does not validate them, so the provider would likely fail after acceptance: " + err.Error()
	}
	return model.TaskCheckPass, ""
}

// checkWorkflowPlan validates the plan graph on this coordinator only.
func checkWorkflowPlan(args json.RawMessage) (string, string) {
	var plan model.WorkflowPlan
	if err := json.Unmarshal(args, &plan); err != nil {
		return model.TaskCheckFail, "invalid workflow plan: " + err.Error()
	}
	ordered, err := plan.Ordered()
	if err != nil {
		return model.TaskCheckFail, err.Error()
	}
	seen := map[string]string{}
	for _, step := range plan.Steps {
		if step.Task.ID == "" {
			continue
		}
		if !safeID.MatchString(step.Task.ID) {
			return model.TaskCheckFail, fmt.Sprintf("workflow step %s: invalid task ID", step.Name)
		}
		key := step.Target + "\x00" + strings.ToLower(step.Task.ID)
		if other, ok := seen[key]; ok {
			return model.TaskCheckFail, fmt.Sprintf("workflow steps %s and %s use the same task ID on the same target, ignoring case", other, step.Name)
		}
		seen[key] = step.Name
	}
	return model.TaskCheckPass, fmt.Sprintf("plan graph valid with %d steps; children were not contacted: their targets, capabilities, arguments, authority, grants and task IDs are checked only at submission", len(ordered))
}

func checkPeerCall(args json.RawMessage) (string, string) {
	var q struct {
		Target string `json:"target"`
		Method string `json:"method"`
	}
	if err := json.Unmarshal(args, &q); err != nil {
		return model.TaskCheckFail, err.Error()
	}
	if q.Target == "" || q.Method == "" {
		return model.TaskCheckFail, "target and method are required"
	}
	switch q.Method {
	case "artifacts.open", "tcp.open", "tcp.listen", clipboard.OpenMethod, clipboard.PasteMethod, upload.OpenMethod, model.ShellOpenMethod:
		return model.TaskCheckFail, "peers.call supports JSON instructions, not raw streaming methods"
	}
	return model.TaskCheckPass, "the target was not contacted; its authority, arguments and state are checked only when the instruction runs"
}

// checkTaskInputs checks reference shape and grant presence. It contacts no
// source and does not disclose whether this node already holds the content.
func (n *Node) checkTaskInputs(inputs []model.Input) (string, string) {
	if len(inputs) == 0 {
		return model.TaskCheckPass, "none"
	}
	var failures, warnings []string
	for i, input := range inputs {
		a := input.Artifact
		switch {
		case !filepath.IsLocal(input.Path):
			failures = append(failures, fmt.Sprintf("input %d: path must be relative to the task workspace", i))
		case !digestID.MatchString(a.ID) || a.SHA256 != a.ID || a.Size < 0:
			failures = append(failures, fmt.Sprintf("input %d: invalid artifact reference", i))
		case a.Node == n.Identity.ID || a.Node == n.Config.Name:
		case a.Grant == "":
			warnings = append(warnings, fmt.Sprintf("input %d: no source grant; staging fails unless this node already holds the content", i))
		}
	}
	if len(failures) != 0 {
		return model.TaskCheckFail, summarizeProblems(failures) + "; staging would fail after acceptance"
	}
	if len(warnings) != 0 {
		return model.TaskCheckWarn, summarizeProblems(warnings)
	}
	return model.TaskCheckPass, "grants were not verified and sources were not contacted"
}

func checkTaskOutputs(outputs []string) (string, string) {
	var failures []string
	for i, path := range outputs {
		if !filepath.IsLocal(path) {
			failures = append(failures, fmt.Sprintf("output %d: path must be relative to the task workspace", i))
		}
	}
	if len(failures) != 0 {
		return model.TaskCheckFail, summarizeProblems(failures) + "; publication would fail after the provider ran"
	}
	return model.TaskCheckPass, ""
}

func summarizeProblems(problems []string) string {
	if len(problems) <= taskCheckProblems {
		return strings.Join(problems, "; ")
	}
	return fmt.Sprintf("%s; and %d more", strings.Join(problems[:taskCheckProblems], "; "), len(problems)-taskCheckProblems)
}

// checkTaskLease repeats the tasks.start lease decision. lockLeaseSettled only
// waits for an in-flight lease write; it changes nothing.
func (n *Node) checkTaskLease(ctx context.Context, owner, leaseID string) (string, string, error) {
	if err := n.lockLeaseSettled(ctx); err != nil {
		return "", "", err
	}
	defer n.mu.Unlock()
	now := time.Now()
	if n.lease != nil && (now.Before(n.lease.Expires) || n.leaseBusy()) {
		if leaseID != n.lease.ID || owner != n.lease.Owner || now.After(n.lease.Expires) {
			return model.TaskCheckFail, "node has an exclusive lease; provide the current owner's unexpired leaseId", nil
		}
		return model.TaskCheckPass, "holds the current lease until " + n.lease.Expires.UTC().Format(time.RFC3339), nil
	}
	if leaseID != "" {
		return model.TaskCheckFail, "lease expired or does not exist", nil
	}
	return model.TaskCheckPass, "", nil
}

func (n *Node) checkTaskAdmission(ctx context.Context) (string, string) {
	paused, disabled := n.work.Admission()
	// The low-disk guard refuses new tasks before maintenance admission. This
	// reads the same cached probe and writes nothing to disk.
	_, _, lowDisk := n.taskDiskState(ctx)
	n.mu.Lock()
	stopped := n.closed || n.ctx == nil || n.ctx.Err() != nil
	// tasks.start counts its own reservation before comparing with the bound.
	full := len(n.cancels)+len(n.accepting)+1 > 256
	n.mu.Unlock()
	switch {
	case stopped:
		return model.TaskCheckFail, "node is not running"
	case disabled:
		return model.TaskCheckFail, "admission is closed: the machine is disabled by its fleet owner, unregistered, or still starting"
	case full:
		return model.TaskCheckFail, "node task queue is full"
	case lowDisk != nil:
		return model.TaskCheckFail, "low disk space: " + lowDisk.Error()
	case paused:
		return model.TaskCheckWarn, "a maintenance reservation, such as a managed update, holds admission; a submission waits until it ends or the request deadline passes"
	}
	return model.TaskCheckPass, ""
}

// taskCheckPools snapshots the pools this specification would use, in stage
// order. Snapshots are copies; reading them takes no slot.
func (n *Node) taskCheckPools(spec model.TaskSpec) []model.TaskCheckPool {
	var pools []model.TaskCheckPool
	add := func(name string, pool *providerPool) {
		if pool != nil {
			pools = append(pools, model.TaskCheckPool{Name: name, ConcurrencyPool: pool.snapshot()})
		}
	}
	if len(spec.Inputs) != 0 {
		add("inputStaging", n.inputStagingPool)
	}
	switch pool, _ := n.providerSlots(spec.Capability); pool {
	case n.executionPool:
		add("execution", pool)
	case n.operationsPool:
		add("operations", pool)
	case n.desktopPool:
		add("desktop", pool)
	}
	if len(spec.Outputs) != 0 {
		add("outputPublication", n.publicationPool)
	}
	return pools
}
