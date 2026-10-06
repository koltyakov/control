package client

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/koltyakov/control/internal/buildinfo"
	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/rpa"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func (c Client) MCPServer() *mcp.Server {
	server := mcp.NewServer(&mcp.Implementation{Name: "control", Version: buildinfo.Version}, &mcp.ServerOptions{Instructions: "An account-authenticated orchestrator client initiates execution; a worker is an enrolled node executing requested work. Fleet membership and worker/common credentials permit discovery, not independent peer commands. The gateway handles enrollment, discovery, signaling, relay, and fleet administration, not execution. Agent refers to AI software. Default account routing uses this client's stable owner even when a local node is running. Use control_nodes to resolve machine names and control_describe for capability schemas. Do not schedule disabled or controlPending machines. Use control_delegate to execute a specific instruction on one worker through another; the client prepares instruction-bound destination grants. Workflow tasks and artifact delivery prepare grants too. Grants are not ambient worker permissions or inherited by arbitrary worker scripts. Inspect grants with control_access_list and revoke them with control_access_revoke at each destination. Revocation cancels associated tasks and streams but cannot undo effects. Grants expire after one hour idle and are lost on destination restart; accepted tasks keep them active. Synchronous delegated instructions are single-use. Use control_task_start with explicit IDs for long work and reconciliation; accepted tasks survive this process exiting. Concurrent account clients sharing the saved owner can query or cancel them. Use control_forward_start/list/stop for process-owned forwards, with reverse: true for an orchestrator-local dev server. Closing a forward does not stop its destination service. control_session inspects this client without enrolling a machine. Artifacts move directly between producers and consumers. Discover MCP tools before invoking them. Use named execution targets; nodes.list, nodes.select, and activities.pool accept an empty target. Explicit local API routing permits local work and peer discovery, not independent peer execution. Never replay uncertain side effects."})
	add := func(name, description, method string, properties map[string]any, required []string, transform func(map[string]json.RawMessage) (string, any, error)) {
		input := map[string]any{"type": "object", "properties": properties}
		if len(required) > 0 {
			input["required"] = required
		}
		server.AddTool(&mcp.Tool{Name: name, Description: description, InputSchema: input}, func(ctx context.Context, request *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			args := map[string]json.RawMessage{}
			if err := json.Unmarshal(request.Params.Arguments, &args); err != nil {
				return toolError(err), nil
			}
			for _, key := range required {
				if _, ok := args[key]; !ok {
					return toolError(fmt.Errorf("missing required field %s", key)), nil
				}
			}
			target, params, err := transform(args)
			if err != nil {
				return toolError(err), nil
			}
			actualMethod := method
			if method == "" {
				if err = json.Unmarshal(args["method"], &actualMethod); err != nil {
					return toolError(err), nil
				}
			}
			var result json.RawMessage
			if err = c.Call(ctx, target, actualMethod, params, &result); err != nil {
				return toolError(err), nil
			}
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(result)}}}, nil
		})
	}
	text := map[string]any{"type": "string"}
	object := map[string]any{"type": "object"}
	normal := func(args map[string]json.RawMessage) (string, any, error) {
		var target string
		if b, ok := args["node"]; ok {
			if err := json.Unmarshal(b, &target); err != nil {
				return "", nil, err
			}
		}
		delete(args, "node")
		return target, args, nil
	}
	nested := func(field string) func(map[string]json.RawMessage) (string, any, error) {
		return func(args map[string]json.RawMessage) (string, any, error) {
			target, _, err := normal(args)
			params := args[field]
			if len(params) == 0 {
				params = model.JSON(map[string]any{})
			}
			return target, params, err
		}
	}
	add("control_nodes", "List enrolled machines, software versions, online/disabled status, pending policy, labels, and capabilities.", "nodes.list", map[string]any{}, nil, normal)
	add("control_activities", "Read a pool-wide activity snapshot, including all registered machines, availability, tasks from all owners, transfers, tunnels, and cached system metrics. Unavailable nodes are reported individually. Requires activities.list permission on the observed nodes.", "activities.pool", map[string]any{"nodes": map[string]any{"type": "array", "items": text}, "recent": map[string]any{"type": "integer", "minimum": 0, "maximum": 64}}, nil, normal)
	add("control_system", "Read OS, CPU, RAM, and disk usage from a node's cached system sample. Set refresh to request a new sample.", "system.info", map[string]any{"node": text, "refresh": map[string]any{"type": "boolean"}}, []string{"node"}, normal)
	add("control_describe", "Inspect a node and discover capability input schemas.", "node.describe", map[string]any{"node": text}, []string{"node"}, normal)
	add("control_select", "Find an online, enabled machine matching labels and a capability. Machines awaiting a policy acknowledgement are excluded.", "nodes.select", map[string]any{"labels": object, "capability": text}, nil, normal)
	add("control_call", "Call a node method or capability with structured params. Discover capabilities first. Also supports artifacts.list, artifacts.pull, artifacts.grant, tasks.list, and mcp.request.", "", map[string]any{"node": text, "method": text, "params": object}, []string{"method"}, nested("params"))
	add("control_task_start", "Submit a tracked task. task has capability, args, optional id, timeoutSeconds, inputs [{artifact,path}], and outputs [relative paths]. Reuse the task ID to reconcile an uncertain submission.", "tasks.start", map[string]any{"node": text, "task": object}, []string{"node", "task"}, nested("task"))
	add("control_delegate", "Execute one orchestrator-authorized JSON instruction on target through worker node. Prepares a destination-owned, instruction-bound grant. Worker credentials alone cannot execute peer commands. Use tasks.start with an explicit ID for durable child work; synchronous instructions are single-use.", "peers.call", map[string]any{"node": text, "target": text, "method": text, "params": object}, []string{"node", "target", "method", "params"}, normal)
	add("control_access_list", "List this orchestrator owner's instruction-bound grants at node, including idle expiry. Does not list other owners' grants.", "access.list", map[string]any{"node": text}, []string{"node"}, normal)
	add("control_access_revoke", "Revoke this owner's grant at node and cancel associated tasks and streams. Cannot undo completed effects. Idempotent when the grant is absent.", "access.revoke", map[string]any{"node": text, "id": text}, []string{"node", "id"}, normal)
	add("control_task_get", "Read task status, result, and artifact references.", "tasks.get", map[string]any{"node": text, "id": text}, []string{"node", "id"}, normal)
	add("control_task_cancel", "Cancel a queued or running task.", "tasks.cancel", map[string]any{"node": text, "id": text}, []string{"node", "id"}, normal)
	add("control_task_logs", "Read task logs from a byte offset. Returns the next offset.", "tasks.logs", map[string]any{"node": text, "id": text, "offset": map[string]any{"type": "integer"}}, []string{"node", "id"}, normal)
	add("control_mcp_discover", "List configured MCP servers, or discover a server's tools and input schemas.", "mcp.discover", map[string]any{"node": text, "server": text}, []string{"node"}, normal)
	add("control_mcp_call", "Invoke an installed MCP tool on the selected node.", "mcp.call", map[string]any{"node": text, "server": text, "tool": text, "arguments": object}, []string{"node", "server", "tool"}, normal)
	add("control_rpa", "Run an opt-in GUI action batch on a worker's logged-in desktop. Discover rpa.run first. Use secret instead of text on setValue/type for worker-local credentials; never request their values. Inspect accessibility elements before selector actions. Screenshots return PNG artifacts and are not secret-masked. Batches are serialized per OS user; use leased tasks for multi-batch workflows. A failed call may have changed the desktop; never replay automatically.", "rpa.run", map[string]any{"node": text, "actions": rpa.Schema()["properties"].(map[string]any)["actions"]}, []string{"node", "actions"}, func(args map[string]json.RawMessage) (string, any, error) {
		target, params, err := normal(args)
		if err != nil {
			return "", nil, err
		}
		return target, params, rpa.Validate(model.JSON(params))
	})
	add("control_artifact_export", "Publish a file from a node's filesystem root as an immutable downloadable artifact.", "artifacts.export", map[string]any{"node": text, "path": text}, []string{"node", "path"}, normal)
	add("control_artifact_deliver", "Tell the source node to deliver an artifact directly to another node, without passing bytes through the orchestrator.", "artifacts.deliver", map[string]any{"node": text, "id": text, "target": text}, []string{"node", "id", "target"}, normal)
	local := func(name, description string, properties map[string]any, required []string, call func(context.Context, json.RawMessage) (any, error)) {
		input := map[string]any{"type": "object", "properties": properties, "additionalProperties": false}
		if len(required) > 0 {
			input["required"] = required
		}
		server.AddTool(&mcp.Tool{Name: name, Description: description, InputSchema: input}, func(ctx context.Context, request *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			result, err := call(ctx, request.Params.Arguments)
			if err != nil {
				return toolError(err), nil
			}
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(model.JSON(result))}}}, nil
		})
	}
	local("control_session", "Inspect this orchestrator process's role, stable task owner, live transport identity, connections, and port forwards. Does not enroll a fleet machine.", map[string]any{}, nil, func(ctx context.Context, _ json.RawMessage) (any, error) {
		return c.Session(ctx)
	})
	local("control_clipboard_paste", "Paste this machine's clipboard onto a remote node. Text goes to its desktop clipboard; copied regular files stream into an existing dir relative to the worker's workDir, default '.'. Set reverse to paste the remote clipboard onto this machine, with dir then a local directory. Transfer happens only on this explicit call. Does not delete source files, overwrite files, or sync automatically. Requires clipboard.paste permission, or clipboard.open for reverse. Both desktops need clipboard access for text. Returns metadata only, never clipboard text. Do not read or transfer a user's clipboard without their request, and do not retry an uncertain paste.", map[string]any{"node": text, "dir": text, "reverse": map[string]any{"type": "boolean"}}, []string{"node"}, func(ctx context.Context, args json.RawMessage) (any, error) {
		var spec ClipboardSpec
		if err := json.Unmarshal(args, &spec); err != nil {
			return nil, err
		}
		return c.PasteClipboard(ctx, spec)
	})
	local("control_forward_start", "Forward a local TCP port to HOST:PORT through a named fleet machine. With reverse: true, bind listen on that machine and forward to address on this orchestrator, for example a local dev server. Defaults to loopback on an ephemeral port. Returns immediately and survives tool-call completion until stopped or this process exits. Requires remote tcp.open permission, or tcp.listen for reverse forwarding. Existing sockets are never replayed.", map[string]any{"node": text, "address": text, "listen": text, "reverse": map[string]any{"type": "boolean"}}, []string{"node", "address"}, func(ctx context.Context, args json.RawMessage) (any, error) {
		var spec ForwardSpec
		if err := json.Unmarshal(args, &spec); err != nil {
			return nil, err
		}
		f, err := c.StartForward(ctx, spec)
		if err != nil {
			return nil, err
		}
		return f.Info(), nil
	})
	local("control_forward_list", "List local and reverse port forwards owned by this MCP process, including active connection counts and last connection errors.", map[string]any{}, nil, func(context.Context, json.RawMessage) (any, error) {
		return c.Forwards(), nil
	})
	local("control_forward_stop", "Close this process's port forward by ID, including its listener and active sockets. Does not stop the remote service or durable tasks.", map[string]any{"id": text}, []string{"id"}, func(_ context.Context, args json.RawMessage) (any, error) {
		var q struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(args, &q); err != nil {
			return nil, err
		}
		if err := c.StopForward(q.ID); err != nil {
			return nil, err
		}
		return map[string]bool{"stopped": true}, nil
	})
	return server
}

func toolError(err error) *mcp.CallToolResult {
	return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: err.Error()}}}
}
