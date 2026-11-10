package client

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/koltyakov/control/internal/model"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const discoveryFixture = `[
 {"id":"user-id","name":"Worker","os":"windows","online":true,"lastSeen":"2026-11-10T02:00:00Z","publicKey":"AQ==","system":{"arch":"arm64","memoryTotalBytes":123},"software":{"version":"v-test","arch":"amd64"},"runAs":"user","hybridId":"pair-id","hybridRole":"user","labels":{"app":"example"},"capabilities":[{"name":"exec.run","description":"full description","inputSchema":{"type":"object"}}],"futureField":{"preserve":true}},
 {"id":"system-id","name":"Worker-system","os":"windows","online":false,"lastSeen":"2026-11-09T02:00:00Z","software":{"version":"v-old","arch":"arm64"},"disabled":true,"controlPending":true,"runAs":"system","instructionOnly":true,"hybridId":"pair-id","hybridRole":"system"},
 {"id":"legacy-id","name":"legacy","os":"linux","online":false}
]`

func TestMachineDiscoveryProjection(t *testing.T) {
	t.Parallel()
	data := json.RawMessage(discoveryFixture)
	for _, q := range []MachineQuery{{}, {Compact: false}} {
		got, err := q.project(data)
		if err != nil || string(got) != string(data) {
			t.Fatal("default listing changed full JSON", string(got), err)
		}
	}
	for _, name := range []string{"Worker", "user-id"} {
		got, err := (MachineQuery{Node: name}).project(data)
		if err != nil {
			t.Fatal(err)
		}
		var records []map[string]json.RawMessage
		if err := json.Unmarshal(got, &records); err != nil || len(records) != 1 {
			t.Fatal("named listing must remain an array", string(got), err)
		}
		for _, key := range []string{"capabilities", "system", "labels", "futureField"} {
			if _, ok := records[0][key]; !ok {
				t.Errorf("full named listing lost %s", key)
			}
		}
	}
	got, err := (MachineQuery{Compact: true}).project(data)
	if err != nil {
		t.Fatal(err)
	}
	var summaries []machineSummary
	if err := json.Unmarshal(got, &summaries); err != nil || len(summaries) != 3 {
		t.Fatal("compact listing discarded offline or legacy nodes", string(got), err)
	}
	if summaries[0].ID != "user-id" || summaries[0].Arch != "arm64" || summaries[0].Version != "v-test" || summaries[0].Disabled || summaries[0].ControlPending {
		t.Fatal("incorrect User summary", summaries[0])
	}
	if summaries[1].Online || !summaries[1].Disabled || !summaries[1].ControlPending || !summaries[1].InstructionOnly || summaries[1].HybridID != "pair-id" || summaries[1].HybridRole != "system" || summaries[1].RunAs != "system" || summaries[1].Arch != "arm64" {
		t.Fatal("compact listing lost policy, pairing or fallback architecture", summaries[1])
	}
	if summaries[2].Arch != "" || summaries[2].RunAs != "" || summaries[2].HybridRole != "" {
		t.Fatal("legacy metadata was invented", summaries[2])
	}
	for _, omitted := range []string{"capabilities", "inputSchema", "system", "memoryTotalBytes", "publicKey", "labels", "futureField"} {
		if strings.Contains(string(got), `"`+omitted+`":`) {
			t.Errorf("compact output contains %s", omitted)
		}
	}
	for _, name := range []string{"Worker-system", "system-id"} {
		got, err := (MachineQuery{Node: name, Compact: true}).project(data)
		var selected []machineSummary
		if err != nil || json.Unmarshal(got, &selected) != nil || len(selected) != 1 || selected[0] != summaries[1] {
			t.Fatal("explicit System selection changed", string(got), err)
		}
	}
	for _, name := range []string{"worker", "Work", "missing", "foreign-id"} {
		if _, err := (MachineQuery{Node: name, Compact: true}).project(data); err == nil || !strings.Contains(err.Error(), "unknown machine") {
			t.Errorf("unknown/exact-name query %q did not fail: %v", name, err)
		}
	}
	for _, empty := range []string{"[]", "null"} {
		got, err := (MachineQuery{Compact: true}).project(json.RawMessage(empty))
		if err != nil || string(got) != "[]" {
			t.Errorf("empty compact listing: %s, %v", got, err)
		}
	}
	if _, err := (MachineQuery{Compact: true}).project(json.RawMessage(`{"not":"an array"}`)); err == nil {
		t.Fatal("invalid directory accepted")
	}
	large := json.RawMessage(strings.Replace(discoveryFixture, "full description", strings.Repeat("unrelated capability documentation ", 1000), 1))
	compact, err := (MachineQuery{Compact: true}).project(large)
	if err != nil || len(compact)*10 >= len(large) {
		t.Fatal("compact projection did not remove repeated capability output", len(compact), len(large), err)
	}
}

func TestAdminMachineDiscovery(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != http.MethodGet || r.URL.RequestURI() != "/v1/nodes" || r.Header.Get("Authorization") != "Bearer common-key" {
			t.Errorf("unexpected discovery request: %s %s", r.Method, r.URL)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(discoveryFixture))
	}))
	defer server.Close()
	c := Admin{URL: server.URL, Key: "common-key"}
	got, err := c.Machines(t.Context(), MachineQuery{Node: "Worker", Compact: true})
	if err != nil || !strings.Contains(string(got), `"id":"user-id"`) || calls.Load() != 1 {
		t.Fatal("discovery should make one fleet-scoped request without management authentication", string(got), calls.Load(), err)
	}
	server.Close()
	if _, err := c.Machines(t.Context(), MachineQuery{}); err == nil {
		t.Fatal("gateway failure was hidden")
	}
}

func TestMCPMachineDiscoveryCompatibility(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var call model.APICall
		if r.URL.Path != "/v1/call" || r.Header.Get("Authorization") != "Bearer local-key" || json.NewDecoder(r.Body).Decode(&call) != nil || call.Method != "nodes.list" || call.Target != "" || string(call.Params) != "{}" {
			t.Error("projection options were sent to the backend", call)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(model.Response{Result: json.RawMessage(discoveryFixture)})
	}))
	defer api.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	st, ct := mcp.NewInMemoryTransports()
	ss, err := (Client{URL: api.URL, Token: "local-key"}).MCPServer().Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ss.Close() }()
	session, err := mcp.NewClient(&mcp.Implementation{Name: "discovery-test", Version: "1"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = session.Close() }()
	for _, q := range []MachineQuery{{}, {Node: "Worker"}, {Compact: true}, {Node: "system-id", Compact: true}, {Node: "missing"}} {
		before := calls.Load()
		result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "control_nodes", Arguments: q})
		if err != nil || calls.Load() != before+1 {
			t.Fatal("MCP discovery failed or repeated backend discovery", result, calls.Load(), err)
		}
		want, wantErr := q.project(json.RawMessage(discoveryFixture))
		if wantErr != nil {
			if !result.IsError {
				t.Fatal("unknown named machine did not report a tool error", result)
			}
			continue
		}
		if result.IsError || len(result.Content) != 1 {
			t.Fatal("unexpected MCP result", result)
		}
		text, ok := result.Content[0].(*mcp.TextContent)
		if !ok {
			t.Fatal("expected JSON text", result.Content)
		}
		var actual, expected any
		if json.Unmarshal([]byte(text.Text), &actual) != nil || json.Unmarshal(want, &expected) != nil || !reflect.DeepEqual(actual, expected) {
			t.Fatal("MCP output differs from shared projection", text.Text, string(want))
		}
	}
	for _, invalid := range []map[string]any{{"compact": "true"}, {"node": true}, {"node": ""}, {"unknown": true}} {
		before := calls.Load()
		result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "control_nodes", Arguments: invalid})
		if err == nil && !result.IsError {
			t.Fatal("invalid discovery options accepted", invalid)
		}
		if calls.Load() != before {
			t.Fatal("invalid options contacted backend")
		}
	}
}
