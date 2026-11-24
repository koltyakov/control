package node

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/koltyakov/control/internal/client"
	"github.com/koltyakov/control/internal/gateway"
	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/transport"
	"github.com/koltyakov/control/internal/transport/transporttest"
)

// cleanupRecorder is a local provider recording which steps ran, in order.
// Gated steps block until released or cancelled.
type cleanupRecorder struct {
	mu      sync.Mutex
	order   []string
	gates   map[string]chan struct{}
	entered chan string
}

func cleanupNode(t *testing.T, gated ...string) (*Node, *cleanupRecorder) {
	t.Helper()
	n := localNode(t, func(cfg *Config) { cfg.MaxTasks = 4 })
	r := &cleanupRecorder{gates: map[string]chan struct{}{}, entered: make(chan string, 32)}
	for _, name := range gated {
		r.gates[name] = make(chan struct{})
	}
	if err := n.Register(provider{cap: model.Capability{Name: "test.cleanup", InputSchema: schema(nil)}, run: func(ctx context.Context, args json.RawMessage, e Execution) (any, error) {
		var q struct {
			Name string
			Fail bool
		}
		if err := json.Unmarshal(args, &q); err != nil {
			return nil, err
		}
		r.mu.Lock()
		r.order = append(r.order, q.Name)
		gate := r.gates[q.Name]
		r.mu.Unlock()
		r.entered <- q.Name
		if gate != nil {
			select {
			case <-gate:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		if q.Fail {
			return nil, fmt.Errorf("%s failed", q.Name)
		}
		return q.Name, nil
	}}); err != nil {
		t.Fatal(err)
	}
	return n, r
}

func (r *cleanupRecorder) ran() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.order)
}

func (r *cleanupRecorder) wait(t *testing.T, ctx context.Context, name string) {
	t.Helper()
	for !slices.Contains(r.ran(), name) {
		select {
		case <-r.entered:
		case <-ctx.Done():
			t.Fatalf("step %s did not start: %v (ran %v)", name, ctx.Err(), r.ran())
		}
	}
}

func cleanupStep(name string, fail bool, when string, needs ...string) WorkflowStep {
	return WorkflowStep{Name: name, When: when, Needs: needs, Task: model.TaskSpec{ID: name, Capability: "test.cleanup",
		Args: model.JSON(map[string]any{"name": name, "fail": fail}), TimeoutSeconds: 60}}
}

func runCleanupWorkflow(ctx context.Context, n *Node, steps []WorkflowStep, parallel int) (map[string]model.Task, error) {
	value, err := n.workflowRun(ctx, model.JSON(map[string]any{"steps": steps, "maxParallel": parallel}), Execution{Log: io.Discard})
	results, _ := value.(map[string]model.Task)
	return results, err
}

