package client

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/runbooks"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type vaultRequest struct {
	Op     string
	Params json.RawMessage
}

// fakeVaultPeer records broker operations and answers from respond.
type fakeVaultPeer struct {
	mu       sync.Mutex
	requests []vaultRequest
	respond  func(op string, params json.RawMessage) (json.RawMessage, error)
}

func (p *fakeVaultPeer) Do(_ context.Context, op string, params json.RawMessage) (json.RawMessage, error) {
	p.mu.Lock()
	p.requests = append(p.requests, vaultRequest{Op: op, Params: append(json.RawMessage(nil), params...)})
	respond := p.respond
	p.mu.Unlock()
	if respond == nil {
		return json.RawMessage(`{}`), nil
	}
	return respond(op, params)
}

func (p *fakeVaultPeer) sent() []vaultRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]vaultRequest(nil), p.requests...)
}

// actions omits grant checks, leaving the operations that do work.
func (p *fakeVaultPeer) actions() []vaultRequest {
	var out []vaultRequest
	for _, r := range p.sent() {
		if r.Op != VaultOpCheck {
			out = append(out, r)
		}
	}
	return out
}

func vaultSession(t *testing.T, peer VaultPeer) *mcp.ClientSession {
	t.Helper()
	v, err := NewVault(peer)
	if err != nil {
		t.Fatal(err)
	}
	server, err := v.MCPServer(t.Context(), VaultMCPOptions{Scope: VaultScopeProcess})
	if err != nil {
		t.Fatal(err)
	}
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	ss, err := server.Connect(t.Context(), serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ss.Close() })
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test"}, nil).Connect(t.Context(), clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

func callVaultTool(t *testing.T, cs *mcp.ClientSession, name string, args any) (string, bool) {
	t.Helper()
	result, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Content) != 1 {
		t.Fatalf("%s content = %#v", name, result.Content)
	}
	return result.Content[0].(*mcp.TextContent).Text, result.IsError
}

