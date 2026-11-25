package client

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/koltyakov/control/internal/model"
)

type recordedCall struct{ target, method string }

// tagAPI is a local node API whose describe answers advertise task tags only
// for the named targets.
func tagAPI(t *testing.T, supported ...string) (Client, func() []recordedCall) {
	t.Helper()
	var mu sync.Mutex
	var calls []recordedCall
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var call model.APICall
		if err := json.NewDecoder(r.Body).Decode(&call); err != nil {
			t.Error(err)
			return
		}
		mu.Lock()
		calls = append(calls, recordedCall{call.Target, call.Method})
		mu.Unlock()
		result := model.JSON(model.Task{ID: "accepted", State: "queued"})
		if call.Method == "node.describe" {
			description := map[string]any{"id": call.Target}
			if slices.Contains(supported, call.Target) {
				description["taskTags"] = model.TaskTagsDescription()
			}
			result = model.JSON(description)
		}
		_ = json.NewEncoder(w).Encode(model.Response{Result: result})
	}))
	t.Cleanup(server.Close)
	return Client{URL: server.URL}, func() []recordedCall {
		mu.Lock()
		defer mu.Unlock()
		out := slices.Clone(calls)
		calls = nil
		return out
	}
}

// An older worker ignores tags, so a tagged task would be accepted untagged
// and later missed by cancel-by-tag. The client refuses before sending.
func TestClientRefusesTagsForWorkersWithoutSupport(t *testing.T) {
	c, calls := tagAPI(t, "new")
	spec := model.TaskSpec{ID: "job", Capability: "exec.run", Tags: []string{"batch"}}

	_, err := c.StartTask(t.Context(), "old", spec, nil)
	var submitErr *TaskSubmitError
	var rpcErr *model.RPCError
	if err == nil || errors.As(err, &submitErr) || !errors.As(err, &rpcErr) || rpcErr.Outcome != "not_sent" || !strings.Contains(err.Error(), "does not advertise task tags") {
		t.Fatal("tagged task to an older worker", err)
	}
	if got := calls(); !slices.Equal(got, []recordedCall{{"old", "node.describe"}}) {
		t.Fatal("refused task reached the worker", got)
	}
	if _, err := c.StartTask(t.Context(), "new", spec, nil); err != nil {
		t.Fatal(err)
	}
	if got := calls(); !slices.Equal(got, []recordedCall{{"new", "node.describe"}, {"new", "tasks.start"}}) {
		t.Fatal("supported submission was described more than once or not sent", got)
	}
	spec.Tags = nil
	if _, err := c.StartTask(t.Context(), "old", spec, nil); err != nil {
		t.Fatal(err)
	}
	if got := calls(); !slices.Equal(got, []recordedCall{{"old", "tasks.start"}}) {
		t.Fatal("untagged submission paid for a support check", got)
	}

	if err := c.Call(t.Context(), "old", "tasks.page", map[string]any{"tag": "batch"}, nil); err == nil {
		t.Fatal("older worker would list every task as matching")
	}
	if err := c.Call(t.Context(), "old", "tasks.page", map[string]any{"limit": 10}, nil); err != nil {
		t.Fatal(err)
	}
	if got := calls(); !slices.Equal(got, []recordedCall{{"old", "node.describe"}, {"old", "tasks.page"}}) {
		t.Fatal("tag filter check", got)
	}

	// Forwarded tasks and workflow steps are checked at their destinations.
	tagged := model.TaskSpec{ID: "child", Capability: "exec.run", Tags: []string{"batch"}}
	if err := c.Call(t.Context(), "new", "peers.call", map[string]any{"target": "old", "method": "tasks.start", "params": tagged}, nil); err == nil {
		t.Fatal("forwarded tagged task reached an older destination")
	}
	plan := model.WorkflowPlan{Steps: []model.WorkflowStep{{Name: "a", Target: "new", Task: tagged}, {Name: "b", Target: "old", Task: model.TaskSpec{ID: "b", Capability: "exec.run", Tags: []string{"batch"}}}}}
	if err := c.Call(t.Context(), "new", "tasks.start", model.TaskSpec{ID: "flow", Capability: "workflow.run", Args: model.JSON(plan)}, nil); err == nil {
		t.Fatal("tagged workflow step reached an older destination")
	}
	if got := calls(); slices.ContainsFunc(got, func(c recordedCall) bool { return c.method != "node.describe" }) {
		t.Fatal("refused requests were sent", got)
	}
}

func TestCompactTaskKeepsTags(t *testing.T) {
	task := model.Task{ID: "job", State: "running", Spec: model.TaskSpec{ID: "job", Capability: "exec.run", Args: model.JSON(map[string]string{"script": "long"}), Tags: []string{"a", "b"}}}
	var compact map[string]json.RawMessage
	if err := json.Unmarshal(WithoutTaskSpec(model.JSON(task)), &compact); err != nil {
		t.Fatal(err)
	}
	if _, ok := compact["spec"]; ok || string(compact["tags"]) != `["a","b"]` || string(compact["id"]) != `"job"` {
		t.Fatal("compact task", compact)
	}
	task.Spec.Tags = nil
	if strings.Contains(string(WithoutTaskSpec(model.JSON(task))), "tags") {
		t.Fatal("untagged compact task gained tags")
	}
}
