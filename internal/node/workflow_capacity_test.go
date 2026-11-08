package node

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/transport/transporttest"
)

func workflowTestPool(limit, inUse, queued int) model.ConcurrencyPool {
	return model.ConcurrencyPool{Limit: limit, InUse: inUse, Queued: &queued}
}

func workflowTestStep(name, target, capability string) WorkflowStep {
	return WorkflowStep{Name: name, Target: target, Task: model.TaskSpec{ID: name, Capability: capability, Args: model.JSON(map[string]any{}), TimeoutSeconds: 3600}}
}

func TestWorkflowCapacityOrdering(t *testing.T) {
	free, busy := workflowTestPool(1, 0, 0), workflowTestPool(1, 1, 0)
	for _, tc := range []struct {
		name    string
		steps   []WorkflowStep
		reports map[string]*model.NodeConcurrency
		active  map[string]WorkflowStep
		want    int
	}{
		{"idle destination", []WorkflowStep{workflowTestStep("a", "busy", "files.list"), workflowTestStep("b", "busy", "files.list"), workflowTestStep("c", "idle", "files.list")}, map[string]*model.NodeConcurrency{"busy": {Operations: busy}, "idle": {Operations: free}}, nil, 2},
		{"in-flight headroom", []WorkflowStep{workflowTestStep("a", "one", "exec.run"), workflowTestStep("b", "two", "exec.run")}, map[string]*model.NodeConcurrency{"one": {Execution: free}, "two": {Execution: free}}, map[string]WorkflowStep{"accepted": workflowTestStep("accepted", "one", "exec.run")}, 1},
		{"different pools", []WorkflowStep{workflowTestStep("a", "one", "files.list"), workflowTestStep("b", "one", "exec.run")}, map[string]*model.NodeConcurrency{"one": {Execution: busy, Operations: free}}, map[string]WorkflowStep{"accepted": workflowTestStep("accepted", "one", "exec.run")}, 0},
		{"legacy queues unknown", []WorkflowStep{workflowTestStep("a", "legacy", "exec.run"), workflowTestStep("b", "idle", "exec.run")}, map[string]*model.NodeConcurrency{"legacy": {Execution: model.ConcurrencyPool{Limit: 1}}, "idle": {Execution: free}}, nil, 1},
		{"failed probes stable", []WorkflowStep{workflowTestStep("a", "one", "exec.run"), workflowTestStep("b", "two", "exec.run")}, nil, nil, 0},
		{"unknown before busy", []WorkflowStep{workflowTestStep("a", "busy", "exec.run"), workflowTestStep("b", "unknown", "exec.run")}, map[string]*model.NodeConcurrency{"busy": {Execution: busy}}, nil, 1},
		{"all busy stable", []WorkflowStep{workflowTestStep("a", "one", "exec.run"), workflowTestStep("b", "two", "exec.run")}, map[string]*model.NodeConcurrency{"one": {Execution: busy}, "two": {Execution: busy}}, nil, 0},
		{"queue consumes headroom", []WorkflowStep{workflowTestStep("a", "one", "exec.run"), workflowTestStep("b", "two", "exec.run")}, map[string]*model.NodeConcurrency{"one": {Execution: workflowTestPool(4, 0, 1)}, "two": {Execution: free}}, nil, 1},
		{"desktop separate", []WorkflowStep{workflowTestStep("a", "one", "rpa.run"), workflowTestStep("b", "one", "files.list")}, map[string]*model.NodeConcurrency{"one": {Desktop: &busy, Operations: free}}, nil, 1},
		{"absent desktop unknown", []WorkflowStep{workflowTestStep("a", "one", "desktop.run"), workflowTestStep("b", "two", "desktop.run")}, map[string]*model.NodeConcurrency{"one": {Execution: free}, "two": {Execution: free, Desktop: &free}}, nil, 1},
		{"custom desktop may use execution", []WorkflowStep{workflowTestStep("a", "one", "desktop.run"), workflowTestStep("b", "two", "desktop.run")}, map[string]*model.NodeConcurrency{"one": {Execution: busy, Desktop: &free}, "two": {Execution: free, Desktop: &free}}, nil, 1},
		{"in-flight desktop may use execution", []WorkflowStep{workflowTestStep("a", "one", "exec.run"), workflowTestStep("b", "two", "exec.run")}, map[string]*model.NodeConcurrency{"one": {Execution: free}, "two": {Execution: free}}, map[string]WorkflowStep{"accepted": workflowTestStep("accepted", "one", "desktop.run")}, 1},
		{"staging headroom", []WorkflowStep{{Name: "a", Target: "one", Task: model.TaskSpec{Capability: "files.list", Inputs: []model.Input{{Path: "input"}}}}, workflowTestStep("b", "one", "files.list")}, map[string]*model.NodeConcurrency{"one": {Operations: free, InputStaging: &busy}}, nil, 1},
		{"in-flight staging", []WorkflowStep{{Name: "a", Target: "one", Task: model.TaskSpec{Capability: "files.list", Inputs: []model.Input{{Path: "input"}}}}, workflowTestStep("b", "one", "files.list")}, map[string]*model.NodeConcurrency{"one": {Operations: free, InputStaging: &free}}, map[string]WorkflowStep{"accepted": {Target: "one", Task: model.TaskSpec{Capability: "exec.run", Inputs: []model.Input{{Path: "input"}}}}}, 1},
		{"absent staging unknown", []WorkflowStep{{Name: "a", Target: "one", Task: model.TaskSpec{Capability: "files.list", Inputs: []model.Input{{Path: "input"}}}}, workflowTestStep("b", "one", "files.list")}, map[string]*model.NodeConcurrency{"one": {Operations: free}}, nil, 1},
		{"invalid reading unknown", []WorkflowStep{workflowTestStep("a", "one", "exec.run"), workflowTestStep("b", "two", "exec.run")}, map[string]*model.NodeConcurrency{"one": {Execution: workflowTestPool(0, 0, 0)}, "two": {Execution: free}}, nil, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := string(model.JSON(tc.steps))
			if got := workflowNextStep(tc.steps, tc.reports, tc.active); got != tc.want {
				t.Fatalf("selected %d, want %d", got, tc.want)
			}
			if string(model.JSON(tc.steps)) != before {
				t.Fatal("ordering changed task identity, destination or specification")
			}
		})
	}
}

