package client

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/koltyakov/control/internal/model"
)

func cleanupPlan(when string) map[string]any {
	return map[string]any{"steps": []model.WorkflowStep{
		{Name: "work", Target: "worker", Task: model.TaskSpec{ID: "work", Capability: "exec.run"}},
		{Name: "cleanup", Target: "worker", When: when, Needs: []string{"work"}, Task: model.TaskSpec{ID: "cleanup", Capability: "exec.run"}},
	}}
}

// Through the local API the node forwards the plan unchanged, so the client
// checks the coordinator's advertised schema before submitting it.
func TestLocalWorkflowCleanupRequiresCoordinatorSupport(t *testing.T) {
	for _, supported := range []bool{false, true} {
		item := map[string]any{"type": "object"}
		if supported {
			item["properties"] = map[string]any{"when": model.WorkflowWhenSchema()}
		}
		capabilities := []model.Capability{{Name: "workflow.run", InputSchema: model.JSON(map[string]any{"type": "object", "properties": map[string]any{"steps": map[string]any{"type": "array", "items": item}}})}}
		var mu sync.Mutex
		var methods []string
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var call model.APICall
			if err := json.NewDecoder(r.Body).Decode(&call); err != nil {
				t.Error(err)
				return
			}
			mu.Lock()
			methods = append(methods, call.Method)
			mu.Unlock()
			result := model.JSON(map[string]string{"id": "parent"})
			if call.Method == "capabilities.list" {
				result = model.JSON(capabilities)
			}
			_ = json.NewEncoder(w).Encode(model.Response{Result: result})
		}))
		c := Client{URL: server.URL}
		spec := model.TaskSpec{ID: "parent", Capability: "workflow.run", Args: model.JSON(cleanupPlan(model.WorkflowWhenAlways))}
		taskErr := c.Call(t.Context(), "coordinator", "tasks.start", spec, nil)
		callErr := c.Call(t.Context(), "coordinator", "workflow.run", cleanupPlan(model.WorkflowWhenAlways), nil)
		plainErr := c.Call(t.Context(), "coordinator", "workflow.run", cleanupPlan(""), nil)
		server.Close()
		if plainErr != nil {
			t.Fatal("plan without cleanup steps was checked or refused", plainErr)
		}
		if supported {
			if taskErr != nil || callErr != nil || !slices.Equal(methods, []string{"capabilities.list", "tasks.start", "capabilities.list", "workflow.run", "workflow.run"}) {
				t.Fatal("supported coordinator refused cleanup plan", taskErr, callErr, methods)
			}
			continue
		}
		for _, err := range []error{taskErr, callErr} {
			if err == nil || !strings.Contains(err.Error(), "does not advertise always") {
				t.Fatal("unsupported coordinator accepted cleanup plan", err)
			}
		}
		if !slices.Equal(methods, []string{"capabilities.list", "capabilities.list", "workflow.run"}) {
			t.Fatal("refused plan reached the coordinator", methods)
		}
	}
}

func TestWorkflowAlwaysSupportDetection(t *testing.T) {
	for _, tc := range []struct {
		name   string
		schema any
		want   bool
	}{
		{"legacy", map[string]any{"properties": map[string]any{"steps": map[string]any{"items": map[string]any{"type": "object"}}}}, false},
		{"success only", map[string]any{"properties": map[string]any{"steps": map[string]any{"items": map[string]any{"properties": map[string]any{"when": map[string]any{"enum": []string{"success"}}}}}}}, false},
		{"current", map[string]any{"properties": map[string]any{"steps": map[string]any{"items": map[string]any{"properties": map[string]any{"when": model.WorkflowWhenSchema()}}}}}, true},
	} {
		if got := model.WorkflowAlwaysSupported([]model.Capability{{Name: "workflow.run", InputSchema: model.JSON(tc.schema)}}); got != tc.want {
			t.Errorf("%s: supported = %v", tc.name, got)
		}
	}
	if model.WorkflowAlwaysSupported([]model.Capability{{Name: "other", InputSchema: model.JSON(map[string]any{"properties": map[string]any{"steps": map[string]any{"items": map[string]any{"properties": map[string]any{"when": model.WorkflowWhenSchema()}}}}})}}) {
		t.Error("another capability's schema advertised workflow support")
	}
}

// notStarted requires a terminal coordinator with a retained result lacking
// the step, and the destination's own not-found reply. Anything else stays an
// ordinary observation error.
func TestWorkflowReconcileNotStartedEvidence(t *testing.T) {
	steps := cleanupPlan(model.WorkflowWhenAlways)["steps"]
	for _, tc := range []struct {
		name   string
		parent model.Task
		reply  model.Response
		want   bool
	}{
		{"terminal and not found", model.Task{State: "failed", Result: model.JSON(map[string]model.Task{"work": {ID: "work", State: "failed"}})}, model.Response{Error: "task not found", Code: "operation_failed", Outcome: "unknown"}, true},
		{"still running", model.Task{State: "running"}, model.Response{Error: "task not found", Code: "operation_failed", Outcome: "unknown"}, false},
		{"recorded by coordinator", model.Task{State: "failed", Result: model.JSON(map[string]model.Task{"cleanup": {ID: "cleanup"}})}, model.Response{Error: "task not found", Code: "operation_failed", Outcome: "unknown"}, false},
		{"other query error", model.Task{State: "failed", Result: model.JSON(map[string]model.Task{})}, model.Response{Error: "task belongs to another caller", Code: "operation_failed", Outcome: "unknown"}, false},
		{"result artifact", model.Task{State: "failed", ResultArtifact: &model.Artifact{ID: "large"}}, model.Response{Error: "task not found", Code: "operation_failed", Outcome: "unknown"}, false},
	} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var call model.APICall
			_ = json.NewDecoder(r.Body).Decode(&call)
			var q struct{ ID string }
			_ = json.Unmarshal(call.Params, &q)
			if call.Method != "tasks.get" {
				t.Errorf("unexpected method %s", call.Method)
			}
			switch q.ID {
			case "parent":
				parent := tc.parent
				parent.ID, parent.Spec = "parent", model.TaskSpec{Capability: "workflow.run", Args: model.JSON(map[string]any{"steps": steps})}
				_ = json.NewEncoder(w).Encode(model.Response{Result: model.JSON(parent)})
			case "work":
				_ = json.NewEncoder(w).Encode(model.Response{Result: model.JSON(model.Task{ID: "work", State: "failed"})})
			default:
				_ = json.NewEncoder(w).Encode(tc.reply)
			}
		}))
		report, err := (Client{URL: server.URL}).ReconcileWorkflow(t.Context(), "coordinator", "parent")
		server.Close()
		if err != nil {
			t.Fatal(tc.name, err)
		}
		cleanup := report.Steps[1]
		if cleanup.NotStarted != tc.want || cleanup.When != model.WorkflowWhenAlways || cleanup.Error == "" || report.Steps[0].NotStarted || report.Complete {
			t.Fatalf("%s: unexpected report %+v", tc.name, report)
		}
	}
}
