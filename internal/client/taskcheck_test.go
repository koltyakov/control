package client

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/koltyakov/control/internal/model"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Older nodes reject tasks.check as an unknown method, or deny it by name in
// restrictive access rules. Both reach MCP control_call as a clear error.
func TestTaskCheckOlderWorkerThroughControlCall(t *testing.T) {
	for _, legacy := range []string{
		`unknown method or capability "tasks.check"`,
		"caller is not authorized for tasks.check",
	} {
		t.Run(legacy, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var call model.APICall
				if err := json.NewDecoder(r.Body).Decode(&call); err != nil {
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				if call.Method != model.TaskCheckMethod {
					t.Errorf("check sent %s", call.Method)
				}
				_ = json.NewEncoder(w).Encode(model.ErrorResponse(&model.RPCError{Code: "remote_error", Outcome: "unknown", Message: legacy}))
			}))
			defer server.Close()
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			c := Client{URL: server.URL, Token: "test"}
			_, err := c.CheckTask(ctx, "worker", model.TaskSpec{Capability: "exec.run"})
			var rpc *model.RPCError
			if err == nil || !strings.Contains(err.Error(), "preflight not supported by this node") || !errors.As(err, &rpc) || rpc.Code != taskCheckUnsupported {
				t.Fatalf("older worker error: %v", err)
			}
			serverTransport, clientTransport := mcp.NewInMemoryTransports()
			serverSession, err := c.MCPServer().Connect(ctx, serverTransport, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = serverSession.Close() }()
			session, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil).Connect(ctx, clientTransport, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = session.Close() }()
			result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "control_call", Arguments: map[string]any{"node": "worker", "method": "tasks.check", "params": map[string]any{"capability": "exec.run"}}})
			if err != nil || !result.IsError {
				t.Fatal("control_call accepted an unsupported check", result, err)
			}
			var failure model.RPCError
			if err := json.Unmarshal([]byte(result.Content[0].(*mcp.TextContent).Text), &failure); err != nil || failure.Code != taskCheckUnsupported || !strings.Contains(failure.Message, "preflight not supported") {
				t.Fatal("control_call error", failure, err)
			}
		})
	}
	// Other errors, including current nodes' permission errors, are unchanged.
	if err := taskCheckError(&model.RPCError{Message: "caller is not authorized for tasks.start"}); strings.Contains(err.Error(), "preflight not supported") {
		t.Fatal("current permission error was rewritten", err)
	}
}