func TestWorkflowReadyLookaheadAndCapacityBounds(t *testing.T) {
	var steps []WorkflowStep
	if err := json.Unmarshal([]byte(`[{"name":"started"},{"name":"dependency","needs":["unfinished"]},{"name":"input","inputs":[{"step":"unfinished"}]}]`), &steps); err != nil {
		t.Fatal(err)
	}
	for i := range 100 {
		steps = append(steps, workflowTestStep(fmt.Sprint(i), fmt.Sprint(i/2), "exec.run"))
	}
	candidates := workflowReadySteps(steps, map[string]bool{"started": true}, nil)
	if len(candidates) != workflowReadyLookahead || candidates[0].Name != "0" || candidates[len(candidates)-1].Name != "31" {
		t.Fatal("lookahead included blocked/started steps or exceeded bound", candidates)
	}
	var mu sync.Mutex
	calls := map[string]int{}
	reports := workflowCapacityReports(t.Context(), candidates, func(ctx context.Context, target string) *model.NodeConcurrency {
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > workflowCapacityTimeout {
			t.Error("unbounded discovery lifetime")
		}
		mu.Lock()
		calls[target]++
		mu.Unlock()
		return nil
	})
	if len(calls) != workflowCapacityTargets || len(reports) != workflowCapacityTargets {
		t.Fatal("destination query bound", calls, reports)
	}
	for i := range workflowCapacityTargets {
		if calls[fmt.Sprint(i)] != 1 {
			t.Fatal("destination not deduplicated in plan order", calls)
		}
	}
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	var joined atomic.Int32
	workflowCapacityReports(ctx, candidates, func(ctx context.Context, target string) *model.NodeConcurrency {
		defer joined.Add(1)
		<-ctx.Done()
		return nil
	})
	if joined.Load() != workflowCapacityTargets {
		t.Fatal("discovery returned without joining cancelled readers", joined.Load())
	}
}