func TestVaultMCPClassifiesEveryTool(t *testing.T) {
	tools, _, err := vaultCatalog(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	listed := map[string]bool{}
	for _, tool := range tools {
		listed[tool.Name] = true
		count := 0
		if _, ok := vaultRemoteTools[tool.Name]; ok {
			count++
		}
		if _, ok := vaultLocalTools[tool.Name]; ok {
			count++
		}
		if _, ok := vaultUnsupportedTools[tool.Name]; ok {
			count++
		}
		if count != 1 {
			t.Errorf("%s has %d vault classifications, want exactly one", tool.Name, count)
		}
	}
	for _, names := range []map[string]bool{keys(vaultRemoteTools), keys(vaultLocalTools), keys(vaultUnsupportedTools)} {
		for name := range names {
			if !listed[name] {
				t.Errorf("vault classifies %s, which the regular MCP server no longer lists", name)
			}
		}
	}
}

func keys[V any](m map[string]V) map[string]bool {
	out := map[string]bool{}
	for k := range m {
		out[k] = true
	}
	return out
}

func TestVaultMCPListsOriginalSchemasWithoutBroker(t *testing.T) {
	peer := &fakeVaultPeer{respond: func(op string, _ json.RawMessage) (json.RawMessage, error) {
		return nil, errors.New("listing must not contact the broker: " + op)
	}}
	cs := vaultSession(t, peer)
	original, _, err := vaultCatalog(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	schemas := map[string]any{}
	for _, tool := range original {
		schemas[tool.Name] = tool.InputSchema
	}
	count := 0
	for tool, err := range cs.Tools(t.Context(), nil) {
		if err != nil {
			t.Fatal(err)
		}
		count++
		if _, unsupported := vaultUnsupportedTools[tool.Name]; unsupported {
			t.Errorf("unsupported tool %s is exposed", tool.Name)
		}
		if !reflect.DeepEqual(tool.InputSchema, schemas[tool.Name]) {
			t.Errorf("%s schema differs from the regular MCP server", tool.Name)
		}
	}
	if want := len(original) - len(vaultUnsupportedTools); count != want {
		t.Fatalf("listed %d tools, want %d", count, want)
	}
	instructions := cs.InitializeResult().Instructions
	for _, want := range []string{"Approval scope: process", "every request from this MCP server process", "cannot attribute calls to an individual chat or subagent", "control_upload", "Nothing is replayed automatically"} {
		if !strings.Contains(instructions, want) {
			t.Errorf("instructions lack %q", want)
		}
	}
	if len(peer.sent()) != 0 {
		t.Fatalf("listing sent %v", peer.sent())
	}
}

func TestVaultMCPApprovalRequiredIsNotReplayed(t *testing.T) {
	peer := &fakeVaultPeer{respond: func(string, json.RawMessage) (json.RawMessage, error) {
		return nil, &model.RPCError{Code: VaultApprovalRequired, Outcome: "not_sent", Message: "approval_required: grant this connection (code ABCD-EFGH)"}
	}}
	cs := vaultSession(t, peer)
	text, isError := callVaultTool(t, cs, "control_system", map[string]any{"node": "w1"})
	if !isError {
		t.Fatal("pending approval reported success")
	}
	var payload map[string]string
	if err := json.Unmarshal([]byte(text), &payload); err != nil {
		t.Fatal(err)
	}
	if payload["code"] != VaultApprovalRequired || payload["outcome"] != "not_sent" || payload["approvalScope"] != "process" || !strings.Contains(payload["message"], "ABCD-EFGH") || !strings.Contains(payload["guidance"], "only after the user confirms") {
		t.Fatalf("approval payload = %v", payload)
	}
	if sent := peer.sent(); len(sent) != 1 || sent[0].Op != VaultOpCheck {
		t.Fatalf("want one grant check and no action: %v", sent)
	}
}

func TestVaultMCPRoutesTypedOperations(t *testing.T) {
	peer := &fakeVaultPeer{respond: func(op string, params json.RawMessage) (json.RawMessage, error) {
		switch op {
		case VaultOpCall:
			var call VaultCall
			if err := json.Unmarshal(params, &call); err != nil {
				return nil, err
			}
			if call.Method == "tasks.start" {
				var spec model.TaskSpec
				_ = json.Unmarshal(call.Params, &spec)
				return model.JSON(model.Task{ID: spec.ID, State: "queued", Spec: spec}), nil
			}
			return json.RawMessage(`{"ok":true}`), nil
		case VaultOpForwardStart:
			return json.RawMessage(`{"id":"f1","node":"w1","listen":"127.0.0.1:9000","activeConnections":0,"capacityRejections":0}`), nil
		}
		return json.RawMessage(`{"disposed":true}`), nil
	}}
	cs := vaultSession(t, peer)
	if text, isError := callVaultTool(t, cs, "control_call", map[string]any{"node": "w1", "method": "files.list", "params": map[string]any{"path": "."}}); isError || text != `{"ok":true}` {
		t.Fatalf("control_call = %s (error %v)", text, isError)
	}
	text, isError := callVaultTool(t, cs, "control_task_start", map[string]any{"node": "w1", "task": map[string]any{"capability": "exec.run", "args": map[string]any{"command": "true"}}})
	if isError || strings.Contains(text, `"spec"`) {
		t.Fatalf("control_task_start = %s (error %v)", text, isError)
	}
	if _, isError := callVaultTool(t, cs, "control_forward_start", map[string]any{"node": "w1", "address": "127.0.0.1:80"}); isError {
		t.Fatal("forward start failed")
	}
	if _, isError := callVaultTool(t, cs, "control_tunnel_dispose", map[string]any{"id": "t1"}); isError {
		t.Fatal("tunnel dispose failed")
	}
	sent := peer.actions()
	if len(sent) != 4 || len(peer.sent()) != 8 {
		t.Fatalf("sent %v, want one check before each of four actions", peer.sent())
	}
	var call VaultCall
	if err := json.Unmarshal(sent[0].Params, &call); err != nil || sent[0].Op != VaultOpCall || call.Target != "w1" || call.Method != "files.list" || string(call.Params) != `{"path":"."}` {
		t.Fatalf("control_call sent %s %s", sent[0].Op, sent[0].Params)
	}
	var spec model.TaskSpec
	if err := json.Unmarshal(sent[1].Params, &call); err != nil || call.Method != "tasks.start" || json.Unmarshal(call.Params, &spec) != nil || !model.TaskIDPattern.MatchString(spec.ID) {
		t.Fatalf("task start was sent without a client-assigned ID: %s", sent[1].Params)
	}
	var forward ForwardSpec
	if err := json.Unmarshal(sent[2].Params, &forward); err != nil || sent[2].Op != VaultOpForwardStart || forward.Node != "w1" || forward.Address != "127.0.0.1:80" {
		t.Fatalf("forward start sent %s %s", sent[2].Op, sent[2].Params)
	}
	if sent[3].Op != VaultOpTunnelDispose || string(sent[3].Params) != `{"id":"t1"}` {
		t.Fatalf("tunnel dispose sent %s %s", sent[3].Op, sent[3].Params)
	}
}

func TestVaultMCPRefusesBeforeSending(t *testing.T) {
	peer := &fakeVaultPeer{}
	cs := vaultSession(t, peer)
	for _, method := range []string{"shell.open", "tcp.open", "tcp.listen", "artifacts.open", "files.upload.open", "clipboard.paste", "connection.test", "access.grant", "secrets.set", "update.status"} {
		text, isError := callVaultTool(t, cs, "control_call", map[string]any{"node": "w1", "method": method})
		if !isError || !strings.Contains(text, `"outcome":"not_sent"`) {
			t.Errorf("%s = %s (error %v)", method, text, isError)
		}
	}
	if text, isError := callVaultTool(t, cs, "control_system", map[string]any{"node": "w1", "refrsh": true}); !isError || !strings.Contains(text, "unknown argument refrsh") {
		t.Errorf("misspelled argument = %s", text)
	}
	if text, isError := callVaultTool(t, cs, "control_browser_view", map[string]any{"node": "w1", "listen": "0.0.0.0:0"}); !isError || !strings.Contains(text, "loopback") {
		t.Errorf("non-loopback browser view = %s", text)
	}
	if text, isError := callVaultTool(t, cs, "control_call", map[string]any{"node": "w1", "method": "files.write", "params": map[string]any{"data": strings.Repeat("x", VaultMaxPayload)}}); !isError || !strings.Contains(text, "exceed") {
		t.Errorf("oversized request = %.200s", text)
	}
	if sent := peer.actions(); len(sent) != 0 {
		t.Fatalf("refused requests reached the broker: %v", sent)
	}
	if _, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: "control_upload", Arguments: map[string]any{}}); err == nil {
		t.Fatal("unsupported upload tool is callable")
	}
}

