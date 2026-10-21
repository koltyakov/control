package client

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/koltyakov/control/internal/model"
)

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
			response.Error = "worker unavailable"
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
}
