package node

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/koltyakov/control/internal/model"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestMCPToolResultStatus(t *testing.T) {
	server := mcp.NewServer(&mcp.Implementation{Name: "fixture", Version: "1"}, nil)
	server.AddTool(&mcp.Tool{Name: "status", InputSchema: map[string]any{"type": "object"}}, func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var args struct{ Fail bool }
		if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
			return nil, err
		}
		return &mcp.CallToolResult{IsError: args.Fail, Content: []mcp.Content{&mcp.TextContent{Text: strings.Repeat("detail", 1000)}}}, nil
	})
	httpServer := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil))
	defer httpServer.Close()
	n := localNode(t, func(c *Config) { c.MCP = map[string]Command{"tools": {URL: httpServer.URL}} })
	defer func() { _ = n.Close() }()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	for _, fail := range []bool{false, true} {
		result, err := n.mcpCall(ctx, model.JSON(map[string]any{"server": "tools", "tool": "status", "arguments": map[string]bool{"fail": fail}}), Execution{})
		if fail {
			var rpc *model.RPCError
			if !errors.As(err, &rpc) || rpc.Code != "mcp_tool_failed" || rpc.Outcome != "unknown" || len(rpc.Message) > 1040 {
				t.Fatalf("unbounded or unclassified tool failure: %v", err)
			}
		} else if err != nil {
			t.Fatal("successful tool failed", err)
		}
		if result == nil || result.(*mcp.CallToolResult).IsError != fail || len(result.(*mcp.CallToolResult).Content[0].(*mcp.TextContent).Text) != 6000 {
			t.Fatal("original tool result was changed", result)
		}
	}
}

func TestMCPToolFailureStopsWorkflowAndRetainsResult(t *testing.T) {
	var calls atomic.Int32
	server := mcp.NewServer(&mcp.Implementation{Name: "fixture", Version: "1"}, nil)
	server.AddTool(&mcp.Tool{Name: "fail", InputSchema: map[string]any{"type": "object"}}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		calls.Add(1)
		return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: "partial operation failed"}}}, nil
	})
	httpServer := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil))
	defer httpServer.Close()
	n := localNode(t, func(c *Config) { c.MCP = map[string]Command{"tools": {URL: httpServer.URL}} })
	defer func() { _ = n.Close() }()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	steps := []WorkflowStep{
		{Name: "tool", Task: model.TaskSpec{ID: "tool-child", Capability: "mcp.call", Args: model.JSON(map[string]any{"server": "tools", "tool": "fail"})}},
		{Name: "dependent", Needs: []string{"tool"}, Task: model.TaskSpec{ID: "dependent-child", Capability: "files.list"}},
	}
	_, err := n.workflowRun(ctx, model.JSON(map[string]any{"steps": steps}), Execution{Log: io.Discard})
	if err == nil {
		t.Fatal("workflow succeeded after an MCP tool failure")
	}
	var child model.Task
	if err := n.Call(ctx, "", "tasks.get", map[string]any{"id": "tool-child"}, &child); err != nil {
		t.Fatal(err)
	}
	var result mcp.CallToolResult
	if err := json.Unmarshal(child.Result, &result); err != nil {
		t.Fatal(err)
	}
	if child.State != "failed" || !result.IsError || !strings.Contains(string(child.Result), "partial operation failed") {
		t.Fatalf("tool failure was not retained: %+v", child)
	}
	if err := n.Call(ctx, "", "tasks.get", map[string]any{"id": "dependent-child"}, nil); err == nil {
		t.Fatal("dependent task was submitted after tool failure")
	}
	if calls.Load() != 1 {
		t.Fatalf("tool executed %d times", calls.Load())
	}
}

func TestMCPSetupDoesNotBlockOtherServersOrWaiterCancellation(t *testing.T) {
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	slow := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case entered <- struct{}{}:
		default:
		}
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer slow.Close()
	defer close(release)
	server := mcp.NewServer(&mcp.Implementation{Name: "fixture", Version: "1"}, nil)
	fast := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil))
	defer fast.Close()
	n := localNode(t, func(c *Config) { c.MCP = map[string]Command{"slow": {URL: slow.URL}, "fast": {URL: fast.URL}} })
	defer func() { _ = n.Close() }()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { _, err := n.mcpSession(ctx, "slow"); done <- err }()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("setup did not start")
	}
	quick, stop := context.WithTimeout(t.Context(), 3*time.Second)
	defer stop()
	if _, err := n.mcpSession(quick, "fast"); err != nil {
		t.Fatal("unrelated setup blocked", err)
	}
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancelled waiter succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("waiter ignored cancellation")
	}
	n.mcpMu.Lock()
	pending := len(n.mcpPending)
	n.mcpMu.Unlock()
	if pending != 1 {
		t.Fatal("caller cancelled shared setup", pending)
	}
}
