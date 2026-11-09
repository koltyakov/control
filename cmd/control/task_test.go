package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/koltyakov/control/internal/client"
	"github.com/koltyakov/control/internal/model"
)

func captureTaskStderr(t *testing.T) *os.File {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "stderr")
	if err != nil {
		t.Fatal(err)
	}
	previous := os.Stderr
	os.Stderr = f
	t.Cleanup(func() { os.Stderr = previous; _ = f.Close() })
	return f
}

func TestTaskStartWait(t *testing.T) {
	t.Setenv("CONTROL_HOME", t.TempDir())
	for _, tc := range []struct {
		name     string
		flags    []string
		before   bool
		file     bool
		generate bool
		initial  string
		final    string
		gets     int32
	}{
		{name: "acceptance only", initial: "queued"},
		{name: "inline wait after JSON", flags: []string{"--wait"}, initial: "queued", final: "succeeded", gets: 1},
		{name: "inline wait before JSON", flags: []string{"--wait"}, before: true, initial: "queued", final: "succeeded", gets: 1},
		{name: "explicit no wait", flags: []string{"--wait=false"}, initial: "queued"},
		{name: "file compatibility", flags: []string{"--wait"}, file: true, initial: "queued", final: "succeeded", gets: 1},
		{name: "generated ID", flags: []string{"--wait"}, generate: true, initial: "queued", final: "succeeded", gets: 1},
		{name: "failed result", flags: []string{"--wait"}, initial: "queued", final: "failed", gets: 1},
		{name: "cancelled result", flags: []string{"--wait"}, initial: "queued", final: "cancelled", gets: 1},
		{name: "interrupted result", flags: []string{"--wait"}, initial: "queued", final: "interrupted", gets: 1},
		{name: "already succeeded", flags: []string{"--wait"}, initial: "succeeded"},
		{name: "already failed", flags: []string{"--wait"}, initial: "failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stderr := captureTaskStderr(t)
			spec := model.TaskSpec{ID: "leased-task", Capability: "desktop.run", LeaseID: "current-lease", TimeoutSeconds: 120,
				Args: model.JSON(map[string]string{"command": "setValue", "ref": "@snapshot:e1", "secret": "vpn.username"}), Outputs: []string{"result.json"}}
			if tc.generate {
				spec.ID = ""
			}
			var starts, gets atomic.Int32
			var accepted model.Task
			var mu sync.Mutex
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				var call model.APICall
				if err := json.NewDecoder(r.Body).Decode(&call); err != nil {
					t.Error(err)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				if call.Target != "worker" || r.URL.Path != "/v1/call" {
					t.Errorf("unexpected route: %s %s", r.URL.Path, call.Target)
				}
				task := model.Task{}
				switch call.Method {
				case "tasks.start":
					starts.Add(1)
					var got model.TaskSpec
					if err := json.Unmarshal(call.Params, &got); err != nil {
						t.Error(err)
					}
					want := spec
					if tc.generate {
						want.ID = got.ID
					}
					if got.ID == "" || !reflect.DeepEqual(got, want) {
						t.Errorf("specification changed: got %+v, want %+v", got, want)
					}
					printed, err := os.ReadFile(stderr.Name())
					if err != nil || string(printed) != "task "+got.ID+"\n" {
						t.Errorf("ID not printed before submission: %q, %v", printed, err)
					}
					task = model.Task{ID: got.ID, Spec: got, Owner: "owner", State: tc.initial}
					accepted = task
				case "tasks.get":
					gets.Add(1)
					var params struct {
						ID          string `json:"id"`
						WaitSeconds int    `json:"waitSeconds"`
					}
					if err := json.Unmarshal(call.Params, &params); err != nil || params.ID != accepted.ID || params.WaitSeconds <= 0 {
						t.Errorf("wait did not use the accepted ID and blocking get: %+v, %v", params, err)
					}
					task = accepted
					task.State = tc.final
				default:
					t.Errorf("unexpected call %q", call.Method)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				if task.Terminal() {
					task.Result = json.RawMessage(`{"details":{"delivery":"delivered_unverified","retry":"unsafe"}}`)
					if task.State != "succeeded" {
						task.Error = "retained failure details"
					}
				}
				_ = json.NewEncoder(w).Encode(model.Response{Result: model.JSON(task)})
			}))
			t.Cleanup(server.Close)
			value := string(model.JSON(spec))
			if tc.file {
				path := filepath.Join(t.TempDir(), "existing-spec.json")
				if err := os.WriteFile(path, []byte(value), 0600); err != nil {
					t.Fatal(err)
				}
				value = "@" + path
			}
			args := []string{"--api", server.URL, "task", "start", "worker"}
			if tc.before {
				args = append(args, tc.flags...)
			}
			args = append(args, value)
			if !tc.before {
				args = append(args, tc.flags...)
			}
			output, err := captureUpdatePushOutput(t, func() error { return run(context.Background(), args) })
			state := tc.initial
			if tc.gets > 0 {
				state = tc.final
			}
			failed := state == "failed" || state == "cancelled" || state == "interrupted"
			if failed {
				if err == nil || !strings.Contains(err.Error(), "retained failure details") {
					t.Fatalf("failure lost: %v, %s", err, output)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			var task model.Task
			if err := json.Unmarshal([]byte(output), &task); err != nil || task.State != state || task.Spec.LeaseID != spec.LeaseID {
				t.Fatalf("result lost: %+v, %v, %s", task, err, output)
			}
			if task.Terminal() && !strings.Contains(string(task.Result), "delivered_unverified") {
				t.Fatalf("partial result omitted: %s", output)
			}
			if starts.Load() != 1 || gets.Load() != tc.gets {
				t.Fatalf("unexpected submissions/waits: %d/%d", starts.Load(), gets.Load())
			}
		})
	}
}

