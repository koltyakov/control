package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/koltyakov/control/internal/browser"
	"github.com/koltyakov/control/internal/buildinfo"
	"github.com/koltyakov/control/internal/desktop"
	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/runbooks"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// VaultScope names what one broker approval covers. It is a statement about
// the connection, never an attribution derived from tool arguments or the
// agent's environment.
type VaultScope string

// VaultScopeProcess means one approval covers every request on this MCP
// server process's broker connection, including every chat or subagent the
// host routes through that process.
const VaultScopeProcess VaultScope = "process"

// ErrVaultSelectiveScope refuses per-chat or per-subagent approval. An MCP host
// may route several chats or subagents through one stdio server process, and
// no dedicated-connection integration proves otherwise.
var ErrVaultSelectiveScope = errors.New("selective per-chat or per-subagent vault approval is unsupported: this host can share one stdio MCP server process across chats and subagents, and Control has no dedicated-connection proof to attribute calls; use process scope")

// VaultMCPOptions configures a vault MCP server.
type VaultMCPOptions struct {
	// Scope must be set explicitly. Only VaultScopeProcess is supported.
	Scope VaultScope
}

func (o VaultMCPOptions) validate() error {
	switch o.Scope {
	case VaultScopeProcess:
		return nil
	case "":
		return errors.New("vault MCP requires an explicit approval scope; only process scope is supported")
	default:
		return fmt.Errorf("vault approval scope %q: %w", o.Scope, ErrVaultSelectiveScope)
	}
}

// vaultRemoteTools forward to one node method through the broker's call
// operation, keeping the original tool's schema and argument handling. An
// empty method reads the method argument, as control_call does.
var vaultRemoteTools = map[string]string{
	"control_activities":       "activities.pool",
	"control_system":           "system.info",
	"control_describe":         "node.describe",
	"control_select":           "nodes.select",
	"control_diagnostics":      "diagnostics.get",
	"control_call":             "",
	"control_task_start":       "tasks.start",
	"control_delegate":         "peers.call",
	"control_access_list":      "access.list",
	"control_access_revoke":    "access.revoke",
	"control_task_get":         "tasks.get",
	"control_tasks":            "tasks.page",
	"control_task_cancel":      "tasks.cancel",
	"control_task_logs":        "tasks.logs",
	"control_mcp_discover":     "mcp.discover",
	"control_mcp_call":         "mcp.call",
	"control_desktop":          "desktop.run",
	"control_artifact_export":  "artifacts.export",
	"control_artifact_deliver": "artifacts.deliver",
	"control_browser":          "browser.run",
}

// vaultUnsupportedTools are not exposed in vault mode. Each needs a local
// data-plane stream, local state or a local privileged action that the broker
// channel does not provide. New MCP tools must be classified explicitly.
var vaultUnsupportedTools = map[string]string{
	"control_upload":          "streams local file bytes to the worker",
	"control_upload_resume":   "streams local file bytes to the worker",
	"control_clipboard_paste": "reads or writes this machine's clipboard",
	"control_speedtest":       "streams synthetic bulk traffic",
	"control_wake":            "inspects local network evidence and may send local packets",
}

type vaultLocalTool func(v *Vault, ctx context.Context, raw json.RawMessage, scope VaultScope) (any, error)

func decodeStrict(raw json.RawMessage, value any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	return decoder.Decode(value)
}

