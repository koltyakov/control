package client

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/koltyakov/control/internal/buildinfo"
	"github.com/koltyakov/control/internal/desktop"
	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/rpa"
	"github.com/koltyakov/control/internal/runbooks"
	"github.com/koltyakov/control/internal/upload"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const uploadInstructions = "For local-to-worker files, use control_upload with an explicit ID and localPath on this MCP server host, not repeated base64 files.write calls or the clipboard. It uses one bulk stream, verified resume, and atomic no-overwrite publication. After an interrupted or uncertain upload, inspect control_upload_status before explicitly using control_upload_resume with that ID. Never choose a new ID to retry uncertain publication. Use artifacts for worker-to-worker delivery. "

const concurrencyInstructions = "A busy worker is not unavailable. Run independent work on the same worker in parallel when resources permit; do not wait for unrelated tasks or cancel them to free the machine. Inspect node.describe concurrency for execution and operations pool limits and inUse counts; these are advisory, not reservations. Commands, AI CLIs, custom providers and MCP tools share maxTasks execution slots. Built-in file, HTTP, Wake-on-LAN and desktop operations share separate maxOperations slots, including when submitted as tasks. Transfers, tunnels and observation have their own admission. Use explicit task IDs and detached submission for long work, then monitor only when its result is needed. Use workflow maxParallel for independent ready steps. Do not acquire a machine-wide lease for ordinary independent work; use one only when the workflow requires exclusive execution. Respect existing leases and serialize conflicting file, application, desktop or shared-session changes. Separate task workspaces do not isolate external state. "

const desktopInstructions = runbookInstructions + uploadInstructions + concurrencyInstructions + `
Snapshots can succeed with data.complete false in agent-desktop v1.2.0. Preserve and inspect complete, truncated, nodes_observed, and subtree_truncated before concluding an element is absent. A partial observation never authorizes replaying an action or using stale refs.
When a GUI task needs missing native desktop support, suggest installation and ask for explicit user permission. Control does not bundle or automatically install FFI dependencies. After approval, select the latest compatible release from https://github.com/vercel-labs/agent-desktop/releases. Download its agent-desktop-ffi-* archive for the target User worker's OS/architecture and verify SHA-256 against checksums.txt from the same release. Require ABI major 6 or 7 and a 96-byte AdAction; reject incompatible libraries rather than bypassing the helper's ABI/layout checks. Extract the full package with companions outside workDir into private storage, and merge an absolute desktop.library path into that User profile without replacing other settings. Restart while idle, rediscover desktop.run, and check native status before credential use. OS permission grants need separate approval. See https://github.com/koltyakov/control/blob/main/docs/desktop.md. Do not install the upstream CLI merely to use the native adapter.
For remote UI work, first use control_describe on the requested User context, then control_call with method exec.run and params {"command":"agent-desktop","args":["version"]} to check for https://github.com/vercel-labs/agent-desktop on that worker. A local installation does not establish remote availability. If present, load its version-matched skills on that same worker with exec.run arguments ["skills","get","desktop"] and ["skills","get","platform"], then check ["status"] for desktop access and permissions. Read the returned data.content before UI actions. Route every agent-desktop command through Control to that same worker using structured arguments, not the orchestrator's desktop. Missing executables, denied execution, missing permissions, or unavailable desktop sessions are blockers, not permission to install software, request OS grants, or switch to System. Ask before installing agent-desktop or its harness skill; the optional skill installer is npx skills add https://github.com/vercel-labs/agent-desktop --skill agent-desktop. If agent-desktop is absent, use an already configured rpa.run when suitable, otherwise ask for setup. Use fresh snapshot-qualified refs, observe after actions, and never replay uncertain effects. Headed physical input requires explicit user permission; do not add --headed as a fallback.
When desktop.run is advertised, prefer control_desktop for native status, app-scoped snapshot, click, clear, and headless setValue, without Python. Use refs from that adapter's snapshots, not CLI refs: its private state namespace is separate. For login, ask only for worker-local secret names, including the username when private. The user enters values themselves outside chat with control secrets set NAME on the worker, control secrets set NAME --node WORKER in their orchestrator terminal, or the web console's machine Secrets action. Never run secret entry through an AI tool or control exec. Use control_desktop with request {command: "setValue", ref: "@SNAPSHOT:e1", secret: "app.password"} on the same worker/profile. The worker resolves credentials into an anonymous helper stdin pipe, never argv, environment, or persisted task arguments. Use the adapter for subsequent observations too; all responses mask configured secrets, status omits private state paths, and raw helper logs are suppressed. It exposes no screenshots, clipboard, tracing, or physical input. The existing control_rpa secret-reference path remains an alternative; inspect its selectors first because agent-desktop refs are not RPA selectors. Ordinary agent-desktop CLI text commands do not resolve Control secret names, and exec.run output is not secret-masked. Never retrieve secrets through a shell or pass values in tool inputs. If protected input is unavailable, stop for manual login or approved setup, never substitute a shell credential workaround. Do not use raw CLI observations until credentials are no longer visible. Masking is best-effort text protection, not a sandbox. Verify login by non-sensitive application state, not by reading filled values.
`

