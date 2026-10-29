package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/koltyakov/control/internal/model"
)

func TestWorkflowReconcileBoundsConcurrencyAndJoinsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	entered := make(chan string, 20)
	var active, peak atomic.Int32
	steps := make([]model.WorkflowStep, 20)
	for i := range steps {
		steps[i] = model.WorkflowStep{Name: fmt.Sprintf("step-%02d", i), Target: "worker", Task: model.TaskSpec{ID: fmt.Sprintf("child-%02d", i)}}
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var call model.APICall
		if err := json.NewDecoder(r.Body).Decode(&call); err != nil {
			t.Error(err)
			return
		}
		if call.Method != "tasks.get" {
			t.Errorf("unexpected mutation: %s", call.Method)
			return
		}
		var q struct{ ID string }
		_ = json.Unmarshal(call.Params, &q)
		if q.ID == "parent" {
			_ = json.NewEncoder(w).Encode(model.Response{Result: model.JSON(model.Task{ID: "parent", Spec: model.TaskSpec{Capability: "workflow.run", Args: model.JSON(map[string]any{"steps": steps})}})})
			return
		}
		n := active.Add(1)
		defer active.Add(-1)
		for old := peak.Load(); n > old && !peak.CompareAndSwap(old, n); old = peak.Load() {
		}
		entered <- q.ID
		<-r.Context().Done()
	}))
	defer server.Close()
	defer cancel()
	type outcome struct {
		report WorkflowReconciliation
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		report, err := (Client{URL: server.URL}).ReconcileWorkflow(ctx, "coordinator", "parent")
		done <- outcome{report, err}
	}()
	for range 8 {
		select {
		case <-entered:
		case <-time.After(3 * time.Second):
			t.Fatal("child queries serialized behind an unavailable worker")
		}
	}
	cancel()
	select {
	case result := <-done:
		if !errors.Is(result.err, context.Canceled) || result.report.Complete || len(result.report.Steps) != len(steps) {
			t.Fatalf("incomplete cancellation report: %+v, %v", result.report, result.err)
		}
		for i, step := range result.report.Steps {
			if step.ID != steps[i].Task.ID || step.Error == "" {
				t.Fatalf("lost or reordered unresolved child: %+v", step)
			}
		}
	case <-time.After(3 * time.Second):
		t.Fatal("reconciliation did not join cancelled queries")
	}
	if peak.Load() > 8 {
		t.Fatalf("unbounded child queries: %d", peak.Load())
	}
}

func TestWorkflowReconcileOnlyObservesStableChildren(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var call model.APICall
		if err := json.NewDecoder(r.Body).Decode(&call); err != nil {
			t.Error(err)
			return
		}
		calls.Add(1)
		if call.Method != "tasks.get" {
			t.Errorf("reconciliation sent a mutation: %s", call.Method)
			return
		}
		var q struct {
			ID string `json:"id"`
		}
		_ = json.Unmarshal(call.Params, &q)
		response := model.Response{}
		switch q.ID {
		case "parent":
			response.Result = model.JSON(model.Task{ID: "parent", State: "interrupted", Spec: model.TaskSpec{Capability: "workflow.run", Args: model.JSON(map[string]any{"steps": []model.WorkflowStep{{Name: "done", Target: "worker", Task: model.TaskSpec{ID: "child-a"}}, {Name: "uncertain", Target: "offline", Task: model.TaskSpec{ID: "child-b"}}}})}})
		case "child-a":
			response.Result = model.JSON(model.Task{ID: "child-a", State: "succeeded", Result: model.JSON("private result")})
		case "child-b":
			response.Error = "worker unavailable: " + strings.Repeat("detail", 1000)
			response.Code, response.Outcome = "unavailable", "not_sent"
		default:
			t.Errorf("invented child ID: %s", q.ID)
		}
		_ = json.NewEncoder(w).Encode(response)
	}))
	defer server.Close()
	report, err := (Client{URL: server.URL}).ReconcileWorkflow(t.Context(), "coordinator", "parent")
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 3 || report.Complete || len(report.Steps) != 2 || report.Steps[0].Task.State != "succeeded" || report.Steps[1].Error == "" {
		t.Fatalf("wrong reconciliation: %+v", report)
	}
	if strings.Contains(string(model.JSON(report)), "private result") {
		t.Fatal("report retained payloads")
	}
	if failure := report.Steps[1]; failure.Code != "unavailable" || failure.Outcome != "not_sent" || !failure.ErrorTruncated || len(failure.Error) > 1024 {
		t.Fatalf("unbounded or unclassified failure: %+v", failure)
	}
}

func TestWorkflowReconcileReturnsHealthyChildrenAfterQueryTimeout(t *testing.T) {
	t.Parallel()
	var childCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var call model.APICall
		if err := json.NewDecoder(r.Body).Decode(&call); err != nil {
			t.Error(err)
			return
		}
		if call.Method != "tasks.get" {
			t.Errorf("unexpected mutation: %s", call.Method)
			return
		}
		var q struct{ ID string }
		_ = json.Unmarshal(call.Params, &q)
		var task model.Task
		switch q.ID {
		case "parent":
			task = model.Task{ID: "parent", Spec: model.TaskSpec{Capability: "workflow.run", Args: model.JSON(map[string]any{"steps": []model.WorkflowStep{
				{Name: "unreachable", Target: "slow", Task: model.TaskSpec{ID: "slow-child"}},
				{Name: "healthy", Target: "fast", Task: model.TaskSpec{ID: "fast-child"}},
				{Name: "legacy", Target: "fast"},
			}})}}
		case "slow-child":
			childCalls.Add(1)
			<-r.Context().Done()
			return
		case "fast-child":
			childCalls.Add(1)
			task = model.Task{ID: q.ID, State: "succeeded"}
		default:
			t.Errorf("invented child ID: %s", q.ID)
		}
		_ = json.NewEncoder(w).Encode(model.Response{Result: model.JSON(task)})
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	report, err := (Client{URL: server.URL}).ReconcileWorkflow(ctx, "coordinator", "parent")
	if err != nil {
		t.Fatal("one child timeout discarded the report", err)
	}
	if report.Complete || len(report.Steps) != 3 || childCalls.Load() != 2 {
		t.Fatalf("incorrect partial report: %+v, calls=%d", report, childCalls.Load())
	}
	if report.Steps[0].Code != "deadline_exceeded" || report.Steps[1].Task == nil || report.Steps[1].Task.State != "succeeded" || report.Steps[2].Error == "" {
		t.Fatalf("lost child outcomes or plan ordering: %+v", report)
	}
}