// vaultLocalTools handle tools whose original handler is not a single node
// method. They reach the broker only through its typed operations.
var vaultLocalTools = map[string]vaultLocalTool{
	"control_nodes": func(v *Vault, ctx context.Context, raw json.RawMessage, _ VaultScope) (any, error) {
		var input struct {
			Node    *string `json:"node"`
			Compact bool    `json:"compact"`
		}
		if err := decodeStrict(raw, &input); err != nil {
			return nil, vaultNotSent(err)
		}
		q := MachineQuery{Compact: input.Compact}
		if input.Node != nil {
			if *input.Node == "" {
				return nil, vaultNotSent(errors.New("node requires a nonempty machine name or stable ID"))
			}
			q.Node = *input.Node
		}
		return v.Machines(ctx, q)
	},
	"control_artifacts": func(v *Vault, ctx context.Context, raw json.RawMessage, _ VaultScope) (any, error) {
		var q ArtifactQuery
		if err := decodeStrict(raw, &q); err != nil {
			return nil, vaultNotSent(err)
		}
		return v.Artifacts(ctx, q)
	},
	"control_browser_view": func(v *Vault, ctx context.Context, raw json.RawMessage, _ VaultScope) (any, error) {
		var spec BrowserViewSpec
		if err := decodeStrict(raw, &spec); err != nil {
			return nil, vaultNotSent(err)
		}
		return v.StartBrowserView(ctx, spec)
	},
	"control_runbooks": func(v *Vault, ctx context.Context, raw json.RawMessage, _ VaultScope) (any, error) {
		var q runbooks.Query
		if err := decodeStrict(raw, &q); err != nil {
			return nil, vaultNotSent(err)
		}
		return v.Runbooks(ctx, q)
	},
	"control_guide": func(v *Vault, ctx context.Context, raw json.RawMessage, _ VaultScope) (any, error) {
		var q struct {
			Topic string `json:"topic"`
		}
		if err := decodeStrict(raw, &q); err != nil {
			return nil, vaultNotSent(err)
		}
		if q.Topic != "desktop" {
			return nil, vaultNotSent(fmt.Errorf("unknown guide topic %q; available: desktop", q.Topic))
		}
		return map[string]string{"topic": q.Topic, "guidance": desktopGuide}, nil
	},
	"control_session": func(v *Vault, ctx context.Context, _ json.RawMessage, scope VaultScope) (any, error) {
		return v.Session(ctx, scope)
	},
	"control_forward_start": func(v *Vault, ctx context.Context, raw json.RawMessage, _ VaultScope) (any, error) {
		var spec ForwardSpec
		if err := decodeStrict(raw, &spec); err != nil {
			return nil, vaultNotSent(err)
		}
		return v.StartForward(ctx, spec)
	},
	"control_forward_list": func(v *Vault, ctx context.Context, _ json.RawMessage, _ VaultScope) (any, error) {
		return v.Forwards(ctx)
	},
	"control_forward_stop": func(v *Vault, ctx context.Context, raw json.RawMessage, _ VaultScope) (any, error) {
		var q struct {
			ID string `json:"id"`
		}
		if err := decodeStrict(raw, &q); err != nil {
			return nil, vaultNotSent(err)
		}
		if err := v.StopForward(ctx, q.ID); err != nil {
			return nil, err
		}
		return map[string]bool{"stopped": true}, nil
	},
	"control_tunnel_start": func(v *Vault, ctx context.Context, raw json.RawMessage, _ VaultScope) (any, error) {
		var spec PersistentTunnelSpec
		if err := decodeStrict(raw, &spec); err != nil {
			return nil, vaultNotSent(err)
		}
		return v.StartPersistentTunnel(ctx, spec)
	},
	"control_upload_status": func(v *Vault, ctx context.Context, raw json.RawMessage, _ VaultScope) (any, error) {
		return vaultUploadState(v, ctx, raw, false)
	},
	"control_upload_abort": func(v *Vault, ctx context.Context, raw json.RawMessage, _ VaultScope) (any, error) {
		return vaultUploadState(v, ctx, raw, true)
	},
	"control_workflow_reconcile": func(v *Vault, ctx context.Context, raw json.RawMessage, _ VaultScope) (any, error) {
		var q struct {
			Node string `json:"node"`
			ID   string `json:"id"`
		}
		if err := decodeStrict(raw, &q); err != nil {
			return nil, vaultNotSent(err)
		}
		return v.ReconcileWorkflow(ctx, q.Node, q.ID)
	},
	"control_tunnel_list": func(v *Vault, ctx context.Context, _ json.RawMessage, _ VaultScope) (any, error) {
		return v.PersistentTunnels(ctx)
	},
	"control_tunnel_dispose": func(v *Vault, ctx context.Context, raw json.RawMessage, _ VaultScope) (any, error) {
		var q struct {
			ID string `json:"id"`
		}
		if err := decodeStrict(raw, &q); err != nil {
			return nil, vaultNotSent(err)
		}
		if err := v.DisposePersistentTunnel(ctx, q.ID); err != nil {
			return nil, err
		}
		return map[string]bool{"disposed": true}, nil
	},
}

func vaultUploadState(v *Vault, ctx context.Context, raw json.RawMessage, abort bool) (any, error) {
	var q struct {
		Node string `json:"node"`
		ID   string `json:"id"`
	}
	if err := decodeStrict(raw, &q); err != nil {
		return nil, vaultNotSent(err)
	}
	return v.UploadState(ctx, q.Node, q.ID, abort)
}

// vaultCatalog lists the regular MCP server's tools and instructions over an
// in-memory session. Its handlers are never called; only the static
// definitions are reused, so vault tools keep the original schemas.
func vaultCatalog(ctx context.Context) ([]*mcp.Tool, string, error) {
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	server, err := (Client{}).MCPServer().Connect(ctx, serverTransport, nil)
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = server.Close() }()
	session, err := mcp.NewClient(&mcp.Implementation{Name: "control-vault-catalog", Version: buildinfo.Version}, nil).Connect(ctx, clientTransport, nil)
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = session.Close() }()
	var tools []*mcp.Tool
	for tool, err := range session.Tools(ctx, nil) {
		if err != nil {
			return nil, "", err
		}
		tools = append(tools, tool)
	}
	instructions := ""
	if result := session.InitializeResult(); result != nil {
		instructions = result.Instructions
	}
	return tools, instructions, nil
}