type uploadProgressKey struct{}

func (c Client) MCPServer() *mcp.Server {
	server := mcp.NewServer(&mcp.Implementation{Name: "control", Version: buildinfo.Version}, &mcp.ServerOptions{Instructions: "An account-authenticated orchestrator client initiates execution; a worker is an enrolled node executing requested work. Fleet membership and worker/common credentials permit discovery, not independent peer commands. The gateway handles enrollment, discovery, signaling, relay, and fleet administration, not execution. Agent refers to AI software. Default account routing uses this client's stable owner even when a local node is running. Use control_nodes to resolve machine names and control_describe for capability schemas. Do not schedule disabled or controlPending machines. Use control_delegate to execute a specific instruction on one worker through another; the client prepares instruction-bound destination grants. Workflow tasks and artifact delivery prepare grants too. Grants are not ambient worker permissions or inherited by arbitrary worker scripts. Inspect grants with control_access_list and revoke them with control_access_revoke at each destination. Revocation cancels associated tasks and streams but cannot undo effects. Grants expire after one hour idle and are lost on destination restart; accepted tasks keep them active. Synchronous delegated instructions are single-use. Use control_task_start with explicit IDs for long work and reconciliation; accepted tasks survive this process exiting. Concurrent account clients sharing the saved owner can query or cancel them. Use control_forward_start/list/stop for process-owned forwards, with reverse: true for an orchestrator-local dev server. Use control_tunnel_start/list/dispose for retained host-owned tunnels that survive this process exiting and restore listeners after restart. Persistent tunnels need an explicit ID, a fixed listen port, and optional ttlSeconds; omitted TTL means until disposed. Starting accepts desired state, so list to check active/retrying state and reconcile uncertain requests. Never replay interrupted TCP traffic or create replacement IDs automatically. Closing a forward does not stop its destination service. control_session inspects this client without enrolling a machine. Artifacts move directly between producers and consumers. Discover MCP tools before invoking them. Use named execution targets; nodes.list, nodes.select, and activities.pool accept an empty target. Explicit local API routing permits local work and peer discovery, not independent peer execution. Never replay uncertain side effects." + desktopInstructions})
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
	add("control_nodes", "List enrolled contexts, software versions, status, labels, and capabilities. Hybrid contexts share gateway-owned hybridId and identify user/system with hybridRole. Default to User; explicitly choose the paired System context only for authorized operations requiring elevation, never as a blind retry of failed or uncertain User work.", "nodes.list", map[string]any{}, nil, normal)
	add("control_activities", "Read a pool-wide activity snapshot, including all registered machines, availability, tasks from all owners, transfers, tunnels, direct/TURN/gateway-relay connection counts, and cached system metrics. Unavailable nodes are reported individually. Requires activities.list permission on the observed nodes.", "activities.pool", map[string]any{"nodes": map[string]any{"type": "array", "items": text}, "recent": map[string]any{"type": "integer", "minimum": 0, "maximum": 64}}, nil, normal)
	add("control_system", "Read OS, CPU, RAM, disk, and optional GPU/NPU inventory from a node's cached system sample. GPU kind identifies discrete, integrated, virtual, or npu hardware; absent kind is unknown. Utilization and dedicated-memory used/total depend on driver support; absent readings are unknown, not zero. Set refresh to request a new sample.", "system.info", map[string]any{"node": text, "refresh": map[string]any{"type": "boolean"}}, []string{"node"}, normal)
	add("control_describe", "Inspect capability schemas, concurrency pool limits and inUse counts, and runbooks from project/global and authorized worker stores. Busy does not mean unavailable for independent work. Read matching shared or machine-specific runbooks before ad-hoc probing or asking which application or secret names to use. Source errors do not mean no runbooks exist. For remote UI work also check agent-desktop version through exec.run on this worker, then load its skills get desktop and skills get platform before use. Presence is not desktop permission.", "node.describe", map[string]any{"node": text}, []string{"node"}, normal)
	add("control_select", "Select an online enabled machine by capability, labels and fresh resources. Excludes known leases and update reservations. Resource constraints require fresh health; selection does not reserve capacity.", "nodes.select", map[string]any{"labels": object, "capability": text, "minMemoryBytes": map[string]any{"type": "integer", "minimum": 0}, "minDiskBytes": map[string]any{"type": "integer", "minimum": 0}, "maxCpuPercent": map[string]any{"type": "number", "minimum": 0, "maximum": 100}, "requireFresh": map[string]any{"type": "boolean"}}, nil, normal)
	add("control_diagnostics", "Read bounded task cleanup, artifact budget, MCP startup and transport diagnostics. Uses cached system samples and excludes task arguments and credentials.", "diagnostics.get", map[string]any{"node": text}, []string{"node"}, normal)
	add("control_call", "Call a node method or capability with structured params. Discover capabilities first. Also supports artifacts.list, artifacts.pull, artifacts.grant, tasks.list, and mcp.request. exec.run can invoke agent-desktop on the named worker; load its desktop and platform skills first. exec.run output is not secret-masked. Prefer control_desktop when desktop.run is enabled; use secret references for credentials, never literal values or shell secret retrieval.", "", map[string]any{"node": text, "method": text, "params": object}, []string{"method"}, nested("params"))
	add("control_task_start", "Submit a tracked task and return after durable acceptance without waiting for completion. Independent tasks can run on the same worker in parallel; busy is not an exclusion. task has capability, args, optional id, timeoutSeconds, inputs [{artifact,path}], and outputs [relative paths]. Reuse the task ID to reconcile an uncertain submission.", "tasks.start", map[string]any{"node": text, "task": object}, []string{"node", "task"}, nested("task"))
	add("control_delegate", "Execute one orchestrator-authorized JSON instruction on target through worker node. Prepares a destination-owned, instruction-bound grant. Worker credentials alone cannot execute peer commands. Use tasks.start with an explicit ID for durable child work; synchronous instructions are single-use.", "peers.call", map[string]any{"node": text, "target": text, "method": text, "params": object}, []string{"node", "target", "method", "params"}, normal)
	add("control_access_list", "List this orchestrator owner's instruction-bound grants at node, including idle expiry. Does not list other owners' grants.", "access.list", map[string]any{"node": text}, []string{"node"}, normal)
	add("control_access_revoke", "Revoke this owner's grant at node and cancel associated tasks and streams. Cannot undo completed effects. Idempotent when the grant is absent.", "access.revoke", map[string]any{"node": text, "id": text}, []string{"node", "id"}, normal)
	add("control_task_get", "Read task status, result, and artifact references. Set waitSeconds (up to 60) to wait for completion instead of polling; a non-terminal result means the wait elapsed. Older nodes answer immediately.", "tasks.get", map[string]any{"node": text, "id": text, "waitSeconds": map[string]any{"type": "integer", "minimum": 0, "maximum": 60}}, []string{"node", "id"}, normal)
	add("control_tasks", "List bounded task summaries. Continue with nextCursor. Use tasks.get for arguments, results and artifact references.", "tasks.page", map[string]any{"node": text, "cursor": text, "limit": map[string]any{"type": "integer", "minimum": 1, "maximum": 200}}, []string{"node"}, normal)
	add("control_task_cancel", "Cancel a queued or running task.", "tasks.cancel", map[string]any{"node": text, "id": text}, []string{"node", "id"}, normal)
	add("control_task_logs", "Read task logs from a byte offset. Returns the next offset.", "tasks.logs", map[string]any{"node": text, "id": text, "offset": map[string]any{"type": "integer"}}, []string{"node", "id"}, normal)
	add("control_mcp_discover", "List configured MCP servers, or discover a server's tools and input schemas. Requires execution authority; selecting a server may start its configured process or remote session.", "mcp.discover", map[string]any{"node": text, "server": text}, []string{"node"}, normal)
	add("control_mcp_call", "Invoke an installed MCP tool on the selected node. Tool-reported isError is a failure with unknown effects, not permission to retry. Use a tracked mcp.call task to retain the full result, including partial output on failure.", "mcp.call", map[string]any{"node": text, "server": text, "tool": text, "arguments": object}, []string{"node", "server", "tool"}, normal)
	add("control_desktop", "Use the opt-in worker-local native agent-desktop adapter, without Python. Discover desktop.run first. request is status, snapshot with app, click/clear with ref, or setValue with ref and exactly one of secret/text. Use secret names for credentials. All native responses mask configured secrets; raw logs are suppressed. No screenshot, clipboard, tracing, or headed input. Refs belong to this adapter's private namespace. Use tracked tasks for retained partial failures; never replay uncertain effects.", "desktop.run", map[string]any{"node": text, "request": desktop.Schema()}, []string{"node", "request"}, func(args map[string]json.RawMessage) (string, any, error) {
		target, params, err := nested("request")(args)
		if err != nil {
			return "", nil, err
		}
		if _, err := desktop.Decode(args["request"]); err != nil {
			return "", nil, err
		}
		return target, params, nil
	})
	add("control_rpa", "Run an opt-in GUI action batch on a worker's logged-in desktop. Discover rpa.run first. Use secret instead of text on setValue/type for worker-local credentials; never request their values. This is the protected credential-entry path during agent-desktop workflows on the same worker/profile; inspect RPA selectors rather than reusing agent-desktop refs. Text results mask configured secrets, but screenshots do not. If unavailable, ask for manual login or helper setup, not shell secret retrieval. Batches are serialized per OS user; use leased tasks for multi-batch workflows. A failed call may have changed the desktop; never replay automatically.", "rpa.run", map[string]any{"node": text, "actions": rpa.Schema()["properties"].(map[string]any)["actions"]}, []string{"node", "actions"}, func(args map[string]json.RawMessage) (string, any, error) {
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
			ctx = context.WithValue(ctx, uploadProgressKey{}, request)
			result, err := call(ctx, request.Params.Arguments)
			if err != nil {
				return toolError(err), nil
			}
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(model.JSON(result))}}}, nil
		})
	}
	for _, resume := range []bool{false, true} {
		name, description := "control_upload", "Stream one regular file from the MCP server host into a worker workspace. Requires node, explicit id, localPath, and remote path. Uses bounded binary streaming, SHA-256 verification, and atomic no-overwrite publication. Does not require a desktop or local enrolled node. Never upload using repeated files.write calls. After failure query control_upload_status, then explicitly resume; never retry an uncertain upload with a new ID."
		required := []string{"node", "id", "localPath", "path"}
		if resume {
			name = "control_upload_resume"
			description = "Explicitly resume a retained upload after inspecting its status. localPath is on the MCP server host and must match the original content. Reuses the pinned worker identity and verifies its durable prefix. Does not silently start a replacement or overwrite files."
			required = []string{"node", "id", "localPath"}
		}
		properties := map[string]any{"node": text, "id": text, "localPath": text, "timeoutSeconds": map[string]any{"type": "integer", "minimum": 1, "maximum": 86400}}
		if !resume {
			properties["path"] = text
		}
		local(name, description, properties, required, func(ctx context.Context, args json.RawMessage) (any, error) {
			var q UploadSpec
			if err := json.Unmarshal(args, &q); err != nil {
				return nil, err
			}
			q.Resume = resume
			if request, ok := ctx.Value(uploadProgressKey{}).(*mcp.CallToolRequest); ok && request.Params.GetProgressToken() != nil {
				q.Progress = func(p upload.Result) {
					progressCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
					defer cancel()
					_ = request.Session.NotifyProgress(progressCtx, &mcp.ProgressNotificationParams{ProgressToken: request.Params.GetProgressToken(), Progress: float64(p.Offset), Total: float64(p.Size), Message: "Upload " + p.State})
				}
			}
			return c.Upload(ctx, q)
		})
	}
	for _, abort := range []bool{false, true} {
		name, description := "control_upload_status", "Read this owner's upload checkpoint or published receipt. A missing/expired record does not prove publication never occurred. Does not transfer bytes or replay publication."
		if abort {
			name = "control_upload_abort"
			description = "Abandon inactive upload staging. Never deletes or rolls back a published destination. An active upload must first be cancelled and inspected."
		}
		local(name, description, map[string]any{"node": text, "id": text}, []string{"node", "id"}, func(ctx context.Context, args json.RawMessage) (any, error) {
			var q struct {
				Node string `json:"node"`
				ID   string `json:"id"`
			}
			if err := json.Unmarshal(args, &q); err != nil {
				return nil, err
			}
			return c.UploadStatus(ctx, q.Node, q.ID, abort)
		})
	}
	local("control_runbooks", "Discover shared or machine-specific operational instructions. requires is agent-friendly guidance: plain text, a list, or optional structured details, preserved even when full content is omitted. Use the user's task and worker evidence to choose a suitable machine; do not invent fixed OS, GPU, or capability constraints. Optional node filters machine bindings and adds worker stores; local: true avoids worker contact. source and file read a complete exact-origin document, with node required for worker origins. Reports source errors and conflicts. Discovery does not execute instructions or retrieve secret values.", map[string]any{"node": text, "file": text, "source": text, "local": map[string]any{"type": "boolean"}}, nil, func(ctx context.Context, args json.RawMessage) (any, error) {
		var q runbooks.Query
		if err := json.Unmarshal(args, &q); err != nil {
			return nil, err
		}
		return c.Runbooks(ctx, q)
	})
	local("control_wake", "Send Wake-on-LAN to an offline fleet machine using verified LAN evidence. Prefers this host when a registered NIC matches, or its OS neighbor table matches the target's registered IP/MAC on the same local interface. A matching awake witness with a verified relationship to the target also qualifies this host. Otherwise selects an online verified worker. A standalone orchestrator needs no node enrollment. Optional from chooses a worker or local. No LAN labels or manual MACs are needed. Requires an account key and retained target wakeInterfaces. Success means packet sent, not awake. Never retries or switches sender after a wake-send failure.", map[string]any{"node": text, "from": text}, []string{"node"}, func(ctx context.Context, args json.RawMessage) (any, error) {
		var spec WakeSpec
		if err := json.Unmarshal(args, &spec); err != nil {
			return nil, err
		}
		return c.Wake(ctx, spec)
	})
	local("control_workflow_reconcile", "Inspect a retained workflow plan and query its stable child IDs after interruption or uncertainty. Queries up to eight children concurrently, with six seconds per child and thirty seconds overall. Returns plan-ordered summaries and bounded errors for unavailable children. complete means every child was observed succeeded. Does not replay work or recreate grants.", map[string]any{"node": text, "id": text}, []string{"node", "id"}, func(ctx context.Context, args json.RawMessage) (any, error) {
		var q struct {
			Node string `json:"node"`
			ID   string `json:"id"`
		}
		if err := json.Unmarshal(args, &q); err != nil {
			return nil, err
		}
		return c.ReconcileWorkflow(ctx, q.Node, q.ID)
	})
	local("control_session", "Inspect this orchestrator process's role, stable task owner, live transport identity, connections, and port forwards. Does not enroll a fleet machine.", map[string]any{}, nil, func(ctx context.Context, _ json.RawMessage) (any, error) {
		return c.Session(ctx)
	})
	local("control_speedtest", "Run an on-demand Control connection test. Measures warmed round-trip latency, jitter, and sequential throughput in both directions over the actual WebRTC or relay bulk stream, with direct/TURN/gateway-relay route diagnostics when available. node is the destination; optional from selects a source worker instead of this orchestrator. Worker-to-worker bytes do not pass through the orchestrator. Requires account-client routing and connection.open permission at the destination, plus connection.test at the source. Uses 16 MiB per direction and 10 samples by default, capped at 256 MiB, 100 samples, and two minutes. Synthetic traffic uses bounded memory, verifies checksums, and leaves no files. Never retries a failed test automatically.", map[string]any{"node": text, "from": text, "bytes": map[string]any{"type": "integer", "minimum": 1, "maximum": 256 << 20}, "samples": map[string]any{"type": "integer", "minimum": 1, "maximum": 100}}, []string{"node"}, func(ctx context.Context, args json.RawMessage) (any, error) {
		var spec ConnectionTestSpec
		if err := json.Unmarshal(args, &spec); err != nil {
			return nil, err
		}
		return c.TestConnection(ctx, spec)
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
	local("control_tunnel_start", "Retain a forward or reverse TCP tunnel in this host's user tunnel service. Returns after durable acceptance, initially starting. Survives CLI/MCP exits and restores listeners after disconnects or restarts; never replays interrupted sockets. Requires an account login, fixed listen HOST:PORT, and an explicit id for reconciliation. ttlSeconds is optional; omitted or zero keeps it until disposed. Run control_tunnel_list to check active/retrying state. Never expose a non-loopback listener without the user's request.", map[string]any{"id": text, "node": text, "address": text, "listen": text, "reverse": map[string]any{"type": "boolean"}, "ttlSeconds": map[string]any{"type": "integer", "minimum": 0, "maximum": 31536000}}, []string{"id", "node", "address", "listen"}, func(ctx context.Context, args json.RawMessage) (any, error) {
		var spec PersistentTunnelSpec
		if err := json.Unmarshal(args, &spec); err != nil {
			return nil, err
		}
		return c.StartPersistentTunnel(ctx, spec)
	})
	local("control_tunnel_list", "List this host login's retained tunnels, including forward/reverse listeners, active/retrying state, absolute expiry, socket counts, and errors. Does not list another login's tunnels.", map[string]any{}, nil, func(ctx context.Context, _ json.RawMessage) (any, error) {
		return c.PersistentTunnels(ctx)
	})
	local("control_tunnel_dispose", "Durably remove a retained tunnel by ID, then close its listener and sockets. Prevents restoration after restart. Idempotent if absent. Does not stop destination services.", map[string]any{"id": text}, []string{"id"}, func(ctx context.Context, args json.RawMessage) (any, error) {
		var q struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(args, &q); err != nil {
			return nil, err
		}
		if err := c.DisposePersistentTunnel(ctx, q.ID); err != nil {
			return nil, err
		}
		return map[string]bool{"disposed": true}, nil
	})
	return server
}

func toolError(err error) *mcp.CallToolResult {
	return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: string(model.JSON(model.OperationError(err, "unknown")))}}}
}