func TestWorkflowIdleDestinationNotBlockedByQueuedSteps(t *testing.T) {
	for _, relay := range []bool{false, true} {
		t.Run(fmt.Sprintf("relay=%v", relay), func(t *testing.T) {
			source, worker, consumer := cluster(t, relay, func(i int, cfg *Config) { cfg.MaxOperations = 1 }, transporttest.Loopback())
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			release, err := acquireProviderSlot(ctx, worker.operationsPool)
			if err != nil {
				t.Fatal(err)
			}
			var once sync.Once
			unblock := func() { once.Do(release) }
			defer unblock()
			steps := []WorkflowStep{workflowTestStep("queued-one", "worker", "files.list"), workflowTestStep("queued-two", "worker", "files.list"), workflowTestStep("idle", "consumer", "files.list")}
			var parent model.Task
			call(t, source, "source", "tasks.start", model.TaskSpec{ID: "capacity-workflow", Capability: "workflow.run", Args: model.JSON(map[string]any{"steps": steps, "maxParallel": 2})}, &parent)
			// The idle child must finish while both earlier destinations remain
			// blocked. Waiting for parent completion would miss this regression.
			eventually(t, ctx, func() bool {
				consumer.mu.Lock()
				defer consumer.mu.Unlock()
				return consumer.tasks["idle"] != nil && consumer.tasks["idle"].State == "succeeded"
			})
			eventually(t, ctx, func() bool { return *worker.operationsPool.snapshot().Queued == 2 })
			owner := testOrchestrator(t, source).owner.ID
			accepted := map[string]model.Task{}
			worker.mu.Lock()
			for _, step := range steps[:2] {
				accepted[step.Name] = cloneTask(worker.tasks[step.Task.ID])
			}
			worker.mu.Unlock()
			unblock()
			finished, err := testWaitTask(t, source, ctx, "source", parent.ID)
			if err != nil || finished.State != "succeeded" {
				t.Fatal("workflow failed", finished, err)
			}
			var results map[string]model.Task
			if err := json.Unmarshal(finished.Result, &results); err != nil || len(results) != 3 {
				t.Fatal("missing started results", results, err)
			}
			for _, step := range steps {
				got := results[step.Name]
				if got.ID != step.Task.ID || got.Owner != owner || got.State != "succeeded" || !model.SameJSON(model.JSON(got.Spec), model.JSON(step.Task)) {
					t.Fatal("changed accepted task", got)
				}
				if prior, ok := accepted[step.Name]; ok && !prior.Created.Equal(got.Created) {
					t.Fatal("accepted task was replaced", prior, got)
				}
			}
			for _, destination := range []*Node{worker, consumer} {
				want := "webrtc"
				if relay {
					want = "relay"
				}
				if got := source.Peer.Connections()[destination.Identity.ID]; got != want {
					t.Fatalf("transport=%s, want %s", got, want)
				}
			}
		})
	}
}