// vaultInstructions append the vault scope and limits to the original server
// instructions, without claiming per-chat or per-subagent attribution.
func vaultInstructions(original string, scope VaultScope) string {
	unsupported := make([]string, 0, len(vaultUnsupportedTools))
	for name := range vaultUnsupportedTools {
		unsupported = append(unsupported, name)
	}
	sort.Strings(unsupported)
	return original + " Vault mode: this MCP server holds no Control credentials; a vault broker performs every account-client request over this process's connection. Approval scope: " + string(scope) +
		". One approval covers every request from this MCP server process, including any chat or subagent the host routes through it; Control cannot attribute calls to an individual chat or subagent. Only initialize and listing tools need no approval. Until the user approves the connection, every tool call, including locally answered tools and invalid input, returns approval_required with an approval code and does nothing else; ask the user to approve it, and call again only after they confirm. Nothing is replayed automatically. control_describe runbook catalogs contain worker stores only, because the broker never reads its host's project or global stores; control_runbooks adds project and global stores from this MCP server's own host and working directory. control_upload_status and control_upload_abort query single-file staging on the worker as the broker's account owner; directory transfers are not tracked in vault mode. control_workflow_reconcile never infers notStarted through the broker. Forwards and retained tunnels are created and held by the vault broker, not this MCP server; tunnel traffic itself does not pass through approval. Unavailable in vault mode, use the regular control CLI instead: " + strings.Join(unsupported, ", ") + "."
}

// vaultToolError reports broker and validation failures with the same JSON
// shape as toolError, adding approval guidance for a pending connection.
func vaultToolError(err error, scope VaultScope) *mcp.CallToolResult {
	var rpc *model.RPCError
	if !errors.As(err, &rpc) || rpc.Code != VaultApprovalRequired {
		return toolError(err)
	}
	guidance := "Ask the user to review and approve this pending vault connection using the approval code in message. Approval covers this MCP server process, not a single chat or subagent. Call the tool again only after the user confirms approval; nothing is replayed automatically."
	if rpc.Outcome == "not_sent" {
		guidance = "Nothing was sent to a worker. " + guidance
	}
	payload := map[string]string{"code": rpc.Code, "outcome": rpc.Outcome, "message": rpc.Message, "approvalScope": string(scope), "guidance": guidance}
	return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: toolText(model.JSON(payload))}}}
}

func vaultToolResult(result json.RawMessage) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: toolText(result)}}}
}

// MCPServer builds a vault MCP server. Tool listing is static and contacts no
// broker; every tool call goes through the broker's typed operations.
func (v *Vault) MCPServer(ctx context.Context, opts VaultMCPOptions) (*mcp.Server, error) {
	if err := opts.validate(); err != nil {
		return nil, err
	}
	tools, instructions, err := vaultCatalog(ctx)
	if err != nil {
		return nil, fmt.Errorf("load MCP tool catalog: %w", err)
	}
	server := mcp.NewServer(&mcp.Implementation{Name: "control", Version: buildinfo.Version}, &mcp.ServerOptions{Instructions: vaultInstructions(instructions, opts.Scope)})
	for _, tool := range tools {
		if method, ok := vaultRemoteTools[tool.Name]; ok {
			properties, required, err := schemaFields(tool)
			if err != nil {
				return nil, err
			}
			server.AddTool(tool, v.gated(v.remoteTool(tool.Name, method, properties, required, opts.Scope), opts.Scope))
			continue
		}
		if call, ok := vaultLocalTools[tool.Name]; ok {
			properties, _, err := schemaFields(tool)
			if err != nil {
				return nil, err
			}
			server.AddTool(tool, v.gated(v.localTool(call, properties, opts.Scope), opts.Scope))
			continue
		}
		if _, ok := vaultUnsupportedTools[tool.Name]; ok {
			continue
		}
		// Fail closed: an unclassified tool is never exposed.
		return nil, fmt.Errorf("MCP tool %s has no vault classification", tool.Name)
	}
	return server, nil
}

type vaultCheckedKey struct{}

// gated answers a tool call only under a live grant. Before approval, every
// call, including invalid input and tools answered locally, returns the
// broker's authorization error after one no-I/O check, with nothing else
// done. Only initialize and tools/list are public. The check is never retried.
func (v *Vault) gated(handler mcp.ToolHandler, scope VaultScope) mcp.ToolHandler {
	return func(ctx context.Context, request *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if err := v.Check(ctx); err != nil {
			return vaultToolError(err, scope), nil
		}
		return handler(context.WithValue(ctx, vaultCheckedKey{}, true), request)
	}
}