func TestTaskStartWaitUncertainOutcome(t *testing.T) {
	for _, failure := range []string{"submission", "wait", "wait after observation", "cancelled wait"} {
		t.Run(failure, func(t *testing.T) {
			captureTaskStderr(t)
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			var starts, gets atomic.Int32
			retained := model.Task{ID: "uncertain-task", State: "succeeded", Result: json.RawMessage(`{"effect":"already applied"}`)}
			var mu sync.Mutex
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				var call model.APICall
				if err := json.NewDecoder(r.Body).Decode(&call); err != nil {
					t.Error(err)
					return
				}
				response := model.Response{}
				switch call.Method {
				case "tasks.start":
					starts.Add(1)
					if err := json.Unmarshal(call.Params, &retained.Spec); err != nil {
						t.Error(err)
					}
					if failure == "submission" {
						// Lose the reply after retaining acceptance, not before it.
						dropTaskReply(t, w)
						return
					} else {
						accepted := retained
						accepted.State, accepted.Result = "queued", nil
						response.Result = model.JSON(accepted)
					}
				case "tasks.get":
					count := gets.Add(1)
					var params struct{ ID string }
					if err := json.Unmarshal(call.Params, &params); err != nil || params.ID != retained.ID {
						t.Errorf("reconciliation changed the ID: %+v, %v", params, err)
					}
					if count == 1 && failure == "cancelled wait" {
						cancel()
						<-r.Context().Done()
						return
					}
					if count == 1 && failure == "wait after observation" {
						observed := retained
						observed.State = "running"
						observed.Result = json.RawMessage(`{"progress":"effect may have landed"}`)
						response.Result = model.JSON(observed)
					} else if (count == 1 && failure == "wait") || (count == 2 && failure == "wait after observation") {
						dropTaskReply(t, w)
						return
					} else {
						response.Result = model.JSON(retained)
					}
				default:
					t.Errorf("unexpected retry or cancellation: %s", call.Method)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				_ = json.NewEncoder(w).Encode(response)
			}))
			t.Cleanup(server.Close)
			c := client.Client{URL: server.URL}
			output, err := captureUpdatePushOutput(t, func() error {
				return taskStartCLI(ctx, c, []string{"worker", `{"id":"uncertain-task","capability":"desktop.run","leaseId":"current-lease","args":{"command":"click","ref":"@snapshot:e1"}}`, "--wait"})
			})
			if err == nil || !strings.Contains(err.Error(), retained.ID) || !strings.Contains(err.Error(), "query this ID") {
				t.Fatalf("missing reconciliation guidance: %v", err)
			}
			if failure == "cancelled wait" && !errors.Is(err, context.Canceled) {
				t.Fatalf("cancellation cause lost: %v", err)
			}
			if failure != "submission" {
				var last model.Task
				state := "queued"
				if failure == "wait after observation" {
					state = "running"
				}
				if err := json.Unmarshal([]byte(output), &last); err != nil || last.ID != retained.ID || last.State != state || last.Spec.LeaseID != "current-lease" {
					t.Fatalf("last known acceptance lost: %s, %v", output, err)
				}
				if failure == "wait after observation" && !strings.Contains(string(last.Result), "effect may have landed") {
					t.Fatalf("last observed partial result lost: %s", output)
				}
			}
			wantGets := int32(1)
			switch failure {
			case "submission":
				wantGets = 0
			case "wait after observation":
				wantGets = 2
			}
			if starts.Load() != 1 || gets.Load() != wantGets {
				t.Fatalf("uncertain operation retried: starts=%d gets=%d", starts.Load(), gets.Load())
			}
			// A later explicit read reconciles the original task without submission.
			var task model.Task
			if err := c.Call(context.Background(), "worker", "tasks.get", map[string]string{"id": retained.ID}, &task); err != nil || task.State != "succeeded" || !strings.Contains(string(task.Result), "already applied") {
				t.Fatalf("reconciliation failed: %+v, %v", task, err)
			}
			if starts.Load() != 1 {
				t.Fatal("reconciliation resubmitted work")
			}
		})
	}
}

func dropTaskReply(t *testing.T, w http.ResponseWriter) {
	t.Helper()
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		t.Error("test server does not support dropping the reply")
		return
	}
	conn, _, err := hijacker.Hijack()
	if err != nil {
		t.Error(err)
		return
	}
	_ = conn.Close()
}

func TestTaskStartRejectsInvalidArguments(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusBadRequest)
	}))
	t.Cleanup(server.Close)
	for _, args := range [][]string{
		nil, {"worker"}, {"worker", "--wait"}, {"worker", "{"}, {"worker", "[]", "--wait"},
		{"worker", "{}", "extra"}, {"worker", "{}", "--unknown"}, {"worker", "--unknown", "{}"},
		{"worker", "{}", "--wait=invalid"},
	} {
		if err := taskStartCLI(context.Background(), client.Client{URL: server.URL}, args); err == nil {
			t.Errorf("accepted invalid arguments: %v", args)
		}
	}
	if calls.Load() != 0 {
		t.Fatalf("invalid input reached submission: %d calls", calls.Load())
	}
}