func TestWorkflowCapacityFailureDrainsStartedChildren(t *testing.T) {
	n := localNode(t, nil)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	siblingStarted, allowSibling, failed := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	if err := n.Register(provider{cap: model.Capability{Name: "test.workflow", InputSchema: schema(nil)}, run: func(ctx context.Context, args json.RawMessage, e Execution) (any, error) {
		calls.Add(1)
		var name string
		_ = json.Unmarshal(args, &name)
		if name == "bad" {
			select {
			case <-siblingStarted:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			close(failed)
			return nil, errors.New("planned failure")
		}
		close(siblingStarted)
		select {
		case <-allowSibling:
			return nil, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}}); err != nil {
		t.Fatal(err)
	}
	steps := []WorkflowStep{}
	for _, name := range []string{"bad", "sibling", "unstarted", "dependent"} {
		step := workflowTestStep(name, "", "test.workflow")
		step.Task.Args = model.JSON(name)
		if name == "dependent" {
			step.Needs = []string{"bad"}
		}
		steps = append(steps, step)
	}
	type outcome struct {
		result any
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		result, err := n.workflowRun(ctx, model.JSON(map[string]any{"steps": steps, "maxParallel": 2}), Execution{Log: io.Discard})
		done <- outcome{result, err}
	}()
	select {
	case <-failed:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	// Wait until the failed child's durable outcome is visible, then let its
	// already accepted sibling complete. Neither later step may be submitted.
	pipelineFinished(t, n, ctx, n.Identity.ID, "bad", "failed")
	close(allowSibling)
	select {
	case got := <-done:
		results := got.result.(map[string]model.Task)
		if got.err == nil || len(results) != 2 || results["sibling"].State != "succeeded" || calls.Load() != 2 {
			t.Fatal("failure did not stop new work or drain sibling", results, got.err, calls.Load())
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}

func TestWorkflowSequentialOrderAndCancellation(t *testing.T) {
	n := localNode(t, nil)
	var calls atomic.Int32
	if err := n.Register(provider{cap: model.Capability{Name: "test.first", InputSchema: schema(nil)}, run: func(context.Context, json.RawMessage, Execution) (any, error) {
		calls.Add(1)
		return nil, errors.New("first step fails")
	}}); err != nil {
		t.Fatal(err)
	}
	steps := []WorkflowStep{workflowTestStep("first", "", "test.first"), workflowTestStep("coordination", "", "peers.call")}
	// Coordination has no provider pool and would rank before the first step
	// if capacity ordering accidentally ran for sequential workflows.
	for _, parallel := range []int{0, 1} {
		steps[0].Task.ID = fmt.Sprintf("first-%d", parallel)
		value, err := n.workflowRun(t.Context(), model.JSON(map[string]any{"steps": steps, "maxParallel": parallel}), Execution{Log: io.Discard})
		results := value.(map[string]model.Task)
		if err == nil || len(results) != 1 || results["first"].State != "failed" {
			t.Fatal("sequential order changed", results, err)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	value, err := n.workflowRun(ctx, model.JSON(map[string]any{"steps": steps, "maxParallel": 2}), Execution{Log: io.Discard})
	if !errors.Is(err, context.Canceled) || len(value.(map[string]model.Task)) != 0 || calls.Load() != 2 {
		t.Fatal("cancelled workflow launched new work", value, calls.Load(), err)
	}
}

func TestWorkflowRefillsBeyondReadyLookahead(t *testing.T) {
	n := localNode(t, nil)
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	steps := []WorkflowStep{}
	for i := range workflowReadyLookahead + 8 {
		steps = append(steps, workflowTestStep(fmt.Sprintf("step-%d", i), "", "files.list"))
	}
	last := workflowTestStep("dependent", "", "files.list")
	last.Needs = []string{steps[len(steps)-1].Name}
	steps = append(steps, last)
	value, err := n.workflowRun(ctx, model.JSON(map[string]any{"steps": steps, "maxParallel": 16}), Execution{Log: io.Discard})
	results := value.(map[string]model.Task)
	if err != nil || len(results) != len(steps) {
		t.Fatal("lookahead lost pending steps", len(results), err)
	}
	if results["dependent"].Created.Before(results[last.Needs[0]].Updated) {
		t.Fatal("refill started a dependent before upstream completion")
	}
}

func TestWorkflowDeniedCapacityDiscoveryIsAdvisory(t *testing.T) {
	source, worker, _ := cluster(t, true, func(i int, cfg *Config) {
		if i == 1 {
			cfg.Allow = map[string][]string{"@orchestrator": {"*"}}
		}
	}, transporttest.Loopback())
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	if err := source.Call(ctx, worker.Identity.ID, "node.describe", map[string]any{}, nil); err == nil {
		t.Fatal("fixture granted discovery to coordinator")
	}
	if report := source.workflowCapacity(ctx, worker.Identity.ID); report != nil {
		t.Fatal("denied discovery became a capacity reading", report)
	}
	steps := []WorkflowStep{workflowTestStep("first", "worker", "files.list"), workflowTestStep("second", "worker", "files.list")}
	var parent model.Task
	call(t, source, "source", "tasks.start", model.TaskSpec{ID: "unknown-capacity", Capability: "workflow.run", Args: model.JSON(map[string]any{"steps": steps, "maxParallel": 2})}, &parent)
	finished, err := testWaitTask(t, source, ctx, "source", parent.ID)
	if err != nil || finished.State != "succeeded" {
		t.Fatal("advisory discovery prevented delegated work", finished, err)
	}
}
