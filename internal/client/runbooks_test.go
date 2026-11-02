package client

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/runbooks"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func runbookClientFixture(t *testing.T, protocol bool, deny bool) (Client, *atomic.Int32) {
	t.Helper()
	project, global, workerUser, workerCommon := t.TempDir(), t.TempDir(), t.TempDir(), t.TempDir()
	for _, item := range []struct{ root, file, text string }{
		{project, "shared/vpn/RUNBOOK.md", "# VPN\nProject generic instructions."},
		{global, "shared/vpn.md", "# VPN\nGlobal generic instructions."},
		{workerUser, "machines/trader/vpn.md", "---\nrequires:\n  os: [windows]\n  apps: [globalprotect]\n  capabilities: [rpa.run]\n  vpn:\n    vendor: Palo Alto Networks\n    state: configured\n---\n# Trader VPN\nApplication: GlobalProtect\nReferences: vpn.portal vpn.username vpn.password"},
		{workerCommon, "shared/vpn.md", "# VPN\nWorker common instructions."},
	} {
		writeScenarioFixture(t, filepath.Join(item.root, filepath.FromSlash(item.file)), item.text)
	}
	var calls atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var q model.APICall
		if err := json.NewDecoder(r.Body).Decode(&q); err != nil {
			t.Error(err)
			return
		}
		if q.Target != "trader" && q.Target != "stable-id" {
			t.Error("unexpected target", q.Target)
		}
		w.Header().Set("Content-Type", "application/json")
		switch q.Method {
		case "node.describe":
			result := map[string]any{"id": "stable-id", "name": "trader", "capabilities": []any{}}
			if protocol {
				result["runbooks"] = map[string]any{"protocol": runbooks.Protocol}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"result": result})
		case "runbooks.list", "runbooks.read":
			if deny {
				_, _ = w.Write([]byte(`{"error":"runbooks access denied"}`))
				return
			}
			var query runbooks.Query
			if err := json.Unmarshal(q.Params, &query); err != nil {
				t.Error(err)
				return
			}
			catalog, err := runbooks.Discover(r.Context(), []runbooks.Root{{Source: "worker-user", Path: workerUser}, {Source: "worker-common", Path: workerCommon}}, runbooks.Selector{Node: "trader", ID: "stable-id"}, query)
			if err != nil {
				t.Error(err)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"result": catalog})
		default:
			t.Error("unexpected method", q.Method)
		}
	}))
	t.Cleanup(s.Close)
	return Client{URL: s.URL, Token: "test", runbookRoots: []runbooks.Root{{Source: "project", Path: project}, {Source: "global", Path: global}}}, &calls
}

func TestClientRunbooksDiscoveryAndExactRead(t *testing.T) {
	c, calls := runbookClientFixture(t, true, false)
	ctx := context.Background()
	for _, q := range []runbooks.Query{{}, {Node: "trader", Local: true}} {
		catalog, err := c.Runbooks(ctx, q)
		if err != nil || len(catalog.Runbooks) != 2 || calls.Load() != 0 {
			t.Fatalf("local discovery: %+v, %v", catalog, err)
		}
	}
	catalog, err := c.Runbooks(ctx, runbooks.Query{Node: "trader"})
	if err != nil || len(catalog.Runbooks) != 4 || len(catalog.Sources) != 4 || calls.Load() != 2 {
		t.Fatalf("combined discovery: %+v, %v", catalog, err)
	}
	if !catalog.Runbooks[0].Preferred || catalog.Runbooks[0].Machine != "trader" || catalog.Runbooks[0].ConflictCount != 3 {
		t.Fatalf("machine procedure not preferred: %+v", catalog.Runbooks)
	}
	if !reflect.DeepEqual(catalog.Runbooks[0].Requires, map[string]any{"os": []any{"windows"}, "apps": []any{"globalprotect"}, "capabilities": []any{"rpa.run"}, "vpn": map[string]any{"vendor": "Palo Alto Networks", "state": "configured"}}) {
		t.Fatalf("remote applicability metadata omitted: %+v", catalog.Runbooks[0])
	}
	read, err := c.Runbooks(ctx, runbooks.Query{Node: "trader", Source: "worker-user", File: "machines/trader/vpn.md"})
	if err != nil || len(read.Runbooks) != 1 || !strings.Contains(read.Runbooks[0].Content, "GlobalProtect") {
		t.Fatalf("worker read: %+v, %v", read, err)
	}
	before := calls.Load()
	if _, err := c.Runbooks(ctx, runbooks.Query{Source: "project", Node: "trader", File: "shared/vpn/RUNBOOK.md"}); err != nil || calls.Load() != before {
		t.Fatal("project read required remote access", err)
	}
	var description struct {
		Runbooks runbooks.Catalog `json:"runbooks"`
	}
	if err := c.Call(ctx, "stable-id", "node.describe", map[string]any{}, &description); err != nil || len(description.Runbooks.Runbooks) != 4 {
		t.Fatalf("automatic describe discovery: %+v, %v", description, err)
	}
}

