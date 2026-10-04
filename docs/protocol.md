# Configuration and protocol

[Contributor guide](../AGENTS.md) · [Architecture](architecture.md) · [Engineering principles](principles.md) · [Design decisions](decisions.md)

## Node configuration

For generated profiles, user startup, MCP/skill setup, and the invitation API, see [installation and enrollment](installation.md). `control setup` creates a host profile; `control machines add NAME --platform OS/ARCH` creates an expiring target installer within that user's fleet. Redeemed credentials authorize registration only for their bound user, identity, name, and platform. `CONTROL_PUBLIC_URL` overrides the public gateway address in generated links. See [users and private fleets](users.md) for account APIs and SQLite migration.

```json
{
  "name": "linux-media-01",
  "gateway": "https://control.example.com",
  "dataDir": "state",
  "workDir": "workspace",
  "listen": "127.0.0.1:7331",
  "labels": { "os": "linux", "role": "media" },
  "relayOnly": false,
  "maxTasks": 4,
  "metricsIntervalSeconds": 15,
  "iceServers": [{ "urls": ["stun:your-stun-server.example.com:3478"] }],
  "allow": {
    "orchestrator": ["*"],
    "consumer": ["node.describe", "artifacts.open", "artifacts.pull"]
  }
}
```

Use `CONTROL_TOKEN` or the `token` configuration property for the node's user-scoped credential. `dataDir` and `workDir` resolve relative to the config file. Provider/agent/MCP commands resolve through the node's PATH. A command's optional `dir` resolves relative to its execution workspace unless absolute. `env` adds or overrides environment variables.

The configured token can be a user's issued common key or redeemed installation key. The optional gateway bootstrap token belongs only to the legacy fleet. User fleet management uses `CONTROL_USER_KEY`; gateway administration uses a separate `CONTROL_SUPERUSER_KEY`. `GET /v1/auth` returns `role`, `userId`, and management capabilities. Directory records include the authoritative `userId`, and new peers require fleet-aware gateway authentication. See [managed updates](updates.md) for release settings, bundle manifests, and update control messages. Machine registration includes `software` with the running version, OS, architecture, and executable SHA-256. Version JSON and administrative update status also include the embedded `releaseRepo` and UTC `buildTime` when injected. `CONTROL_RELEASE_REPO` overrides the embedded repository; an explicitly empty value disables release polling.

`iceServers` uses Pion's JSON configuration, including `urls`, `username`, and `credential` for TURN. With no ICE servers, WebRTC attempts host connectivity and uses the gateway relay when necessary. `relayOnly` skips WebRTC entirely.

`metricsIntervalSeconds` controls local resource sampling, independently of dashboard activity polls. The default is 15 seconds; 1..3600 changes the interval and `-1` disables periodic collection. Startup, heavy-task completion, and explicit refresh requests can still sample. See [dashboard and system metrics](dashboard.md).

Omitting `allow` trusts enrolled nodes in the same user's fleet. Setting `"allow": {}` denies remote calls except valid artifact download grants within that fleet. Neither grants nor wildcard rules bypass fleet membership. Patterns use Go `path.Match` syntax. For example, `tasks.*` permits task management, but `tasks.start` also requires permission for its requested capability. Task status and logs remain owner-scoped. The local API can inspect every task owned by its node or executed locally.

## Operations

Use `control call NODE METHOD JSON` or `POST /v1/call` on the local API:

```json
{
  "target": "linux-media-01",
  "method": "tasks.start",
  "params": {
    "id": "encode-123",
    "capability": "exec.run",
    "args": { "command": "ffmpeg", "args": ["-version"] }
  }
}
```

Authenticate with `Authorization: Bearer TOKEN`. Responses contain `result` or `error`.

| Method | Parameters |
| --- | --- |
| `nodes.list` | `{}` |
| `nodes.select` | `labels`, optional `capability`; returns first online match by name |
| `node.describe` | `{}`; returns ID, capabilities, connections, configured agent/MCP names |
| `system.info` | Optional `refresh`; otherwise returns the cached OS, CPU, RAM, and disk sample |
| `activities.list` | Optional `recent`, 0..64; node-wide activity metadata and cached resources, subject to access rules |
| `activities.pool` | Optional `nodes` array and `recent`; local-API-only aggregation, including offline and unavailable machines |
| `capabilities.list` | `{}`; descriptions and input schemas |
| `tasks.start` | A task specification |
| `tasks.get`, `tasks.cancel` | `id` |
| `tasks.logs` | `id`, optional byte `offset`; returns `text`, next `offset`, `terminal` |
| `tasks.list` | `{}`; owner-scoped |
| `leases.acquire` | Optional `ttlSeconds`, default 300 |
| `leases.renew` | `id`, optional `ttlSeconds` |
| `leases.release` | `id`; fails while its tasks remain active |
| `leases.get` | `{}` |
| `artifacts.export` | `path` relative to `workDir` |
| `artifacts.list` | `{}` |
| `artifacts.delete` | `id` |
| `artifacts.grant` | `id`, recipient `target`, optional `ttlSeconds` |
| `artifacts.pull` | Full `artifact` reference; resumes an existing partial |
| `artifacts.deliver` | `id`, recipient `target`, optional `ttlSeconds` |
| `mcp.discover` | Optional `server`; omitted lists configured servers |
| `mcp.call` | `server`, `tool`, `arguments` |
| `mcp.request` | `server`, `method`, `params` for resource/prompt operations |
| `exec.run` | `command`, optional `args`, `env`, `dir`, `stdin` |
| `agent.run` | Configured `agent` name, `prompt` |
| `http.request` | `url`, optional `method`, `headers`, text `body` |
| `files.list` | Optional `path` |
| `files.read` | `path`, optional byte `offset`, `limit` |
| `files.write` | `path`, base64 `data`, optional byte `offset`, `truncate` |
| `workflow.run` | `steps`; see `examples/workflow-task.json` |