func TestVaultMCPScopeFailsClosed(t *testing.T) {
	v, err := NewVault(&fakeVaultPeer{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := v.MCPServer(t.Context(), VaultMCPOptions{}); err == nil {
		t.Fatal("implicit scope accepted")
	}
	for _, scope := range []VaultScope{"chat", "subagent", "session"} {
		if _, err := v.MCPServer(t.Context(), VaultMCPOptions{Scope: scope}); !errors.Is(err, ErrVaultSelectiveScope) {
			t.Errorf("scope %s error = %v", scope, err)
		}
	}
	if _, err := NewVault(nil); err == nil {
		t.Fatal("nil broker accepted")
	}
}

func TestVaultBrowserViewPinsMachineID(t *testing.T) {
	peer := &fakeVaultPeer{respond: func(op string, _ json.RawMessage) (json.RawMessage, error) {
		if op == VaultOpMachines {
			return json.RawMessage(`[{"id":"stable-1","name":"w1"}]`), nil
		}
		return json.RawMessage(`{"id":"f1","node":"stable-1","listen":"127.0.0.1:9000","activeConnections":0,"capacityRejections":0}`), nil
	}}
	v, _ := NewVault(peer)
	info, err := v.StartBrowserView(t.Context(), BrowserViewSpec{Node: "w1"})
	if err != nil || info.URL != "http://127.0.0.1:9000" {
		t.Fatalf("browser view = %+v, %v", info, err)
	}
	sent := peer.sent()
	var forward ForwardSpec
	if len(sent) != 2 || sent[0].Op != VaultOpMachines || json.Unmarshal(sent[1].Params, &forward) != nil || forward.Node != "stable-1" || forward.Address != "127.0.0.1:4848" {
		t.Fatalf("sent %v", sent)
	}
}

func TestVaultRunbooksRejectWorkerIdentityMismatch(t *testing.T) {
	peer := &fakeVaultPeer{respond: func(_ string, params json.RawMessage) (json.RawMessage, error) {
		var call VaultCall
		_ = json.Unmarshal(params, &call)
		if call.Method == "node.describe" {
			return json.RawMessage(`{"id":"stable-1","name":"w1","runbooksProtocol":{"protocol":"` + runbooks.Protocol + `"}}`), nil
		}
		return json.RawMessage(`{"node":"w1","machineId":"other","sources":[],"runbooks":[]}`), nil
	}}
	v, _ := NewVault(peer)
	catalog, err := v.Runbooks(t.Context(), runbooks.Query{Node: "w1", Source: "worker-user"})
	if err != nil {
		t.Fatal(err)
	}
	if len(catalog.Sources) != 1 || !strings.Contains(catalog.Sources[0].Error, "invalid runbook identity") {
		t.Fatalf("catalog = %+v", catalog)
	}
}

func TestVaultStartTaskReportsReconcilableID(t *testing.T) {
	peer := &fakeVaultPeer{respond: func(string, json.RawMessage) (json.RawMessage, error) {
		return nil, errors.New("broker connection lost")
	}}
	v, _ := NewVault(peer)
	id, err := v.StartTask(t.Context(), "w1", model.TaskSpec{Capability: "exec.run"}, nil)
	var submit *TaskSubmitError
	if !errors.As(err, &submit) || submit.ID != id || id == "" {
		t.Fatalf("uncertain submission = %q, %v", id, err)
	}
	if len(peer.sent()) != 1 {
		t.Fatal("uncertain submission was replayed")
	}
}

func TestVaultReconcileNeverInfersNotStarted(t *testing.T) {
	plan := model.JSON(map[string]any{"steps": []map[string]any{
		{"name": "a", "target": "w2", "task": map[string]any{"id": "child-a", "capability": "exec.run"}},
		{"name": "b", "target": "w3", "task": map[string]any{"id": "child-b", "capability": "exec.run"}},
	}})
	peer := &fakeVaultPeer{respond: func(_ string, params json.RawMessage) (json.RawMessage, error) {
		var call VaultCall
		_ = json.Unmarshal(params, &call)
		var q struct {
			ID string `json:"id"`
		}
		_ = json.Unmarshal(call.Params, &q)
		switch q.ID {
		case "wf":
			return model.JSON(model.Task{ID: "wf", State: "failed", Spec: model.TaskSpec{ID: "wf", Capability: "workflow.run", Args: plan}, Result: json.RawMessage(`{}`)}), nil
		case "child-a":
			return model.JSON(model.Task{ID: "child-a", State: "succeeded"}), nil
		}
		return nil, &model.RPCError{Code: "op_failed", Outcome: "unknown", Message: workerTaskNotFound}
	}}
	v, _ := NewVault(peer)
	report, err := v.ReconcileWorkflow(t.Context(), "w1", "wf")
	if err != nil {
		t.Fatal(err)
	}
	if report.Complete || report.Steps[0].Task == nil || report.Steps[0].Task.State != "succeeded" || report.Steps[1].NotStarted || report.Steps[1].Outcome != "unknown" {
		t.Fatalf("report = %+v", report)
	}
	for _, r := range peer.sent() {
		var call VaultCall
		if json.Unmarshal(r.Params, &call) != nil || call.Method != "tasks.get" {
			t.Fatalf("reconcile sent %s", r.Params)
		}
	}
}

func TestVaultUploadStatePinsDestination(t *testing.T) {
	nodeID := "stable-1"
	peer := &fakeVaultPeer{respond: func(op string, params json.RawMessage) (json.RawMessage, error) {
		if op == VaultOpMachines {
			return json.RawMessage(`[{"id":"stable-1","name":"w1"}]`), nil
		}
		var call VaultCall
		_ = json.Unmarshal(params, &call)
		if call.Target != "stable-1" || call.Method != "files.upload.abort" {
			return nil, errors.New("unexpected call " + string(params))
		}
		return json.RawMessage(`{"id":"up1","nodeId":"` + nodeID + `","state":"aborted"}`), nil
	}}
	v, _ := NewVault(peer)
	if result, err := v.UploadState(t.Context(), "w1", "up1", true); err != nil || result.State != "aborted" {
		t.Fatalf("abort = %+v, %v", result, err)
	}
	nodeID = "other"
	if _, err := v.UploadState(t.Context(), "w1", "up1", true); err == nil || !strings.Contains(err.Error(), "identity mismatch") {
		t.Fatalf("mismatch = %v", err)
	}
	before := len(peer.sent())
	if _, err := v.UploadState(t.Context(), "w1", "../bad", false); err == nil || len(peer.sent()) != before {
		t.Fatalf("invalid id = %v", err)
	}
}

func pendingVaultPeer() *fakeVaultPeer {
	return &fakeVaultPeer{respond: func(string, json.RawMessage) (json.RawMessage, error) {
		return nil, &model.RPCError{Code: VaultApprovalRequired, Outcome: "not_sent", Message: "approval_required: grant this connection (code WXYZ-1234)"}
	}}
}

func TestVaultMCPEveryToolNeedsLiveGrant(t *testing.T) {
	peer := pendingVaultPeer()
	cs := vaultSession(t, peer)
	calls := []struct {
		name string
		args map[string]any
	}{
		{"control_guide", map[string]any{"topic": "desktop"}},
		{"control_runbooks", map[string]any{}},
		{"control_runbooks", map[string]any{"local": true}},
		{"control_runbooks", map[string]any{"source": "project", "file": "x.md"}},
		{"control_runbooks", map[string]any{"bogus": true}},
		{"control_system", map[string]any{"node": "w1", "refrsh": true}},
		{"control_call", map[string]any{"node": "w1", "method": "shell.open"}},
		{"control_browser_view", map[string]any{"node": "w1", "listen": "0.0.0.0:0"}},
		{"control_upload_status", map[string]any{"node": "w1", "id": "../x"}},
		{"control_forward_list", map[string]any{}},
	}
	for _, call := range calls {
		text, isError := callVaultTool(t, cs, call.name, call.args)
		if !isError || !strings.Contains(text, `"code":"approval_required"`) || !strings.Contains(text, "WXYZ-1234") || strings.Contains(text, "Snapshots can succeed") {
			t.Errorf("%s %v = %s", call.name, call.args, text)
		}
	}
	sent := peer.sent()
	if len(sent) != len(calls) {
		t.Fatalf("sent %d requests for %d calls: %v", len(sent), len(calls), sent)
	}
	if actions := peer.actions(); len(actions) != 0 {
		t.Fatalf("pending connection performed actions: %v", actions)
	}
}

func TestVaultRunbooksNeedLiveGrant(t *testing.T) {
	peer := pendingVaultPeer()
	v, _ := NewVault(peer)
	for _, q := range []runbooks.Query{{}, {Local: true}, {Source: "global", File: "a.md"}, {Node: "w1"}} {
		catalog, err := v.Runbooks(t.Context(), q)
		if !VaultAuthorizationError(err) || len(catalog.Sources) != 0 || len(catalog.Runbooks) != 0 {
			t.Errorf("%+v = %+v, %v", q, catalog, err)
		}
	}
	if actions := peer.actions(); len(actions) != 0 {
		t.Fatalf("pending runbooks performed actions: %v", actions)
	}
}

func TestVaultRunbooksDoNotMaskLostGrant(t *testing.T) {
	for _, method := range []string{"node.describe", "runbooks.list"} {
		peer := &fakeVaultPeer{respond: func(op string, params json.RawMessage) (json.RawMessage, error) {
			if op == VaultOpCheck {
				return json.RawMessage(`{"granted":true}`), nil
			}
			var call VaultCall
			_ = json.Unmarshal(params, &call)
			if call.Method == method {
				return nil, &model.RPCError{Code: "grant_revoked", Outcome: "unknown", Message: "grant revoked"}
			}
			return json.RawMessage(`{"id":"stable-1","name":"w1","runbooksProtocol":{"protocol":"` + runbooks.Protocol + `"}}`), nil
		}}
		v, _ := NewVault(peer)
		catalog, err := v.Runbooks(t.Context(), runbooks.Query{Node: "w1", Source: "worker-user"})
		if !VaultAuthorizationError(err) || len(catalog.Sources) != 0 {
			t.Errorf("%s revoked = %+v, %v", method, catalog, err)
		}
	}
}

func TestVaultReconcileReportsLostGrant(t *testing.T) {
	plan := model.JSON(map[string]any{"steps": []map[string]any{{"name": "a", "target": "w2", "task": map[string]any{"id": "child-a", "capability": "exec.run"}}}})
	peer := &fakeVaultPeer{respond: func(_ string, params json.RawMessage) (json.RawMessage, error) {
		var call VaultCall
		_ = json.Unmarshal(params, &call)
		if call.Target == "w1" {
			return model.JSON(model.Task{ID: "wf", State: "running", Spec: model.TaskSpec{ID: "wf", Capability: "workflow.run", Args: plan}}), nil
		}
		return nil, &model.RPCError{Code: "grant_expired", Outcome: "unknown", Message: "grant expired"}
	}}
	v, _ := NewVault(peer)
	if _, err := v.ReconcileWorkflow(t.Context(), "w1", "wf"); !VaultAuthorizationError(err) {
		t.Fatalf("lost grant = %v", err)
	}
	if len(peer.sent()) != 2 {
		t.Fatalf("sent %v, want each query once", peer.sent())
	}
}