func TestRunbooksIgnoreRetiredProjectDirectory(t *testing.T) {
	t.Chdir(t.TempDir())
	// Leave a valid legacy document present to catch accidental fallback reads.
	legacy := filepath.Join(".control-scenarios", "trader", "vpn.md")
	writeScenarioFixture(t, legacy, "# VPN\nRetired instructions must never be discovered.")
	c := Client{}
	query := runbooks.Query{Node: "trader", Source: "project"}
	catalog, err := c.Runbooks(context.Background(), query)
	if err != nil || len(catalog.Runbooks) != 0 || len(catalog.Sources) != 1 || catalog.Sources[0].Directory != runbooks.ProjectDirectory || catalog.Sources[0].Available {
		t.Fatalf("legacy directory was used instead of the missing new root: %+v, %v", catalog, err)
	}
	query.File = "machines/trader/vpn.md"
	if _, err := c.Runbooks(context.Background(), query); err == nil {
		t.Fatal("complete read fell back to the retired directory")
	}
	current := filepath.Join(runbooks.ProjectDirectory, "machines", "trader", "vpn.md")
	writeScenarioFixture(t, current, "# VPN\nCurrent runbook instructions.")
	query.File = ""
	catalog, err = c.Runbooks(context.Background(), query)
	if err != nil || len(catalog.Runbooks) != 1 || !strings.Contains(catalog.Runbooks[0].Content, "Current runbook instructions.") || strings.Contains(catalog.Runbooks[0].Content, "Retired instructions") {
		t.Fatalf("discovery did not exclusively use the new layout: %+v, %v", catalog, err)
	}
}

func TestClientRunbooksUnavailableAndDeniedWorker(t *testing.T) {
	for _, protocol := range []bool{false, true} {
		c, calls := runbookClientFixture(t, protocol, true)
		catalog, err := c.Runbooks(context.Background(), runbooks.Query{Node: "trader"})
		if err != nil || len(catalog.Runbooks) != 2 || catalog.Sources[len(catalog.Sources)-1].Error == "" {
			t.Fatalf("worker failure hid local instructions: %+v, %v", catalog, err)
		}
		wantCalls := int32(1)
		if protocol {
			wantCalls = 2
		}
		if calls.Load() != wantCalls {
			t.Fatal("unsupported worker was probed or call retried", calls.Load())
		}
		if _, err := c.Runbooks(context.Background(), runbooks.Query{Node: "trader", Source: "worker-user", File: "machines/trader/vpn.md"}); err == nil {
			t.Fatal("denied exact read fell back")
		}
	}
	c, _ := runbookClientFixture(t, false, true)
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(api.Close)
	c.URL = api.URL
	catalog, err := c.Runbooks(context.Background(), runbooks.Query{Node: "trader"})
	if err != nil || len(catalog.Runbooks) != 2 || catalog.Sources[len(catalog.Sources)-1].Error == "" {
		t.Fatalf("worker API failure hid local runbooks: %+v, %v", catalog, err)
	}
}

func TestMCPRunbooksAndDescribe(t *testing.T) {
	c, _ := runbookClientFixture(t, true, false)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
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
	for _, call := range []mcp.CallToolParams{
		{Name: "control_describe", Arguments: map[string]any{"node": "stable-id"}},
		{Name: "control_call", Arguments: map[string]any{"node": "trader", "method": "node.describe"}},
		{Name: "control_runbooks", Arguments: map[string]any{"node": "trader"}},
		{Name: "control_runbooks", Arguments: map[string]any{"node": "trader", "source": "worker-user", "file": "machines/trader/vpn.md"}},
	} {
		result, err := session.CallTool(ctx, &call)
		if err != nil || result.IsError {
			t.Fatalf("%s: %+v, %v", call.Name, result, err)
		}
		body := result.Content[0].(*mcp.TextContent).Text
		if !strings.Contains(body, "GlobalProtect") || !strings.Contains(body, "vpn.password") || !strings.Contains(body, "worker-user") {
			t.Fatalf("missing runbook discovery in %s: %s", call.Name, body)
		}
		var catalog runbooks.Catalog
		if call.Name == "control_runbooks" {
			if err := json.Unmarshal([]byte(body), &catalog); err != nil {
				t.Fatal(err)
			}
		} else {
			var description struct {
				Runbooks runbooks.Catalog `json:"runbooks"`
			}
			if err := json.Unmarshal([]byte(body), &description); err != nil {
				t.Fatal(err)
			}
			catalog = description.Runbooks
		}
		found := false
		for _, book := range catalog.Runbooks {
			if reflect.DeepEqual(book.Requires, map[string]any{"os": []any{"windows"}, "apps": []any{"globalprotect"}, "capabilities": []any{"rpa.run"}, "vpn": map[string]any{"vendor": "Palo Alto Networks", "state": "configured"}}) {
				found = true
			}
		}
		if !found {
			t.Fatalf("%s omitted structured applicability metadata", call.Name)
		}
	}
	result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "control_runbooks", Arguments: map[string]any{}})
	if err != nil || result.IsError || !strings.Contains(result.Content[0].(*mcp.TextContent).Text, "Project generic instructions") {
		t.Fatalf("shared discovery required a node: %+v, %v", result, err)
	}
}