`artifacts.open` and `tcp.open` are streaming peer methods, not JSON API calls. Use the CLI's `artifact get` and `tunnel` commands, or the local API's `POST /v1/download` and WebSocket `GET /v1/tunnel?target=NODE&address=HOST:PORT`.

## Task specification

```json
{
  "id": "job-001",
  "capability": "exec.run",
  "args": { "command": "ffmpeg", "args": ["-i", "input.mp4", "output.mp4"] },
  "inputs": [{ "artifact": { "node": "SOURCE_ID", "id": "SHA256", "sha256": "SHA256", "size": 1234, "name": "input.mp4" }, "path": "input.mp4" }],
  "outputs": ["output.mp4"],
  "timeoutSeconds": 3600,
  "leaseId": "OPTIONAL_LEASE_ID"
}
```

Input and output paths are relative to the task's workspace. The filesystem API is rooted at `workDir` and rejects paths or symlinks that escape that root. Commands are unrestricted OS processes and may explicitly select another working directory; declared task outputs still resolve against the original task workspace.

Tasks move through `queued`, `running`, and a terminal state of `succeeded`, `failed`, `cancelled`, or `interrupted`. A submitted ID must be 1..128 ASCII letters, digits, underscores, or hyphens. Omit it to generate one, although generating it before submission makes uncertain network outcomes easier to reconcile.

Active tasks also expose a `phase`, such as waiting for a worker slot, fetching inputs, executing, or publishing outputs. Phase updates support observation; they do not change task state or restart semantics.

## Shared-pool leases

Select a machine, acquire its lease, then include the returned ID on every task submitted while holding it:

```sh
control call '' nodes.select '{"labels":{"role":"worker"}}'
control call worker leases.acquire '{"ttlSeconds":300}'
control task start worker '{"capability":"exec.run","leaseId":"LEASE_ID","args":{"command":"ffmpeg","args":["-version"]}}'
control call worker leases.release '{"id":"LEASE_ID"}'
```

Selection is advisory; lease acquisition is the atomic reservation. Retry selection if another orchestrator acquires the chosen node first. Acquire fails if tasks or synchronous provider invocations are active. Synchronous capability calls are disabled while a lease exists; use a tracked task with the lease ID. Leases do not automatically cancel a task on expiry, and an expired lease with running tasks continues to block a new owner until those tasks terminate.

## Subprocess providers

Add a capability to `providers`:

```json
{
  "providers": {
    "custom.echo": {
      "command": "python3",
      "args": ["/absolute/path/to/examples/provider.py"],
      "description": "Echo structured input",
      "inputSchema": { "type": "object", "properties": { "message": { "type": "string" } } }
    }
  }
}
```

The runtime starts one subprocess per invocation and writes this JSON to stdin:

```json
{"version":"1","arguments":{"message":"hello"},"workspace":"/absolute/task/workspace"}
```

The process writes one JSON result to stdout, diagnostics to stderr, and exits zero on success. Cancellation terminates the process tree using process groups on Unix and `taskkill /T` on Windows. Providers own argument validation against their advertised schemas. Use artifacts for large outputs.

## Limits and retention

- 8 MiB control frames; 16 KiB transport chunks.
- 1 MiB per filesystem read/write, 2 MiB per HTTP response.
- Commands retain the first 2 MiB of each stdout/stderr stream in their result. `truncated` indicates additional output.
- Task logs retain the first 10 MiB and support 64 KiB reads by byte offset.
- Four simultaneous worker tasks by default, configurable through `maxTasks`; at most 256 accepted unfinished tasks.
- Coordination workflows do not consume a worker slot, so a workflow can run a local step with `maxTasks: 1`.
- Task timeouts and artifact/lease TTLs are at most 24 hours. A task defaults to one hour.
- At most 100 steps per workflow. Steps execute sequentially in dependency order.
- Artifacts, completed task metadata, logs, and workspaces remain until explicitly removed. `artifacts.delete` removes an artifact; task/workspace pruning is currently an operator action while the node is stopped.

The runtime enforces concurrency and message-size bounds, but does not implement per-user billing, CPU/memory isolation, or disk quotas.
