package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/koltyakov/control/internal/client"
	"github.com/koltyakov/control/internal/model"
)

func TestTaskStartCheck(t *testing.T) {
	spec := `{"id":"checked","capability":"exec.run","args":{"command":"true"},"leaseId":"lease"}`
	for _, tc := range []struct {
		name    string
		args    []string
		reply   model.Response
		fails   string
		printed bool
	}{
		{name: "passing after JSON", args: []string{"worker", spec, "--check"}, printed: true,
			reply: model.Response{Result: model.JSON(model.TaskCheck{Advisory: true, OK: true, TaskID: "checked", Checks: []model.TaskCheckItem{{Name: "spec", Status: "pass"}}, Note: model.TaskCheckAdvisory})}},
		{name: "passing before JSON", args: []string{"worker", "--check", spec}, printed: true,
			reply: model.Response{Result: model.JSON(model.TaskCheck{Advisory: true, OK: true, Note: model.TaskCheckAdvisory})}},
		{name: "failing check", args: []string{"worker", spec, "--check"}, printed: true, fails: "task check failed",
			reply: model.Response{Result: model.JSON(model.TaskCheck{Advisory: true, Checks: []model.TaskCheckItem{{Name: "lease", Status: "fail", Message: "lease expired or does not exist"}}, Note: model.TaskCheckAdvisory})}},
		{name: "older worker", args: []string{"worker", spec, "--check"}, fails: "preflight not supported by this node",
			reply: model.Response{Error: `unknown method or capability "tasks.check"`}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				var call model.APICall
				if err := json.NewDecoder(r.Body).Decode(&call); err != nil || call.Method != model.TaskCheckMethod || call.Target != "worker" {
					t.Errorf("unexpected call %+v %v", call, err)
				}
				var got model.TaskSpec
				if err := json.Unmarshal(call.Params, &got); err != nil || got.ID != "checked" || got.LeaseID != "lease" {
					t.Errorf("specification changed: %s", call.Params)
				}
				_ = json.NewEncoder(w).Encode(tc.reply)
			}))
			t.Cleanup(server.Close)
			output, err := captureUpdatePushOutput(t, func() error {
				return taskStartCLI(context.Background(), client.Client{URL: server.URL}, tc.args)
			})
			if tc.fails == "" && err != nil || tc.fails != "" && (err == nil || !strings.Contains(err.Error(), tc.fails)) {
				t.Fatalf("error %v, want %q", err, tc.fails)
			}
			var report model.TaskCheck
			if decodeErr := json.Unmarshal([]byte(output), &report); (decodeErr == nil && report.Advisory) != tc.printed {
				t.Fatalf("printed report %v: %q", tc.printed, output)
			}
			if calls.Load() != 1 {
				t.Fatalf("%d calls, want one check and no submission", calls.Load())
			}
		})
	}
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls.Add(1) }))
	t.Cleanup(server.Close)
	for _, args := range [][]string{{"worker", spec, "--check", "--wait"}, {"worker", "--spec", "--check", spec}, {"worker", "--check"}, {"worker", "{", "--check"}} {
		if err := taskStartCLI(context.Background(), client.Client{URL: server.URL}, args); err == nil {
			t.Errorf("accepted %v", args)
		}
	}
	if calls.Load() != 0 {
		t.Fatal("invalid check arguments reached the node")
	}
}
