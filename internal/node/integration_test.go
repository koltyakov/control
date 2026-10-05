package node

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/koltyakov/control/internal/gateway"
	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/store"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const testToken = "integration-test-token-123456789"

func cluster(t *testing.T, relayOnly bool, configure func(int, *Config)) (*Node, *Node, *Node) {
	t.Helper()
	g, err := gateway.New(t.TempDir(), testToken)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(g.Handler())
	t.Cleanup(server.Close)
	t.Cleanup(func() { _ = g.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	nodes := []*Node{}
	for i, name := range []string{"source", "worker", "consumer"} {
		cfg := Config{Name: name, Gateway: server.URL, Token: testToken, DataDir: t.TempDir(), WorkDir: t.TempDir(), RelayOnly: relayOnly, Labels: map[string]string{"role": name}}
		if configure != nil {
			configure(i, &cfg)
		}
		n, err := New(cfg)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = n.Close() })
		if err = n.Start(ctx); err != nil {
			t.Fatal(err)
		}
		nodes = append(nodes, n)
	}
	return nodes[0], nodes[1], nodes[2]
}

func call(t *testing.T, n *Node, target, method string, params, result any) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := n.Call(ctx, target, method, params, result); err != nil {
		t.Fatalf("%s on %s: %v", method, target, err)
	}
}

// Child processes exercise real executable execution on all supported OSes.
func TestWorkerProcess(t *testing.T) {
	switch os.Getenv("CONTROL_TEST_WORKER") {
	case "encode":
		b, err := os.ReadFile("input.bin")
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		if err = os.WriteFile("output.bin", bytes.ToUpper(b), 0600); err != nil {
			os.Exit(3)
		}
		fmt.Print("encoded\n")
		os.Exit(0)
	case "sleep":
		time.Sleep(time.Minute)
		os.Exit(0)
	case "delayed":
		time.Sleep(time.Second)
		fmt.Print("completed after submitter disconnect")
		os.Exit(0)
	case "provider":
		var request struct {
			Version   string         `json:"version"`
			Arguments map[string]any `json:"arguments"`
		}
		if json.NewDecoder(os.Stdin).Decode(&request) != nil || request.Version != "1" {
			os.Exit(2)
		}
		_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"echo": request.Arguments})
		os.Exit(0)
	case "agent":
		b, _ := io.ReadAll(os.Stdin)
		fmt.Print("agent received: " + string(b))
		os.Exit(0)
	}
}

func executable(t *testing.T, mode string) Command {
	t.Helper()
	path, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return Command{Command: path, Args: []string{"-test.run=^TestWorkerProcess$"}, Env: map[string]string{"CONTROL_TEST_WORKER": mode}}
}

