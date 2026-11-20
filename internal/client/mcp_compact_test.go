package client

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/koltyakov/control/internal/model"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// compactMCP serves a local API that answers each method from results and
// records the parameters it received.
func compactMCP(t *testing.T, results map[string]string) (*mcp.ClientSession, chan map[string]json.RawMessage) {
	session, requests, _ := compactAPI(t, results)
	return session, requests
}

func compactAPI(t *testing.T, results map[string]string) (*mcp.ClientSession, chan map[string]json.RawMessage, Client) {
	t.Helper()
	requests := make(chan map[string]json.RawMessage, 16)
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request map[string]json.RawMessage
		if json.NewDecoder(r.Body).Decode(&request) != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		requests <- request
		var method string
		_ = json.Unmarshal(request["method"], &method)
		result, ok := results[method]
		if !ok {
			_, _ = w.Write([]byte(`{"error":"unexpected method"}`))
			return
		}
		_, _ = w.Write([]byte(`{"result":` + result + `}`))
	}))
	t.Cleanup(api.Close)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	t.Cleanup(cancel)
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	server, err := (Client{URL: api.URL, Token: "test"}).WithLifetime(ctx).MCPServer().Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })
	session, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil).Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session, requests, Client{URL: api.URL, Token: "test"}
}

func callText(t *testing.T, session *mcp.ClientSession, name string, args map[string]any) string {
	t.Helper()
	result, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatal(name, err)
	}
	text := result.Content[0].(*mcp.TextContent).Text
	if result.IsError {
		t.Fatal(name, text)
	}
	return text
}

func TestMCPTaskResultsOmitSubmittedSpec(t *testing.T) {
	t.Parallel()
	task := `{"id":"t1","owner":"o","spec":{"id":"t1","capability":"exec.run","args":{"command":"sh","args":["-c","a && b"]}},"state":"succeeded","result":{"stdout":"ok"}}`
	session, requests := compactMCP(t, map[string]string{"tasks.start": task, "tasks.get": task, "tasks.cancel": task})
	for _, tool := range []string{"control_task_start", "control_task_get", "control_task_cancel"} {
		args := map[string]any{"node": "worker", "id": "t1"}
		if tool == "control_task_start" {
			args = map[string]any{"node": "worker", "task": map[string]any{"id": "t1", "capability": "exec.run"}}
		}
		text := callText(t, session, tool, args)
		if text != `{"id":"t1","owner":"o","state":"succeeded","result":{"stdout":"ok"}}` {
			t.Fatalf("%s did not omit only the spec, preserving order: %s", tool, text)
		}
		<-requests
	}
	text := callText(t, session, "control_task_get", map[string]any{"node": "worker", "id": "t1", "includeSpec": true})
	if !strings.Contains(text, `"spec":{"id":"t1"`) || !strings.Contains(text, "a && b") {
		t.Fatalf("includeSpec did not return the specification: %s", text)
	}
	if request := <-requests; strings.Contains(string(request["params"]), "includeSpec") {
		t.Fatalf("client-only option reached the worker: %s", request["params"])
	}
}

func TestMCPArtifactsPageOnCurrentAndLegacyWorkers(t *testing.T) {
	t.Parallel()
	id := func(c string) string { return strings.Repeat(c, 64) }
	page := model.ArtifactPage{Node: "node-id", Artifacts: []model.ArtifactSummary{{ID: id("a"), Name: "a.zip", Size: 3}}, Total: 1, TotalBytes: 3}
	session, requests := compactMCP(t, map[string]string{"artifacts.list": string(model.JSON(page))})
	text := callText(t, session, "control_artifacts", map[string]any{"node": "worker", "limit": 5})
	if text != string(model.JSON(page)) {
		t.Fatalf("current worker page changed: %s", text)
	}
	if params := string((<-requests)["params"]); !strings.Contains(params, `"limit":5`) {
		t.Fatalf("page limit was not requested: %s", params)
	}

	// An older worker ignores paging and returns full references.
	var legacy []model.Artifact
	created := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	for i, c := range []string{"a", "b", "c"} {
		legacy = append(legacy, model.Artifact{Node: "node-id", ID: id(c), SHA256: id(c), Name: c, Size: 1, Created: created.Add(time.Duration(i) * time.Hour)})
	}
	session, _ = compactMCP(t, map[string]string{"artifacts.list": string(model.JSON(legacy))})
	var first model.ArtifactPage
	if err := json.Unmarshal([]byte(callText(t, session, "control_artifacts", map[string]any{"node": "worker", "limit": 2})), &first); err != nil {
		t.Fatal(err)
	}
	if first.Node != "node-id" || first.Total != 3 || len(first.Artifacts) != 2 || first.Artifacts[0].Name != "c" || first.NextCursor == "" {
		t.Fatalf("legacy list was not paged newest first: %+v", first)
	}
	var second model.ArtifactPage
	if err := json.Unmarshal([]byte(callText(t, session, "control_artifacts", map[string]any{"node": "worker", "limit": 2, "cursor": first.NextCursor})), &second); err != nil {
		t.Fatal(err)
	}
	if len(second.Artifacts) != 1 || second.Artifacts[0].Name != "a" || second.NextCursor != "" {
		t.Fatalf("legacy continuation failed: %+v", second)
	}
}

func TestDescribeOmitsTransportDiagnosticsUnlessFull(t *testing.T) {
	t.Parallel()
	description := `{"id":"node-id","name":"worker","capabilities":[],"connections":{"peer":"webrtc"},"sessions":[{"id":"s"}],"transport":{"lookupHits":1},"runbooks":{"protocol":"runbooks-v1"}}`
	session, _, c := compactAPI(t, map[string]string{"node.describe": description, "runbooks.list": `{"node":"worker","id":"node-id","sources":[],"runbooks":[]}`})
	text := callText(t, session, "control_describe", map[string]any{"node": ""})
	for _, field := range []string{`"connections"`, `"sessions"`, `"transport"`} {
		if strings.Contains(text, field) {
			t.Fatalf("describe kept %s: %s", field, text)
		}
	}
	if !strings.Contains(text, `"guidance":"`+"No runbooks matched") {
		t.Fatalf("empty catalog kept full guidance: %s", text)
	}
	for _, args := range []map[string]any{{"node": "", "view": "full"}} {
		text = callText(t, session, "control_describe", args)
		for _, field := range []string{`"connections"`, `"sessions"`, `"transport"`} {
			if !strings.Contains(text, field) {
				t.Fatalf("full describe lost %s: %s", field, text)
			}
		}
	}
	var cli map[string]json.RawMessage
	if err := c.Call(t.Context(), "", "node.describe", map[string]any{}, &cli); err != nil {
		t.Fatal(err)
	}
	if _, ok := cli["sessions"]; ok {
		t.Fatal("CLI describe kept transport diagnostics")
	}
}
