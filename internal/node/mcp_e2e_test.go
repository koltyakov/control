package node

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/koltyakov/control/internal/client"
	"github.com/koltyakov/control/internal/model"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Exercise both MCP adapters, destination-owned delegation, durable records,
// account-owner recovery and logs over actual authenticated peer connections.
func TestMCPWorkflowRecoveryEndToEnd(t *testing.T) {
	for _, relay := range []bool{false, true} {
		t.Run(fmt.Sprintf("relay=%t", relay), func(t *testing.T) {
			t.Parallel()
			var calls atomic.Int32
			tools := mcp.NewServer(&mcp.Implementation{Name: "fixture", Version: "1"}, nil)
			tools.AddTool(&mcp.Tool{Name: "operation", InputSchema: map[string]any{"type": "object"}}, func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				calls.Add(1)
				var q struct{ Fail, Large bool }
				if err := json.Unmarshal(req.Params.Arguments, &q); err != nil {
					return nil, err
				}
				text := "retained tool output"
				if q.Large {
					text = strings.Repeat(text, 70000)
				}
				return &mcp.CallToolResult{IsError: q.Fail, Content: []mcp.Content{&mcp.TextContent{Text: text}}}, nil
			})
			server := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return tools }, nil))
			defer server.Close()
			source, worker, consumer := cluster(t, relay, func(_ int, cfg *Config) {
				cfg.MCP = map[string]Command{"fixture": {URL: server.URL}}
			})
			// Close nodes before their configured MCP server, including on failure.
			defer func() { _ = source.Close(); _ = worker.Close(); _ = consumer.Close() }()
			ctx, cancel := context.WithTimeout(t.Context(), 40*time.Second)
			defer cancel()
			fixture := testOrchestrator(t, source)
			c := fixture.client
			serverTransport, clientTransport := mcp.NewInMemoryTransports()
			front, err := c.MCPServer().Connect(ctx, serverTransport, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = front.Close() }()
			session, err := mcp.NewClient(&mcp.Implementation{Name: "agent-test", Version: "1"}, nil).Connect(ctx, clientTransport, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = session.Close() }()
			invoke := func(name string, args any, wantError bool, output any) {
				t.Helper()
				result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
				if err != nil || result == nil || result.IsError != wantError || len(result.Content) != 1 {
					t.Fatalf("%s: result=%+v err=%v", name, result, err)
				}
				text, ok := result.Content[0].(*mcp.TextContent)
				if !ok {
					t.Fatalf("%s: unexpected content %T", name, result.Content[0])
				}
				if output != nil {
					if err := json.Unmarshal([]byte(text.Text), output); err != nil {
						t.Fatalf("%s: %v", name, err)
					}
				}
			}
			for _, fail := range []bool{false, true} {
				var rpc model.RPCError
				var output any
				if fail {
					output = &rpc
				}
				invoke("control_mcp_call", map[string]any{"node": "worker", "server": "fixture", "tool": "operation", "arguments": map[string]bool{"fail": fail}}, fail, output)
				if fail && (rpc.Code != "mcp_tool_failed" || rpc.Outcome != "unknown") {
					t.Fatalf("tool failure lost across MCP/peer adapters: %+v", rpc)
				}
			}
			steps := []WorkflowStep{
				{Name: "fail", Target: "worker", Task: model.TaskSpec{ID: "failed-child", Capability: "mcp.call", Args: model.JSON(map[string]any{"server": "fixture", "tool": "operation", "arguments": map[string]bool{"fail": true, "large": true}})}},
				{Name: "blocked", Target: "consumer", Needs: []string{"fail"}, Task: model.TaskSpec{ID: "blocked-child", Capability: "files.list"}},
			}
			spec := model.TaskSpec{ID: "failed-workflow", Capability: "workflow.run", Args: model.JSON(map[string]any{"steps": steps})}
			var accepted model.Task
			invoke("control_task_start", map[string]any{"node": "source", "task": spec}, false, &accepted)
			finished, err := c.Wait(ctx, "source", accepted.ID)
			if err != nil || finished.State != "failed" {
				t.Fatalf("workflow ignored remote tool failure: %+v %v", finished, err)
			}
			var child model.Task
			invoke("control_task_get", map[string]any{"node": "worker", "id": "failed-child"}, false, &child)
			if child.State != "failed" || child.ResultArtifact == nil || len(child.Result) != 0 {
				t.Fatalf("failed large result was not retained as an artifact: %+v", child)
			}
			var payload bytes.Buffer
			if err := c.Download(ctx, *child.ResultArtifact, 0, &payload); err != nil {
				t.Fatal(err)
			}
			var result mcp.CallToolResult
			if err := json.Unmarshal(payload.Bytes(), &result); err != nil || !result.IsError || result.Content[0].(*mcp.TextContent).Text != strings.Repeat("retained tool output", 70000) {
				t.Fatal("failed result artifact lost partial output", err)
			}
			if err := c.Call(ctx, "consumer", "tasks.get", map[string]string{"id": "blocked-child"}, nil); err == nil {
				t.Fatal("dependent task executed after remote failure")
			}
			var recovery client.WorkflowReconciliation
			invoke("control_workflow_reconcile", map[string]string{"node": "source", "id": spec.ID}, false, &recovery)
			if recovery.Complete || len(recovery.Steps) != 2 || recovery.Steps[0].Task == nil || recovery.Steps[0].Task.State != "failed" || recovery.Steps[1].Error == "" {
				t.Fatalf("failed workflow recovery: %+v", recovery)
			}
			var log bytes.Buffer
			if err := c.FollowLogs(ctx, "source", spec.ID, 0, &log); err != nil || !strings.Contains(log.String(), "task=failed-child") || strings.Contains(log.String(), "task=blocked-child") {
				t.Fatalf("workflow log stream: %q %v", log.String(), err)
			}
			var tail bytes.Buffer
			if err := c.FollowLogs(ctx, "source", spec.ID, int64(log.Len()/2), &tail); err != nil || tail.String() != log.String()[log.Len()/2:] {
				t.Fatalf("workflow log offset: %q %v", tail.String(), err)
			}
			invoke("control_task_start", map[string]any{"node": "source", "task": spec}, false, &accepted)
			if calls.Load() != 3 {
				t.Fatalf("failed tool was replayed: %d calls", calls.Load())
			}
			// A successful plan spans more children than the reconciliation pool.
			steps = make([]WorkflowStep, 12)
			for i := range steps {
				steps[i] = WorkflowStep{Name: fmt.Sprintf("ok-%02d", i), Target: []string{"worker", "consumer"}[i%2], Task: model.TaskSpec{Capability: "files.list"}}
			}
			spec = model.TaskSpec{ID: "successful-workflow", Capability: "workflow.run", Args: model.JSON(map[string]any{"steps": steps, "maxParallel": 4})}
			invoke("control_task_start", map[string]any{"node": "source", "task": spec}, false, &accepted)
			finished, err = c.Wait(ctx, "source", spec.ID)
			if err != nil || finished.State != "succeeded" {
				t.Fatalf("parallel workflow: %+v %v", finished, err)
			}
			// A fresh account client recovers under the same stable task owner.
			reconnected := (client.Client{}).WithStandalone(ctx, fixture.config)
			defer func() { _ = reconnected.Close() }()
			recovery, err = reconnected.ReconcileWorkflow(ctx, "source", spec.ID)
			if err != nil || !recovery.Complete || len(recovery.Steps) != len(steps) {
				t.Fatalf("fresh-client recovery: %+v %v", recovery, err)
			}
			for i, observed := range recovery.Steps {
				if observed.Name != steps[i].Name || observed.ID == "" || observed.Task == nil || observed.Task.State != "succeeded" {
					t.Fatalf("lost child ID or plan order: %+v", observed)
				}
			}
			if err := testOrchestrator(t, consumer).client.FollowLogs(ctx, "source", spec.ID, 0, &bytes.Buffer{}); err == nil {
				t.Fatal("another owner read workflow logs")
			}
			mode := "webrtc"
			if relay {
				mode = "relay"
			}
			info, err := c.Session(ctx)
			if err != nil || info.Connections[worker.Identity.ID] != mode || source.Peer.Connections()[worker.Identity.ID] != mode || source.Peer.Connections()[consumer.Identity.ID] != mode {
				t.Fatalf("workflow did not use %s: client=%+v coordinator=%+v err=%v", mode, info.Connections, source.Peer.Connections(), err)
			}
		})
	}
}
