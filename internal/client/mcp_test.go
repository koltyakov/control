package client

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/koltyakov/control/skills"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestMCPAndSkillUseCanonicalTerminology(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverSession, err := (Client{}).MCPServer().Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = serverSession.Close() }()
	session, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil).Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = session.Close() }()
	for name, text := range map[string]string{"mcp": session.InitializeResult().Instructions, "skill": string(skills.Control)} {
		for _, term := range []string{"orchestrator", "worker", "gateway", "client", "node", "AI software"} {
			if !strings.Contains(text, term) {
				t.Errorf("%s instructions omit %q", name, term)
			}
		}
		for _, obsolete := range []string{"fleet agent", "host agent", "Lifecycle-capable agents"} {
			if strings.Contains(text, obsolete) {
				t.Errorf("%s instructions use obsolete terminology %q", name, obsolete)
			}
		}
	}
}

func TestMCPRoutesRPA(t *testing.T) {
	requests := make(chan map[string]json.RawMessage, 1)
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		requests <- request
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"result":{"results":[{"elements":[],"truncated":false}]}}`))
	}))
	defer httpServer.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverSession, err := (Client{URL: httpServer.URL, Token: "test"}).MCPServer().Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = serverSession.Close() }()
	session, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil).Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = session.Close() }()
	result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "control_rpa", Arguments: map[string]any{"node": "desktop", "actions": []any{map[string]any{"type": "inspect"}}}})
	if err != nil || result.IsError {
		t.Fatalf("RPA tool failed: %+v, %v", result, err)
	}
	select {
	case request := <-requests:
		if string(request["target"]) != `"desktop"` || string(request["method"]) != `"rpa.run"` || !strings.Contains(string(request["params"]), `"actions"`) {
			t.Fatalf("incorrect RPA routing: %v", request)
		}
	case <-ctx.Done():
		t.Fatal("RPA tool did not route a request")
	}
	result, err = session.CallTool(ctx, &mcp.CallToolParams{Name: "control_rpa", Arguments: map[string]any{"node": "desktop", "actions": []any{map[string]any{"type": "unknown"}}}})
	if err == nil && !result.IsError {
		t.Fatal("invalid GUI action bypassed MCP schema validation")
	}
}
