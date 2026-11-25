package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/koltyakov/control/internal/model"
)

func TestTaskTagCommands(t *testing.T) {
	var mu sync.Mutex
	var calls []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var call model.APICall
		if err := json.NewDecoder(r.Body).Decode(&call); err != nil {
			t.Error(err)
			return
		}
		mu.Lock()
		calls = append(calls, call.Method+" "+string(call.Params))
		mu.Unlock()
		var result any
		switch call.Method {
		case "node.describe":
			result = map[string]any{"taskTags": model.TaskTagsDescription()}
		case "tasks.page":
			result = model.TaskPage{Tasks: []model.TaskSummary{{ID: "a", State: "running", Tags: []string{"batch"}}}}
		case model.CancelTaggedTasks:
			result = model.TaggedCancellation{Tag: "batch", Tasks: []model.TaggedCancelResult{{ID: "a", Outcome: model.TaggedCancelRequested, State: "running"}, {ID: "b", Outcome: model.TaggedCancelFailed, State: "queued", Error: "no handle"}}, AlreadyTerminal: 2}
		default:
			t.Errorf("unexpected call %s", call.Method)
		}
		_ = json.NewEncoder(w).Encode(model.Response{Result: model.JSON(result)})
	}))
	t.Cleanup(server.Close)
	ctx := context.Background()
	output, err := captureUpdatePushOutput(t, func() error {
		return run(ctx, []string{"--api", server.URL, "task", "list", "worker", "--tag", "batch", "--limit", "5"})
	})
	if err != nil || !strings.Contains(output, `"batch"`) {
		t.Fatal(output, err)
	}
	output, err = captureUpdatePushOutput(t, func() error {
		return run(ctx, []string{"--api", server.URL, "task", "cancel", "worker", "--tag", "batch"})
	})
	if err == nil || !strings.Contains(err.Error(), "1 of 2") || !strings.Contains(output, `"alreadyTerminal": 2`) {
		t.Fatal("failed outcomes must be printed and fail the command", output, err)
	}
	if err := run(ctx, []string{"--api", server.URL, "task", "cancel", "worker", "--tag", "bad tag"}); err == nil {
		t.Fatal("invalid tag sent")
	}
	want := []string{`node.describe {}`, `tasks.page {"cursor":"","limit":5,"tag":"batch"}`, `tasks.cancelTagged {"tag":"batch"}`}
	mu.Lock()
	defer mu.Unlock()
	if !slices.Equal(calls, want) {
		t.Fatalf("calls %q", calls)
	}
}