func resultNames(results map[string]model.Task) []string {
	var names []string
	for name := range results {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

func TestWorkflowCleanupRunsAfterFailure(t *testing.T) {
	n, r := cleanupNode(t)
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	results, err := runCleanupWorkflow(ctx, n, []WorkflowStep{
		cleanupStep("setup", false, ""),
		cleanupStep("work", true, model.WorkflowWhenSuccess, "setup"),
		cleanupStep("next", false, "", "work"),
		cleanupStep("cleanup", false, model.WorkflowWhenAlways, "work"),
		// Settled dependencies include a success step that will never start.
		cleanupStep("final", false, model.WorkflowWhenAlways, "cleanup", "next"),
		// A success step needing a successful cleanup still never starts after a failure.
		cleanupStep("publish", false, "", "cleanup"),
	}, 1)
	if got := r.ran(); !slices.Equal(got, []string{"setup", "work", "cleanup", "final"}) {
		t.Fatal("unexpected steps ran", got)
	}
	if got := resultNames(results); !slices.Equal(got, []string{"cleanup", "final", "setup", "work"}) {
		t.Fatal("result omitted or invented steps", got)
	}
	if results["cleanup"].State != "succeeded" || results["work"].State != "failed" {
		t.Fatal("unexpected child outcomes", results)
	}
	if err == nil || err.Error() != "step work: work failed" {
		t.Fatalf("workflow error %q does not report only the original failure", err)
	}
}

func TestWorkflowCleanupRunsAfterSuccess(t *testing.T) {
	n, r := cleanupNode(t)
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	results, err := runCleanupWorkflow(ctx, n, []WorkflowStep{
		cleanupStep("setup", false, ""),
		cleanupStep("cleanup", false, model.WorkflowWhenAlways, "setup"),
		cleanupStep("publish", false, "", "cleanup"),
	}, 1)
	if err != nil || !slices.Equal(r.ran(), []string{"setup", "cleanup", "publish"}) || len(results) != 3 {
		t.Fatal("successful workflow did not run cleanup and its dependent", r.ran(), results, err)
	}
}

func TestWorkflowCleanupFailureReporting(t *testing.T) {
	t.Run("after failure", func(t *testing.T) {
		n, _ := cleanupNode(t)
		results, err := runCleanupWorkflow(t.Context(), n, []WorkflowStep{
			cleanupStep("work", true, ""),
			cleanupStep("cleanup", true, model.WorkflowWhenAlways, "work"),
		}, 1)
		if err == nil || err.Error() != "step work: work failed\nstep cleanup: cleanup failed" {
			t.Fatalf("cleanup failure masked or hid the original failure: %q", err)
		}
		if results["cleanup"].State != "failed" || results["cleanup"].ID != "cleanup" {
			t.Fatal("failed cleanup missing from result", results)
		}
	})
	t.Run("alone", func(t *testing.T) {
		n, r := cleanupNode(t)
		results, err := runCleanupWorkflow(t.Context(), n, []WorkflowStep{
			cleanupStep("work", false, ""),
			cleanupStep("cleanup", true, model.WorkflowWhenAlways, "work"),
			cleanupStep("publish", false, "", "cleanup"),
		}, 1)
		if err == nil || err.Error() != "step cleanup: cleanup failed" || results["cleanup"].State != "failed" {
			t.Fatal("cleanup failure did not fail the workflow", results, err)
		}
		if slices.Contains(r.ran(), "publish") {
			t.Fatal("success step started after cleanup failure")
		}
	})
	t.Run("earlier cleanup failure", func(t *testing.T) {
		// A cleanup step without dependencies fails while a success step is
		// still running. The success step's failure is still reported first.
		n, r := cleanupNode(t, "work")
		ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
		defer cancel()
		done := make(chan error, 1)
		go func() {
			_, err := runCleanupWorkflow(ctx, n, []WorkflowStep{
				cleanupStep("work", true, ""),
				cleanupStep("cleanup", true, model.WorkflowWhenAlways),
			}, 2)
			done <- err
		}()
		r.wait(t, ctx, "work")
		r.wait(t, ctx, "cleanup")
		pipelineFinished(t, n, ctx, n.Identity.ID, "cleanup", "failed")
		close(r.gates["work"])
		select {
		case err := <-done:
			if err == nil || err.Error() != "step work: work failed\nstep cleanup: cleanup failed" {
				t.Fatalf("original failure not reported first: %q", err)
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	})
}

func TestWorkflowCleanupWithFailedArtifactInput(t *testing.T) {
	n, r := cleanupNode(t)
	steps := []WorkflowStep{cleanupStep("work", true, ""), cleanupStep("collect", false, model.WorkflowWhenAlways), cleanupStep("final", false, model.WorkflowWhenAlways, "collect")}
	if err := json.Unmarshal([]byte(`[{"step":"work","artifact":0,"path":"work.log"}]`), &steps[1].Inputs); err != nil {
		t.Fatal(err)
	}
	results, err := runCleanupWorkflow(t.Context(), n, steps, 1)
	if got := r.ran(); !slices.Equal(got, []string{"work", "final"}) {
		t.Fatal("cleanup with an unavailable input started, or a later cleanup did not", got)
	}
	if _, ok := results["collect"]; ok {
		t.Fatal("not-started cleanup appeared as a child", results)
	}
	want := "step work: work failed\ncleanup step collect (task collect on ) not started: its input from step work is unavailable because that step did not succeed"
	if err == nil || err.Error() != want {
		t.Fatalf("workflow error = %q, want %q", err, want)
	}
}

func TestWorkflowCleanupNotStartedAfterCancellationOrTimeout(t *testing.T) {
	for _, mode := range []string{"cancel", "timeout"} {
		t.Run(mode, func(t *testing.T) {
			n, r := cleanupNode(t, "work")
			ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
			defer cancel()
			run, stop := context.WithCancel(ctx)
			if mode == "timeout" {
				stop()
				run, stop = context.WithTimeout(ctx, 500*time.Millisecond)
			}
			defer stop()
			done := make(chan error, 1)
			go func() {
				_, err := runCleanupWorkflow(run, n, []WorkflowStep{
					cleanupStep("work", false, ""),
					cleanupStep("cleanup", false, model.WorkflowWhenAlways, "work"),
					// Ready from the start, but sequential order keeps it waiting.
					cleanupStep("independent", false, model.WorkflowWhenAlways),
				}, 1)
				done <- err
			}()
			r.wait(t, ctx, "work")
			if mode == "cancel" {
				stop()
			}
			var err error
			select {
			case err = <-done:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			want := context.Canceled
			if mode == "timeout" {
				want = context.DeadlineExceeded
			}
			if !errors.Is(err, want) || !strings.Contains(err.Error(), "cleanup step cleanup (task cleanup on ) not started: the workflow was cancelled, timed out or lost its authority") || !strings.Contains(err.Error(), "cleanup step independent") {
				t.Fatalf("unexpected %s error: %v", mode, err)
			}
			if got := r.ran(); !slices.Equal(got, []string{"work"}) {
				t.Fatal("cleanup started after the workflow lost authority", got)
			}
			pipelineFinished(t, n, ctx, n.Identity.ID, "work", "cancelled")
		})
	}
}

func TestWorkflowCleanupParallel(t *testing.T) {
	n, r := cleanupNode(t, "bad", "sibling")
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	type outcome struct {
		results map[string]model.Task
		err     error
	}
	done := make(chan outcome, 1)
	go func() {
		results, err := runCleanupWorkflow(ctx, n, []WorkflowStep{
			cleanupStep("bad", true, ""),
			cleanupStep("sibling", false, ""),
			cleanupStep("next", false, "", "bad"),
			cleanupStep("early", false, model.WorkflowWhenAlways, "bad"),
			cleanupStep("late", false, model.WorkflowWhenAlways, "bad", "sibling"),
		}, 2)
		done <- outcome{results, err}
	}()
	r.wait(t, ctx, "bad")
	r.wait(t, ctx, "sibling")
	close(r.gates["bad"])
	// Cleanup for the failed step starts while its sibling is still running;
	// the sibling is never cancelled to make room.
	r.wait(t, ctx, "early")
	if slices.Contains(r.ran(), "late") {
		t.Fatal("cleanup started before every dependency settled")
	}
	close(r.gates["sibling"])
	var got outcome
	select {
	case got = <-done:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if got.err == nil || got.err.Error() != "step bad: bad failed" {
		t.Fatal("unexpected workflow error", got.err)
	}
	if names := resultNames(got.results); !slices.Equal(names, []string{"bad", "early", "late", "sibling"}) {
		t.Fatal("unexpected started steps", names)
	}
	if got.results["sibling"].State != "succeeded" || got.results["late"].Created.Before(got.results["sibling"].Updated) {
		t.Fatal("sibling was cancelled or late cleanup did not wait for it", got.results)
	}
}

func TestWorkflowPlanValidatesStepConditions(t *testing.T) {
	step := func(name, when string, needs ...string) model.WorkflowStep {
		return model.WorkflowStep{Name: name, When: when, Needs: needs, Task: model.TaskSpec{Capability: "exec.run"}}
	}
	for _, tc := range []struct {
		name  string
		steps []model.WorkflowStep
		want  string
	}{
		{"unknown marker", []model.WorkflowStep{step("a", "failure")}, `workflow step a: when must be "success" or "always"`},
		{"case sensitive", []model.WorkflowStep{step("a", "Always")}, `workflow step a: when must be "success" or "always"`},
		{"cycle through cleanup", []model.WorkflowStep{step("a", "", "b"), step("b", model.WorkflowWhenAlways, "a")}, "workflow dependency cycle"},
		{"cleanup self cycle", []model.WorkflowStep{step("a", model.WorkflowWhenAlways, "a")}, "workflow dependency cycle"},
		{"unknown cleanup dependency", []model.WorkflowStep{step("a", model.WorkflowWhenAlways, "missing")}, "unknown dependency missing"},
	} {
		if _, err := (model.WorkflowPlan{Steps: tc.steps}).Ordered(); err == nil || err.Error() != tc.want {
			t.Errorf("%s: error %v, want %q", tc.name, err, tc.want)
		}
	}
	valid := model.WorkflowPlan{Steps: []model.WorkflowStep{step("a", model.WorkflowWhenSuccess), step("b", model.WorkflowWhenAlways, "a"), step("c", "", "b")}}
	if _, err := valid.Ordered(); err != nil || !valid.HasAlways() {
		t.Fatal("valid cleanup plan rejected", err)
	}
}

// A child whose state became unknown never releases cleanup steps that
// depend on it, directly or through a step that will not start.
func TestWorkflowCleanupBlockedByUnresolvedChild(t *testing.T) {
	var armed atomic.Bool
	armed.Store(true)
	f := newLifetimeFixture(t, true, false, func(r *model.Response) bool {
		if armed.Load() && statusReply(r, "child-a") {
			*r = model.Response{Error: "status store unavailable", Code: "operation_failed", Outcome: "unknown"}
		}
		return true
	})
	spec := lifetimeWorkflow()
	spec.Args = model.JSON(map[string]any{"steps": []model.WorkflowStep{
		{Name: "a", Target: "second", Task: lifetimeTask("child-a", true)},
		{Name: "b", Target: "second", Task: lifetimeTask("child-b", false), Needs: []string{"a"}},
		{Name: "after-a", Target: "second", When: model.WorkflowWhenAlways, Task: lifetimeTask("child-after-a", false), Needs: []string{"a"}},
		{Name: "after-b", Target: "second", When: model.WorkflowWhenAlways, Task: lifetimeTask("child-after-b", false), Needs: []string{"b"}},
	}})
	spec.TimeoutSeconds = 6
	if err := f.client.Call(f.ctx, "first", "tasks.start", spec, nil); err != nil {
		t.Fatal(err)
	}
	f.waitEntered(t)
	parent, err := f.client.Wait(f.ctx, "first", spec.ID)
	if err != nil || parent.State != "failed" {
		t.Fatal("unresolved workflow did not fail", parent.State, parent.Error, err)
	}
	for _, want := range []string{"child-a on", "unresolved", "cleanup step after-a", "cleanup step after-b", "the outcome of step a is unknown; reconcile it"} {
		if !strings.Contains(parent.Error, want) {
			t.Fatalf("parent error lacks %q:\n%s", want, parent.Error)
		}
	}
	armed.Store(false)
	f.release()
	if child, err := f.client.Wait(f.ctx, "second", "child-a"); err != nil || child.State != "succeeded" {
		t.Fatal("unresolved child was disturbed", child.State, err)
	}
	if f.calls.Load() != 1 {
		t.Fatal("cleanup started behind an unresolved child", f.calls.Load())
	}
	report, err := f.client.ReconcileWorkflow(f.ctx, "first", spec.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range report.Steps[1:] {
		if step.Task != nil || !step.NotStarted {
			t.Fatalf("step %s not reported as not started: %+v", step.Name, step)
		}
	}
	if report.Steps[2].When != model.WorkflowWhenAlways || report.Steps[1].When != "" {
		t.Fatal("reconciliation lost step conditions", report.Steps)
	}
}

// Revocation is enforced by each destination. Revoking a workflow's grants
// there refuses its cleanup step; nothing is resubmitted.
func TestWorkflowCleanupRefusedAfterRevocation(t *testing.T) {
	f := newLifetimeFixture(t, false, false, nil)
	spec := lifetimeWorkflow()
	spec.Args = model.JSON(map[string]any{"steps": []model.WorkflowStep{
		{Name: "a", Target: "second", Task: lifetimeTask("child-a", true)},
		{Name: "cleanup", Target: "second", When: model.WorkflowWhenAlways, Task: lifetimeTask("child-cleanup", false), Needs: []string{"a"}},
	}})
	if err := f.client.Call(f.ctx, "first", "tasks.start", spec, nil); err != nil {
		t.Fatal(err)
	}
	f.waitEntered(t)
	grants := f.grants(t, "second")
	if len(grants) != 2 {
		t.Fatal("cleanup grant was not prepared up front with the plan", grants)
	}
	for _, grant := range grants {
		if err := f.client.Call(f.ctx, "second", "access.revoke", map[string]string{"id": grant.ID}, nil); err != nil {
			t.Fatal(err)
		}
	}
	parent, err := f.client.Wait(f.ctx, "first", spec.ID)
	if err != nil || parent.State != "failed" {
		t.Fatal("revoked workflow did not fail", parent.State, parent.Error, err)
	}
	if child, err := f.client.Wait(f.ctx, "second", "child-a"); err != nil || child.State != "cancelled" {
		t.Fatal("revocation did not cancel the running child", child.State, err)
	}
	if err := f.client.Call(f.ctx, "second", "tasks.get", map[string]string{"id": "child-cleanup"}, nil); err == nil || !strings.Contains(err.Error(), "task not found") {
		t.Fatal("cleanup ran under revoked authority", err)
	}
	if f.calls.Load() != 1 {
		t.Fatal("a child was replayed or cleanup executed", f.calls.Load())
	}
}

func TestWorkflowCleanupReconcile(t *testing.T) {
	f := newLifetimeFixture(t, false, false, nil)
	fail := model.TaskSpec{ID: "child-work", Capability: "test.lifetime", Args: model.JSON(map[string]bool{"fail": true}), Outputs: []string{"output"}}
	spec := model.TaskSpec{ID: "cleanup-workflow", Capability: "workflow.run", Args: model.JSON(map[string]any{"steps": []any{
		map[string]any{"name": "work", "target": "second", "task": fail},
		map[string]any{"name": "collect", "target": "second", "when": "always", "task": lifetimeTask("child-collect", false), "inputs": []any{map[string]any{"step": "work", "artifact": 0, "path": "work.out"}}},
		map[string]any{"name": "cleanup", "target": "second", "when": "always", "needs": []string{"work"}, "task": lifetimeTask("child-cleanup", false)},
	}})}
	if err := f.client.Call(f.ctx, "first", "tasks.start", spec, nil); err != nil {
		t.Fatal(err)
	}
	parent, err := f.client.Wait(f.ctx, "first", spec.ID)
	if err != nil || parent.State != "failed" || !strings.HasPrefix(parent.Error, "step work: lifetime fixture failure") || !strings.Contains(parent.Error, "cleanup step collect") {
		t.Fatal("unexpected workflow outcome", parent.State, parent.Error, err)
	}
	report, err := f.client.ReconcileWorkflow(f.ctx, "first", spec.ID)
	if err != nil {
		t.Fatal(err)
	}
	work, collect, cleanup := report.Steps[0], report.Steps[1], report.Steps[2]
	if work.Task == nil || work.Task.State != "failed" || work.NotStarted || work.When != "" {
		t.Fatal("failed step misreported", work)
	}
	if collect.Task != nil || !collect.NotStarted || collect.When != model.WorkflowWhenAlways || collect.ID != "child-collect" {
		t.Fatal("cleanup with unavailable input not reported as not started", collect)
	}
	if cleanup.Task == nil || cleanup.Task.State != "succeeded" || cleanup.NotStarted || cleanup.When != model.WorkflowWhenAlways {
		t.Fatal("completed cleanup misreported", cleanup)
	}
	if report.Complete {
		t.Fatal("failed workflow reported complete")
	}
	if f.calls.Load() != 2 {
		t.Fatal("unexpected provider runs", f.calls.Load())
	}
}

// A coordinator that does not advertise always steps would run cleanup as an
// ordinary step. The client refuses before preparing any grant or task.
func TestWorkflowCleanupRefusedByLegacyCoordinator(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	defer cancel()
	const common = "legacy-coordinator-worker-key-1234567890"
	g, err := gateway.New(t.TempDir(), common, gateway.Options{SuperuserKey: testAccount})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = g.Close() })
	server := httptest.NewServer(g.Handler())
	t.Cleanup(server.Close)
	options := transporttest.Loopback()
	var calls atomic.Int32
	for _, name := range []string{"legacy", "worker"} {
		n, err := New(Config{Name: name, Gateway: server.URL, Token: common, DataDir: t.TempDir(), WorkDir: t.TempDir(), RelayOnly: true}, options)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = n.Close() })
		if err := n.Register(provider{cap: model.Capability{Name: "test.lifetime", InputSchema: schema(nil)}, run: func(context.Context, json.RawMessage, Execution) (any, error) {
			calls.Add(1)
			return "done", nil
		}}); err != nil {
			t.Fatal(err)
		}
		if name == "legacy" {
			// The schema advertised before always steps existed.
			n.providers["workflow.run"] = provider{cap: model.Capability{Name: "workflow.run", InputSchema: schema(map[string]any{"steps": map[string]any{"type": "array", "items": map[string]any{"type": "object"}}})}, run: n.workflowRun}
		}
		if err := n.Start(ctx); err != nil {
			t.Fatal(err)
		}
	}
	c := (client.Client{}).WithStandalone(ctx, client.StandaloneConfig{Gateway: client.Admin{URL: server.URL, Key: testAccount},
		StateDir: t.TempDir(), RelayOnly: true, AccountRouting: true, PeerOptions: []transport.Options{options}})
	t.Cleanup(func() { _ = c.Close() })
	plan := func(when string) model.TaskSpec {
		return model.TaskSpec{ID: "legacy-" + when, Capability: "workflow.run", Args: model.JSON(map[string]any{"steps": []model.WorkflowStep{
			{Name: "work", Target: "worker", Task: lifetimeTask("legacy-work-"+when, false)},
			{Name: "cleanup", Target: "worker", When: when, Needs: []string{"work"}, Task: lifetimeTask("legacy-cleanup-"+when, false)},
		}})}
	}
	for _, method := range []string{"tasks.start", "workflow.run"} {
		var err error
		if method == "tasks.start" {
			err = c.Call(ctx, "legacy", method, plan(model.WorkflowWhenAlways), nil)
		} else {
			var args map[string]any
			_ = json.Unmarshal(plan(model.WorkflowWhenAlways).Args, &args)
			err = c.Call(ctx, "legacy", method, args, nil)
		}
		if err == nil || !strings.Contains(err.Error(), "does not advertise always") {
			t.Fatalf("%s: legacy coordinator accepted cleanup steps: %v", method, err)
		}
	}
	var grants []model.Delegation
	if err := c.Call(ctx, "worker", "access.list", nil, &grants); err != nil || len(grants) != 0 {
		t.Fatal("refused plan prepared grants", grants, err)
	}
	if err := c.Call(ctx, "legacy", "tasks.get", map[string]string{"id": "legacy-always"}, nil); err == nil || !strings.Contains(err.Error(), "task not found") {
		t.Fatal("refused plan created a task", err)
	}
	// Plans without cleanup steps remain compatible with the older coordinator.
	if err := c.Call(ctx, "legacy", "tasks.start", plan(model.WorkflowWhenSuccess), nil); err != nil {
		t.Fatal(err)
	}
	if parent, err := c.Wait(ctx, "legacy", "legacy-success"); err != nil || parent.State != "succeeded" || calls.Load() != 2 {
		t.Fatal("ordinary plan failed on legacy coordinator", parent.State, parent.Error, err, calls.Load())
	}
}
