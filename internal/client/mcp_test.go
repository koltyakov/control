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

func TestMCPAndSkillInstructions(t *testing.T) {
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
	skill := strings.ReplaceAll(string(skills.Control), "\r\n", "\n")
	for name, text := range map[string]string{
		"mcp":        session.InitializeResult().Instructions,
		"skill LF":   skill,
		"skill CRLF": strings.ReplaceAll(skill, "\n", "\r\n"),
	} {
		// Windows checkouts can embed CRLF; these checks concern content, not line endings.
		text = strings.ReplaceAll(text, "\r\n", "\n")
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
		required := []string{
			"runbooks", "control_runbooks", ".control/runbooks/",
			"requires", "agent-friendly", "plain",
			"before ad-hoc probing", "contentOmitted", "uncertain actions",
			"https://github.com/vercel-labs/agent-desktop",
			"version", "desktop", "platform", "data.content", "status",
			"exec.run", "structured", "worker", "--headed", "explicit user permission",
			"control secrets set NAME", "secret", "setValue", "profile",
			"exec.run output is not secret-masked",
			"desktop.run", "control_desktop", "private", "masked",
			"suggest installation", "After approval", "latest compatible release", "agent-desktop-ffi-*",
			"https://github.com/vercel-labs/agent-desktop/releases",
			"SHA-256", "checksums.txt", "same release",
			"ABI major 6 or 7", "96-byte AdAction", "reject incompatible libraries", "before credential use",
			"companions outside workDir", "desktop.library",
			"other settings", "while idle", "separate approval",
		}
		if name == "mcp" {
			required = append(required,
				"npx skills add https://github.com/vercel-labs/agent-desktop --skill agent-desktop",
				"never substitute a shell credential workaround",
				"credentials are no longer visible", "not RPA selectors")
		} else {
			required = append(required,
				"Only when a task requires GUI interaction",
				"skip these checks and skill loads for non-GUI tasks",
				"https://github.com/koltyakov/control/blob/main/docs/rpa.md",
				"stop for manual login", "never retrieve credentials through a shell")
			_, gui, found := strings.Cut(text, "## GUI work, only when needed\n")
			gui, _, _ = strings.Cut(gui, "\n## ")
			if !found || len(strings.Fields(gui)) > 200 {
				t.Errorf("%s should keep GUI guidance brief and load details on demand", name)
			}
		}
		// The skill uses Markdown code spans; MCP uses plain instructions.
		plain := strings.ReplaceAll(text, "`", "")
		for _, fragment := range required {
			if !strings.Contains(plain, fragment) {
				t.Errorf("%s instructions omit remote desktop guidance %q", name, fragment)
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
	result, err = session.CallTool(ctx, &mcp.CallToolParams{Name: "control_rpa", Arguments: map[string]any{
		"node": "desktop",
		"actions": []any{map[string]any{
			"type": "setValue", "target": map[string]any{"app": "Login", "name": "Password"}, "secret": "app.password",
		}},
	}})
	if err != nil || result.IsError {
		t.Fatalf("credential handoff failed: %+v, %v", result, err)
	}
	select {
	case request := <-requests:
		if string(request["target"]) != `"desktop"` || string(request["method"]) != `"rpa.run"` {
			t.Fatalf("credential handoff changed worker or capability: %v", request)
		}
		var params struct {
			Actions []map[string]json.RawMessage `json:"actions"`
		}
		if err := json.Unmarshal(request["params"], &params); err != nil {
			t.Fatal(err)
		}
		if len(params.Actions) != 1 || string(params.Actions[0]["secret"]) != `"app.password"` || params.Actions[0]["text"] != nil {
			t.Fatalf("credential reference was changed: %s", request["params"])
		}
	case <-ctx.Done():
		t.Fatal("credential handoff did not route a request")
	}
	result, err = session.CallTool(ctx, &mcp.CallToolParams{Name: "control_desktop", Arguments: map[string]any{
		"node": "desktop", "request": map[string]any{"command": "setValue", "ref": "@s123:e1", "secret": "app.password"},
	}})
	if err != nil || result.IsError {
		t.Fatalf("native desktop tool failed: %+v, %v", result, err)
	}
	select {
	case request := <-requests:
		if string(request["target"]) != `"desktop"` || string(request["method"]) != `"desktop.run"` ||
			!strings.Contains(string(request["params"]), `"secret":"app.password"`) || strings.Contains(string(request["params"]), `"text"`) {
			t.Fatal("native desktop did not route a reference-only request")
		}
	case <-ctx.Done():
		t.Fatal("native desktop did not route a request")
	}
	result, err = session.CallTool(ctx, &mcp.CallToolParams{Name: "control_desktop", Arguments: map[string]any{
		"node": "desktop", "request": map[string]any{"command": "screenshot"},
	}})
	if err == nil && !result.IsError {
		t.Fatal("native desktop tool accepted an unprotected command")
	}
}
