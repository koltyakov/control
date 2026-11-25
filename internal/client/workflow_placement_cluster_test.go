package client

import (
	"encoding/json"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/koltyakov/control/internal/model"
)

// A fan-out resolves on the orchestrator through account routing, skips the
// instruction-only System companion, runs with cleanup, keeps its machines on
// resubmission and is reconciled from the retained resolved plan. A plan that
// fails to resolve creates no grant and no task.
func TestWorkflowFanOutAcrossFleet(t *testing.T) {
	t.Parallel()
	ctx, c, workers, _ := delegationClusterWithTimeout(t, true, 60*time.Second, true)
	const plan = `{"maxParallel":4,"steps":[
		{"name":"probe","target":{"each":{"capability":"test.owner"}},"task":{"capability":"test.owner","args":{}}},
		{"name":"pick","target":{"select":{"capability":"test.owner"}},"needs":["probe"],"task":{"capability":"test.owner","args":{}}},
		{"name":"cleanup","target":"first","when":"always","needs":["probe"],"task":{"capability":"test.owner","args":{}}}]}`
	spec := workflowSpec("fanout-parent", plan)
	resolved, steps, err := c.ResolveTask(ctx, "first", spec)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, step := range steps {
		names = append(names, step.Name)
	}
	if !slices.Equal(names, []string{"probe@first", "probe@third", "pick", "cleanup"}) {
		t.Fatal("System companion or unmatched machine in fan-out", steps)
	}
	if steps[0].Target != workers[0].Identity.ID || steps[1].Target != workers[2].Identity.ID || steps[2].Target == workers[1].Identity.ID {
		t.Fatal("resolved targets", steps)
	}
	var task model.Task
	if _, err = c.StartTask(ctx, "first", spec, &task); err != nil {
		t.Fatal(err)
	}
	finished, err := c.Wait(ctx, "first", "fanout-parent")
	if err != nil || finished.State != "succeeded" {
		t.Fatal("fan-out workflow", finished.State, finished.Error, err)
	}
	var results map[string]model.Task
	if err = json.Unmarshal(finished.Result, &results); err != nil {
		t.Fatal(err)
	}
	var ran []string
	for name, child := range results {
		ran = append(ran, name)
		if child.State != "succeeded" {
			t.Fatal(name, child.State, child.Error)
		}
	}
	sort.Strings(ran)
	if !slices.Equal(ran, []string{"cleanup", "pick", "probe@first", "probe@third"}) {
		t.Fatal("children", ran)
	}
	// Grant preparation also turns the named cleanup target into its ID.
	retained := decodePlan(t, finished.Spec.Args)
	for i, step := range decodePlan(t, resolved.Args) {
		if step.Target == "first" {
			step.Target = workers[0].Identity.ID
		}
		if retained[i].Name != step.Name || retained[i].Target != step.Target || retained[i].Task.ID != step.Task.ID || !slices.Equal(retained[i].Needs, step.Needs) {
			t.Fatal("retained plan differs from the printed plan", retained[i], step)
		}
	}
	// Resubmitting the same ID reuses the retained plan and is idempotent.
	again, againSteps, err := c.ResolveTask(ctx, "first", spec)
	if err != nil || !sameWorkflowPlan(t, again.Args, finished.Spec.Args) {
		t.Fatal("resubmission re-resolved", err)
	}
	for i := range steps {
		if againSteps[i].Name != steps[i].Name || againSteps[i].TaskID != steps[i].TaskID || (i < 3 && againSteps[i].Target != steps[i].Target) {
			t.Fatal("resubmission plan", againSteps[i], steps[i])
		}
	}
	if _, err = c.StartTask(ctx, "first", spec, &task); err != nil || task.Updated != finished.Updated {
		t.Fatal("resubmission was not idempotent", task.State, err)
	}
	report, err := c.ReconcileWorkflow(ctx, "first", "fanout-parent")
	if err != nil || !report.Complete || len(report.Steps) != 4 || report.Steps[1].Name != "probe@third" || report.Steps[1].ID != steps[1].TaskID {
		t.Fatal("reconcile resolved plan", report, err)
	}
	grants := func() int {
		total := 0
		for _, name := range []string{"first", "third"} {
			var list []model.Delegation
			if err := c.Call(ctx, name, "access.list", map[string]any{}, &list); err != nil {
				t.Fatal(err)
			}
			total += len(list)
		}
		return total
	}
	before := grants()
	for _, failing := range []string{
		`{"steps":[{"name":"a","target":"first","task":{"capability":"test.owner"}},{"name":"b","target":{"each":{"labels":{"role":"missing"}}},"needs":["a"],"task":{"capability":"test.owner"}}]}`,
		`{"steps":[{"name":"a","target":{"each":{"capability":"test.owner"}},"task":{"capability":"test.owner"}},{"name":"b","target":"first","inputs":[{"step":"a","artifact":0,"path":"in"}],"task":{"capability":"test.owner"}}]}`,
	} {
		_, err := c.StartTask(ctx, "first", workflowSpec("fanout-failed", failing), nil)
		if err == nil || !strings.Contains(err.Error(), "nothing was submitted") {
			t.Fatal("unresolvable plan", err)
		}
	}
	if after := grants(); after != before {
		t.Fatalf("resolution failure created grants: %d before, %d after", before, after)
	}
	if err := c.Call(ctx, "first", "tasks.get", map[string]any{"id": "fanout-failed"}, nil); err == nil || !strings.Contains(err.Error(), "task not found") {
		t.Fatal("resolution failure created a task", err)
	}
}

func sameWorkflowPlan(t *testing.T, a, b json.RawMessage) bool {
	t.Helper()
	var x, y model.WorkflowPlan
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return false
	}
	return string(model.JSON(x)) == string(model.JSON(y))
}