func schemaFields(tool *mcp.Tool) (map[string]any, []string, error) {
	var schema struct {
		Properties map[string]any `json:"properties"`
		Required   []string       `json:"required"`
	}
	b, err := json.Marshal(tool.InputSchema)
	if err == nil {
		err = json.Unmarshal(b, &schema)
	}
	if err != nil {
		return nil, nil, fmt.Errorf("MCP tool %s schema: %w", tool.Name, err)
	}
	if schema.Properties == nil {
		schema.Properties = map[string]any{}
	}
	return schema.Properties, schema.Required, nil
}

// vaultArguments bounds and decodes tool arguments before anything is sent.
func vaultArguments(request *mcp.CallToolRequest, properties map[string]any) (json.RawMessage, map[string]json.RawMessage, error) {
	raw := toolArguments(request)
	if len(raw) > VaultMaxPayload {
		return nil, nil, fmt.Errorf("tool arguments exceed %d bytes", VaultMaxPayload)
	}
	args := map[string]json.RawMessage{}
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, nil, err
	}
	if err := knownFields(args, properties); err != nil {
		return nil, nil, err
	}
	return raw, args, nil
}

func (v *Vault) localTool(call vaultLocalTool, properties map[string]any, scope VaultScope) mcp.ToolHandler {
	return func(ctx context.Context, request *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		raw, _, err := vaultArguments(request, properties)
		if err != nil {
			return inputError(err), nil
		}
		result, err := call(v, ctx, raw, scope)
		if err != nil {
			return vaultToolError(err, scope), nil
		}
		if data, ok := result.(json.RawMessage); ok {
			return vaultToolResult(data), nil
		}
		return vaultToolResult(model.JSON(result)), nil
	}
}

// vaultTarget splits node from the remaining arguments, as the original
// normal and nested transforms do.
func vaultTarget(name string, args map[string]json.RawMessage) (string, any, error) {
	var target string
	if b, ok := args["node"]; ok {
		if err := json.Unmarshal(b, &target); err != nil {
			return "", nil, err
		}
	}
	delete(args, "node")
	nested := func(field string) json.RawMessage {
		if params := args[field]; len(params) != 0 {
			return params
		}
		return json.RawMessage(`{}`)
	}
	switch name {
	case "control_call":
		return target, nested("params"), nil
	case "control_task_start":
		return target, nested("task"), nil
	case "control_desktop":
		if _, err := desktop.Decode(args["request"]); err != nil {
			return "", nil, err
		}
		return target, nested("request"), nil
	case "control_browser":
		if _, err := browser.Decode(args["request"]); err != nil {
			return "", nil, err
		}
		return target, nested("request"), nil
	}
	return target, args, nil
}

func hasWorkflowPlacement(spec model.TaskSpec) bool {
	if spec.Capability != "workflow.run" {
		return false
	}
	var plan model.WorkflowPlan
	return json.Unmarshal(spec.Args, &plan) == nil && plan.HasPlacement()
}

func (v *Vault) remoteTool(name, method string, properties map[string]any, required []string, scope VaultScope) mcp.ToolHandler {
	return func(ctx context.Context, request *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		_, args, err := vaultArguments(request, properties)
		if err != nil {
			return inputError(err), nil
		}
		omitSpec := taskResultTools[name]
		if raw, ok := args["includeSpec"]; ok && name == "control_task_get" {
			var include bool
			if err := json.Unmarshal(raw, &include); err != nil {
				return inputError(err), nil
			}
			omitSpec = !include
			delete(args, "includeSpec")
		}
		for _, key := range required {
			if _, ok := args[key]; !ok {
				return inputError(fmt.Errorf("missing required field %s", key)), nil
			}
		}
		actualMethod := method
		if method == "" {
			if err := json.Unmarshal(args["method"], &actualMethod); err != nil {
				return inputError(err), nil
			}
		}
		target, params, err := vaultTarget(name, args)
		if err != nil {
			return inputError(err), nil
		}
		var result json.RawMessage
		if actualMethod == "tasks.start" && method != "" {
			spec, err := decodeTaskSpec(params)
			if err != nil {
				return inputError(err), nil
			}
			// The broker resolves select/each targets; the caller does not
			// hold that plan, so keep the returned specification.
			omitSpec = omitSpec && !hasWorkflowPlacement(spec)
			if _, err = v.StartTask(ctx, target, spec, &result); err != nil {
				return vaultToolError(err, scope), nil
			}
		} else if err = v.Call(ctx, target, actualMethod, params, &result); err != nil {
			return vaultToolError(err, scope), nil
		}
		if omitSpec {
			result = WithoutTaskSpec(result)
		}
		return vaultToolResult(result), nil
	}
}