func TestThreeNodeExecutionAndDelivery(t *testing.T) {
	for _, relay := range []bool{true, false} {
		t.Run(fmt.Sprintf("relay=%v", relay), func(t *testing.T) {
			source, worker, consumer := cluster(t, relay, nil)
			payload := bytes.Repeat([]byte("video-frame-content\n"), 120000)
			if err := os.WriteFile(filepath.Join(source.Config.WorkDir, "input.bin"), payload, 0600); err != nil {
				t.Fatal(err)
			}
			var input model.Artifact
			call(t, source, "source", "artifacts.export", map[string]any{"path": "input.bin"}, &input)
			var granted model.Artifact
			call(t, source, "source", "artifacts.grant", map[string]any{"id": input.ID, "target": "worker"}, &granted)
			spec := model.TaskSpec{ID: "encode-once", Capability: "exec.run", Args: model.JSON(executable(t, "encode")), Inputs: []model.Input{{Artifact: granted, Path: "input.bin"}}, Outputs: []string{"output.bin"}}
			var submitted model.Task
			call(t, source, "worker", "tasks.start", spec, &submitted)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			finished, err := source.WaitTask(ctx, "worker", submitted.ID)
			if err != nil {
				t.Fatal(err)
			}
			if finished.State != "succeeded" || len(finished.Artifacts) != 1 {
				t.Fatalf("task failed: %+v", finished)
			}
			var duplicate model.Task
			call(t, source, "worker", "tasks.start", spec, &duplicate)
			if duplicate.Updated != finished.Updated {
				t.Fatal("idempotent submission started another execution")
			}
			var delivered model.Artifact
			call(t, source, "worker", "artifacts.deliver", map[string]any{"id": finished.Artifacts[0].ID, "target": "consumer"}, &delivered)
			if delivered.Node != consumer.Identity.ID {
				t.Fatal("artifact not owned by consumer")
			}
			actual, err := os.ReadFile(consumer.artifactPath(delivered.ID))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(actual, bytes.ToUpper(payload)) {
				t.Fatal("output corrupt")
			}
			if _, err := os.Stat(source.artifactPath(delivered.ID)); !os.IsNotExist(err) {
				t.Fatal("output bytes passed through orchestrator storage")
			}
			mode := "webrtc"
			if relay {
				mode = "relay"
			}
			if got := worker.Peer.Connections()[consumer.Identity.ID]; got != mode {
				t.Fatalf("expected %s to consumer, got %q", mode, got)
			}
			if got := worker.Peer.Connections()[source.Identity.ID]; got != mode {
				t.Fatalf("expected %s to source, got %q", mode, got)
			}
		})
	}
}

