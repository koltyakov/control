package node

import (
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gofrs/flock"
	"github.com/koltyakov/control/internal/model"
)

func TestRPAHelperProcess(t *testing.T) {
	mode := os.Getenv("CONTROL_TEST_RPA")
	if mode == "" {
		return
	}
	var request struct {
		Version   string `json:"version"`
		Workspace string `json:"workspace"`
		Arguments struct {
			Actions []map[string]any `json:"actions"`
		} `json:"arguments"`
	}
	if json.NewDecoder(os.Stdin).Decode(&request) != nil || request.Version != "1" {
		os.Exit(2)
	}
	results := []map[string]any{}
	for i, action := range request.Arguments.Actions {
		item := map[string]any{"type": action["type"], "completed": true}
		if mode == "secret-echo" || mode == "secret-fail" {
			text, _ := action["text"].(string)
			if _, exists := action["secret"]; exists {
				os.Exit(5)
			}
			if text != "" {
				item["echo"] = text
				fmt.Fprint(os.Stderr, text[:len(text)/2])
				fmt.Fprint(os.Stderr, text[len(text)/2:])
				if mode == "secret-fail" {
					os.Exit(6)
				}
			}
		}
		if action["type"] == "screenshot" {
			name := fmt.Sprintf("screen-%d.png", i)
			f, err := os.Create(filepath.Join(request.Workspace, name))
			if err != nil || png.Encode(f, image.NewRGBA(image.Rect(0, 0, 2, 2))) != nil {
				os.Exit(3)
			}
			_ = f.Close()
			if mode == "invalid-png" {
				_ = os.WriteFile(filepath.Join(request.Workspace, name), []byte("not a PNG"), 0600)
			}
			item["image"] = name
			if mode == "escape" {
				item["image"] = "../secret.png"
			}
		}
		results = append(results, item)
	}
	response := map[string]any{"results": results}
	switch mode {
	case "partial":
		response["results"] = results[:1]
		response["error"] = "selector matched 2 elements"
	case "bad-json":
		fmt.Print("not JSON")
		os.Exit(0)
	case "missing-results":
		response = map[string]any{}
	case "bad-count":
		response["results"] = results[:1]
	case "sleep":
		time.Sleep(time.Minute)
	case "serial":
		f, err := os.OpenFile(os.Getenv("CONTROL_TEST_RPA_TRACE"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
		if err != nil {
			os.Exit(4)
		}
		_, _ = f.WriteString("start\n")
		time.Sleep(100 * time.Millisecond)
		_, _ = f.WriteString("end\n")
		_ = f.Close()
	}
	_ = json.NewEncoder(os.Stdout).Encode(response)
	os.Exit(0)
}

func rpaCommand(t *testing.T, mode string) *Command {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return &Command{Command: exe, Args: []string{"-test.run=^TestRPAHelperProcess$"}, Env: map[string]string{"CONTROL_TEST_RPA": mode}}
}

func rpaNode(t *testing.T, mode string) *Node {
	t.Helper()
	n, err := New(Config{Name: "desktop", Gateway: "http://127.0.0.1:1", Token: testToken, DataDir: t.TempDir(), WorkDir: t.TempDir(), RPA: rpaCommand(t, mode)})
	if err != nil {
		t.Fatal(err)
	}
	n.rpaLockPath = filepath.Join(t.TempDir(), "rpa.lock")
	t.Cleanup(func() { _ = n.Close() })
	return n
}

func TestRPAOptInAndResults(t *testing.T) {
	if _, err := New(Config{Name: "bad", Gateway: "http://127.0.0.1:1", Token: testToken, DataDir: t.TempDir(), WorkDir: t.TempDir(), RPA: &Command{}}); err == nil {
		t.Fatal("accepted RPA without a command")
	}
	n := rpaNode(t, "success")
	args := model.JSON(map[string]any{"actions": []any{map[string]any{"type": "screenshot"}}})
	value, err := n.rpaRun(context.Background(), args, Execution{Dir: n.Config.WorkDir, Log: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	response := value.(rpaResponse)
	var artifact model.Artifact
	if err := json.Unmarshal(response.Results[0]["artifact"], &artifact); err != nil || artifact.ID == "" || artifact.Node != n.Identity.ID {
		t.Fatalf("missing screenshot artifact: %+v, %v", response, err)
	}
	if _, exists := response.Results[0]["image"]; exists {
		t.Fatal("temporary screenshot path leaked")
	}
	if _, err := os.Stat(n.artifactPath(artifact.ID)); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(n.Config.DataDir)
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "rpa-") {
			t.Fatal("helper workspace leaked")
		}
	}
}

func TestRPAErrorsAndCancellation(t *testing.T) {
	args := model.JSON(map[string]any{"actions": []any{map[string]any{"type": "screenshot"}, map[string]any{"type": "inspect"}}})
	for _, mode := range []string{"partial", "bad-json", "escape", "sleep", "invalid-png", "missing-results", "bad-count"} {
		t.Run(mode, func(t *testing.T) {
			n := rpaNode(t, mode)
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			value, err := n.rpaRun(ctx, args, Execution{Dir: n.Config.WorkDir, Log: io.Discard})
			if err == nil {
				t.Fatal("expected failure")
			}
			if mode == "partial" {
				response := value.(rpaResponse)
				if len(response.Results) != 1 || len(response.Results[0]["artifact"]) == 0 {
					t.Fatalf("lost partial results: %+v", response)
				}
			}
			lock := flock.New(n.rpaLockPath)
			defer func() { _ = lock.Close() }()
			if locked, err := lock.TryLock(); !locked || err != nil {
				t.Fatalf("desktop lock leaked: %v", err)
			}
		})
	}
}

func TestRPASerializesAcrossProfiles(t *testing.T) {
	one, two := rpaNode(t, "serial"), rpaNode(t, "serial")
	two.rpaLockPath = one.rpaLockPath
	trace := filepath.Join(t.TempDir(), "trace")
	one.Config.RPA.Env["CONTROL_TEST_RPA_TRACE"] = trace
	two.Config.RPA.Env["CONTROL_TEST_RPA_TRACE"] = trace
	var wg sync.WaitGroup
	for _, n := range []*Node{one, two} {
		wg.Go(func() {
			_, err := n.rpaRun(context.Background(), json.RawMessage(`{"actions":[{"type":"inspect"}]}`), Execution{Dir: n.Config.WorkDir, Log: io.Discard})
			if err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	b, err := os.ReadFile(trace)
	if err != nil || string(b) != "start\nend\nstart\nend\n" {
		t.Fatalf("overlapping batches: %q, %v", b, err)
	}
}

func TestRPAWaitingCancellationAndValidation(t *testing.T) {
	n := rpaNode(t, "success")
	lock := flock.New(n.rpaLockPath)
	defer func() { _ = lock.Close() }()
	if ok, err := lock.TryLock(); !ok || err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := n.rpaRun(ctx, json.RawMessage(`{"actions":[{"type":"inspect"}]}`), Execution{Dir: n.Config.WorkDir, Log: io.Discard}); err == nil {
		t.Fatal("cancelled waiter executed")
	}
	if _, err := n.rpaRun(context.Background(), json.RawMessage(`{"actions":[{"type":"inspect"},{"type":"unknown"}]}`), Execution{}); err == nil {
		t.Fatal("invalid batch was not rejected before waiting for the lock")
	}
}

func TestRPAPeerTasksAndAuthorization(t *testing.T) {
	for _, relay := range []bool{true, false} {
		t.Run(fmt.Sprintf("relay=%v", relay), func(t *testing.T) {
			source, worker, consumer := cluster(t, relay, func(i int, cfg *Config) {
				if i == 1 {
					cfg.RPA = rpaCommand(t, "success")
					cfg.Allow = map[string][]string{"source": {"rpa.run", "tasks.*", "leases.*"}}
				}
			})
			worker.rpaLockPath = filepath.Join(t.TempDir(), "rpa.lock")
			if _, exists := source.providers["rpa.run"]; exists {
				t.Fatal("RPA enabled by default")
			}
			args := json.RawMessage(`{"actions":[{"type":"inspect"},{"type":"screenshot"}]}`)
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			if err := testCall(t, consumer, ctx, "worker", "rpa.run", args, nil); err == nil {
				t.Fatal("unauthorized caller accessed desktop")
			}
			var lease model.Lease
			call(t, source, "worker", "leases.acquire", map[string]any{}, &lease)
			if err := testCall(t, source, ctx, "worker", "rpa.run", args, nil); err == nil {
				t.Fatal("synchronous GUI call bypassed lease")
			}
			var task model.Task
			spec := model.TaskSpec{ID: "rpa-once", Capability: "rpa.run", Args: args, LeaseID: lease.ID}
			call(t, source, "worker", "tasks.start", spec, &task)
			finished, err := testWaitTask(t, source, ctx, "worker", task.ID)
			if err != nil || finished.State != "succeeded" {
				t.Fatalf("GUI task failed: %+v, %v", finished, err)
			}
			var response rpaResponse
			if err := json.Unmarshal(finished.Result, &response); err != nil || len(response.Results) != 2 || len(response.Results[1]["artifact"]) == 0 {
				t.Fatalf("missing screenshot: %+v, %v", response, err)
			}
			call(t, source, "worker", "tasks.start", spec, &task)
			if task.Updated != finished.Updated {
				t.Fatal("GUI task replayed on resubmission")
			}
			call(t, source, "worker", "leases.release", map[string]any{"id": lease.ID}, nil)
		})
	}
}