func TestFallbackCancellationAndOwnership(t *testing.T) {
	source, worker, consumer := cluster(t, false, func(i int, cfg *Config) {
		if i == 1 {
			cfg.RelayOnly = true
		}
	})
	var task model.Task
	call(t, source, "worker", "tasks.start", model.TaskSpec{ID: "cancel-me", Capability: "exec.run", Args: model.JSON(executable(t, "sleep"))}, &task)
	if got := source.Peer.Connections()[worker.Identity.ID]; got != "relay" {
		t.Fatalf("expected relay fallback, got %s", got)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := consumer.Call(ctx, "worker", "tasks.get", map[string]any{"id": task.ID}, nil); err == nil {
		t.Fatal("another owner read task")
	}
	call(t, source, "worker", "tasks.cancel", map[string]any{"id": task.ID}, nil)
	finished, err := source.WaitTask(ctx, "worker", task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if finished.State != "cancelled" {
		t.Fatalf("expected cancelled, got %s", finished.State)
	}
}

func TestArtifactResumeGrantAndFilesystemBoundary(t *testing.T) {
	source, worker, consumer := cluster(t, true, func(i int, cfg *Config) {
		if i == 0 {
			cfg.Allow = map[string][]string{}
		}
	})
	payload := bytes.Repeat([]byte("resume"), 100000)
	if err := os.WriteFile(filepath.Join(source.Config.WorkDir, "file.bin"), payload, 0600); err != nil {
		t.Fatal(err)
	}
	var a model.Artifact
	call(t, source, "source", "artifacts.export", map[string]any{"path": "file.bin"}, &a)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := worker.pullArtifact(ctx, a); err == nil {
		t.Fatal("unauthorized artifact transfer succeeded")
	}
	var granted model.Artifact
	call(t, source, "source", "artifacts.grant", map[string]any{"id": a.ID, "target": "worker"}, &granted)
	if _, err := consumer.pullArtifact(ctx, granted); err == nil {
		t.Fatal("grant accepted from wrong subject")
	}
	if err := os.MkdirAll(filepath.Dir(worker.artifactPath(a.ID)), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(worker.artifactPath(a.ID)+".partial", payload[:12345], 0600); err != nil {
		t.Fatal(err)
	}
	local, err := worker.pullArtifact(ctx, granted)
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(worker.artifactPath(local.ID))
	if err != nil || !bytes.Equal(b, payload) {
		t.Fatal("resumed transfer differs", err)
	}
	if err := source.Call(ctx, "worker", "files.read", map[string]any{"path": "../outside"}, nil); err == nil {
		t.Fatal("filesystem traversal accepted")
	}
	if err := source.Call(ctx, "worker", "artifacts.export", map[string]any{"path": "../outside"}, nil); err == nil {
		t.Fatal("artifact traversal accepted")
	}
}

func TestProvidersHTTPAndTCP(t *testing.T) {
	providerCmd, agentCmd := executable(t, "provider"), executable(t, "agent")
	source, _, _ := cluster(t, true, func(i int, cfg *Config) {
		if i == 1 {
			cfg.Providers = map[string]Command{"custom.echo": providerCmd}
			cfg.Agents = map[string]Command{"test": agentCmd}
		}
	})
	var custom map[string]any
	call(t, source, "worker", "custom.echo", map[string]any{"value": "works"}, &custom)
	if custom["echo"].(map[string]any)["value"] != "works" {
		t.Fatal(custom)
	}
	var agent commandResult
	call(t, source, "worker", "agent.run", map[string]any{"agent": "test", "prompt": "do work"}, &agent)
	if agent.Stdout != "agent received: do work" {
		t.Fatal(agent)
	}
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("private-api")) }))
	defer api.Close()
	var response struct {
		Body string `json:"body"`
	}
	call(t, source, "worker", "http.request", map[string]any{"url": api.URL}, &response)
	b, _ := base64.StdEncoding.DecodeString(response.Body)
	if string(b) != "private-api" {
		t.Fatal(string(b))
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	go func() {
		conn, e := listener.Accept()
		if e != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		_, _ = io.Copy(conn, conn)
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := source.OpenTCP(ctx, "worker", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err = conn.Write([]byte("database protocol")); err != nil {
		t.Fatal(err)
	}
	b = make([]byte, len("database protocol"))
	if _, err = io.ReadFull(conn, b); err != nil {
		t.Fatal(err)
	}
	if string(b) != "database protocol" {
		t.Fatal("tunnel corrupt")
	}
}

func TestMCPProxyAndDurableRestart(t *testing.T) {
	server := mcp.NewServer(&mcp.Implementation{Name: "fixture", Version: "1"}, nil)
	mcp.AddTool(server, &mcp.Tool{Name: "echo", Description: "Echo a value"}, func(ctx context.Context, req *mcp.CallToolRequest, args struct {
		Value string `json:"value"`
	}) (*mcp.CallToolResult, map[string]any, error) {
		return nil, map[string]any{"value": args.Value}, nil
	})
	endpoint := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server { return server }, nil))
	t.Cleanup(endpoint.Close)
	source, worker, _ := cluster(t, true, func(i int, cfg *Config) {
		if i == 1 {
			cfg.MCP = map[string]Command{"fixture": {URL: endpoint.URL}}
		}
	})
	var discovery struct {
		Tools []json.RawMessage `json:"tools"`
	}
	call(t, source, "worker", "mcp.discover", map[string]any{"server": "fixture"}, &discovery)
	if len(discovery.Tools) != 1 {
		t.Fatal("missing MCP tool")
	}
	for i := 0; i < 2; i++ {
		var result mcp.CallToolResult
		call(t, source, "worker", "mcp.call", map[string]any{"server": "fixture", "tool": "echo", "arguments": map[string]any{"value": "hello"}}, &result)
		if result.IsError || !strings.Contains(string(model.JSON(result)), "hello") {
			t.Fatalf("MCP result: %+v", result)
		}
	}
	// Simulate a process dying after accepting work. A restart must never replay.
	if err := worker.Close(); err != nil {
		t.Fatal(err)
	}
	pending := model.Task{ID: "crashed", State: "running", Owner: source.Identity.ID, Spec: model.TaskSpec{Capability: "exec.run"}}
	if err := store.Write(worker.taskPath(pending.ID), pending); err != nil {
		t.Fatal(err)
	}
	restarted, err := New(worker.Config)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = restarted.Close() }()
	if got := restarted.tasks["crashed"].State; got != "interrupted" {
		t.Fatalf("got %s after restart", got)
	}
}
