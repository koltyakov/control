# Configuration and protocol

[Contributor guide](../AGENTS.md) · [Architecture](architecture.md) · [Engineering principles](principles.md) · [Design decisions](decisions.md)

## Node configuration

`control machines add auto --platform OS`, an omitted CLI name, or `auto`/blank in the dashboard selects target-hostname naming. The shared client sends `autoName: true` and an empty `name` in `POST /v1/fleet/installations`. The target resolves its OS hostname, persists it with pending enrollment, and supplies it as signed `name` in redemption. Names are claimed only at redemption and remain fleet-scoped, without replacing another identity or bypassing explicit name reservations. The proof's optional lowercase `name` field is omitted for fixed-name invitations, preserving their signature bytes. Both gateway and pinned installer need automatic-name support; see [installation](installation.md#add-a-machine).

Use [orchestrator, gateway, and worker](terminology.md) for operation roles, and client, node, and gateway for components. These names do not add protocol roles or change configuration identifiers. In particular, `agents` and `agent.run` refer to optional AI integrations.

For generated profiles, background startup, MCP/skill setup, and the invitation API, see [installation and enrollment](installation.md). `control setup` creates a host profile; `control machines add NAME --platform macos|windows|linux` creates an expiring target installer within that user's fleet. The dashboard's `a` wizard creates and copies the same command. Installers detect architecture from the target and select a checksum-pinned binary; signed redemption binds that selection. Windows invitation scripts default to user-login startup under the installing user. Explicit `OS/ARCH` remains supported. Redeemed credentials authorize registration only for their bound user, identity, name, and platform. `CONTROL_PUBLIC_URL` overrides the public gateway address in generated links. See [users and private fleets](users.md) for account APIs and SQLite migration.

On Windows, `setup|enroll --service user` selects a login scheduled task under the installing user's limited interactive token, without a password. Invitation scripts select `CONTROL_SERVICE_MODE` when set, otherwise `user`, and pass it explicitly as `enroll --service`. Set `$env:CONTROL_SERVICE_MODE = 'auto'` to request an automatic LocalService SCM service. New user-mode installations do not require elevation; system startup and migration from an existing system service require Administrator PowerShell under the intended user. `control service start --mode user|auto` changes startup mode while retaining the profile and identity. `<node-config>.startup.json` stores the Windows selection for subsequent service commands and direct re-enrollment. Direct `control setup` retains its system-mode default. See [Windows user-login startup](installation.md#switch-windows-to-user-login-startup).

Running an invitation on an existing profile replaces its enrollment and can rename the same identity. Redemption atomically rotates the installation credential and updates the directory entry within the identity's existing fleet. Same-name invitations include `replaceId` and require that identity's signature. Previous invitation credentials are revoked; installed identities must use their current bound credential. See [replacement and recovery](installation.md#single-use-and-recovery).

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

`control update [--version TAG]`, alias `control upgrade`, updates only the invoked CLI from public GitHub Releases without gateway credentials. `CONTROL_RELEASE_REPO` selects the repository and `CONTROL_VERSION` the default tag. The administrative `update push|status|check` subcommands remain superuser-only. Superuser login persists a gateway-scoped `update-admin.json`, independently of the regular fleet login. `make update` first runs `update authorize --check`, which verifies selected credentials without prompting or saving anything. Update commands prefer explicit global `--token`, then `CONTROL_SUPERUSER_KEY`, then matching saved update authorization, before the usual gateway credential selection. Explicit `update authorize` can prompt and save separate authorization; `--key-stdin` accepts a replacement key for noninteractive validation and storage. It cannot be combined with `--check`. See [CLI updates](updates.md#update-the-local-cli) and [development updates](updates.md#push-from-a-development-machine).

`metricsIntervalSeconds` controls local resource sampling, independently of dashboard activity polls. The default is 15 seconds; 1..3600 changes the interval and `-1` disables periodic collection. Startup, heavy-task completion, and explicit refresh requests can still sample. See [dashboard and system metrics](dashboard.md).

Omitting `allow` trusts enrolled nodes in the same user's fleet. Setting `"allow": {}` denies remote calls except valid artifact download grants within that fleet. Neither grants nor wildcard rules bypass fleet membership. Patterns use Go `path.Match` syntax. For example, `tasks.*` permits task management, but `tasks.start` also requires permission for its requested capability. Task status and logs remain owner-scoped. The local API can inspect every task owned by its node or executed locally.

## Operations

### Gateway observation

Authenticated `GET /v1/status` returns a JSON pool snapshot with `observedAt`, `gateway`, and `nodes`. `gateway` contains the configured public `url`, running `software` build metadata, service `startedAt`, and cached host `system` metrics. The URL may be empty when `CONTROL_PUBLIC_URL` is unset; the CLI displays its configured connection URL. Sampling runs at startup and every 15 seconds independently of requests. Disk paths use the label `state`, and collection errors are sanitized.

`nodes` contains only the authenticated credential owner's fleet, including for superuser requests, which see the legacy fleet. Directory-only entries have status `online` or `offline` and registration-time metrics. Account keys also receive fresh node-health reports with status `summary`, numeric `activeCount`, boolean `leased`, cached `system`, and the gateway receipt time in `observedAt`. These reports contain no active/recent records. Common keys do not receive them. The dashboard optionally enriches matching IDs through the local-only `activities.pool` API, producing status `ready` with authorized activity details. Its optional `notice` describes missing live machine health. Every valid credential may read status; unauthenticated, revoked, and disabled credentials receive HTTP 401. The endpoint grants no task control, account administration, or update permissions.

`GET /v1/auth` advertises `nodeHealth: true` when the gateway accepts reports. Compatible nodes send a `node.health` routing packet with empty `to` every five seconds, beginning after connection. Its JSON data is limited to 16 KiB and contains `activeCount`, `leased`, and `system`. The gateway binds the report to the authenticated connection, ignores the supplied sender ID, and never forwards health packets to peers. Reports expire after 20 seconds without receipt and are not persisted. Nodes redact filesystem paths and raw collection errors. Resource sample timestamps retain their original collection times. Reporting stops with node shutdown and is disabled for older gateways that omit the feature flag, preserving nodes-first managed updates.

### Machine lifecycle

Account keys can `PATCH /v1/fleet/nodes/{id}` with `{"disabled":true}` or `false`, and `DELETE /v1/fleet/nodes/{id}?stop=true` to unregister. These routes are fleet-scoped and commit policy in SQLite before acknowledgement. `/v1/auth` includes the `machines.manage` capability for account keys. The directory and dashboard include `disabled`, `controlPending`, and `software`. Directory records also indicate `managed` once that executable has demonstrated lifecycle support. An online enabled node with pending policy is excluded from `nodes.select` until it acknowledges the current revision.

Unregister accepts online and offline registrations regardless of `managed`. It removes the directory entry, revokes bound installation credentials, closes the gateway connection, and persists a tombstone that rejects future registration with that identity. Lifecycle-capable nodes stop on their next state query; older nodes require a local stop if still running. A new invitation can reinstall a retired local profile using a fresh identity and state directory while retaining its workspace and settings.

Nodes query `POST /v1/node/state` at startup and every two seconds. This endpoint takes `publicKey`, `signedAt`, `revision`, and `signature`, where the signature is Ed25519 over `model.MachineStateProof.Message()`, including purpose `control-machine-state-v1`. Requests are limited to 4 KiB; timestamps must be within five minutes. The signing identity alone selects the record. The response contains monotonic `revision`, `disabled`, and `unregistered`. No account, directory, work, or foreign identity information is returned. Unlike ordinary endpoints, this read uses proof of the node's private key instead of a bearer credential, so an unregistered node can receive its stop policy after credential revocation. A subsequent signed query acknowledges the revision only after local persistence and admission changes.

Installers send optional `inspectOnly: true`, included in the signed proof, to read retirement status without changing lifecycle support or acknowledgements for the running node. Omitting this field preserves the original proof format. Gateways must support this field before accepting new installers; existing nodes keep using the original format.

Nodes persist policy in `machine-state.json`. Disable rejects new work independently of managed-update reservations while allowing already accepted work to finish, status queries, cancellation, lease cleanup, and idempotent task recovery. Unregister cancels the node context and cleanly stops its supervisor/service. Tombstones prevent re-registration with the same identity even using another valid key. Offline changes apply when the node can reach the gateway. Old gateways returning 404 for the state endpoint remain compatible with new nodes; local persisted restrictions are retained.

### Node operations

Remote CLI and MCP operations use a running local node when available. Otherwise, unless `--api` or `CONTROL_API` is explicit, a read-only local API probe that fails with connection refusal selects an outbound-only client session using the saved gateway credential. This session uses the same account checks, pinned TLS, framed requests, and streaming methods as nodes, but never enrolls as a machine. It exposes no execution capabilities or local API. Backend selection happens before work and never retries an uncertain operation. See [standalone CLI and MCP](installation.md#standalone-cli-and-mcp) for support negotiation, identity storage, ownership, and concurrency limits.

`GET /v1/auth` advertises `clientSessions: true` on compatible gateways. Clients connect through `GET /v1/client/connect`, not `/v1/connect`. The handshake's `Hello` has `client: true`; its Ed25519 signature covers the fresh challenge, `control-client-session-v1` followed by a NUL byte, and the JSON identity metadata. Machine signatures retain their original challenge-plus-metadata encoding. Client names are fixed to `cli-<first-16-identity-hex-digits>` for access-rule lookup, not machine discovery. Client metadata must have no capabilities, labels, or system samples. Installed-machine credentials cannot open these sessions. The gateway commits account ownership and client role before returning `ready`, and limits each account to 64 live sessions.

`GET /v1/peers/{id}` is an authenticated, account-scoped identity lookup used by workers before accepting peer streams. It returns machine metadata or a connected client's transient identity metadata; an unknown, disconnected client or foreign ID returns 404. `/v1/nodes` and `/v1/status` contain only enrolled machines. Clients cannot send `node.health` or `update.status` packets, and never join managed rollouts. Routing still overwrites the sender and checks the destination account for every packet. Compatible machine records include `clientSessions: true`, negotiated with the gateway before signing. Standalone execution rejects older targets before sending work.

Concurrent clients also require `clientOwners: true` in authenticated gateway metadata and target records. Each process uses an ephemeral TLS identity, with metadata `clientOwner` naming the stable owner ID and `name` fixed to `cli-<first-16-owner-hex-digits>`. `Hello.ownerKey` is the owner's Ed25519 public key; `ownerSignature` signs `control-client-owner-v1` plus a NUL byte followed by `Hello.Message(challenge)`. The owner hash must match `clientOwner`. Both owner and transport signatures cover the fresh challenge and connection metadata. The gateway persists immutable account and transport-to-owner bindings before `ready`. Machine registration cannot supply `clientOwner` or owner proofs. Client owners and transports cannot enroll as machines or change their bindings.

Workers get `clientOwner` only from scoped gateway lookup and use it for tasks, leases, provider execution ownership, and activity owner fields. Access rules accept that owner ID, the transport ID, wildcard patterns, or the derived client name. TLS and artifact grants still use the transport ID; a grant for one process does not authorize a different process sharing its task owner. Older single-identity client sessions remain accepted by upgraded servers. New concurrent clients reject older gateways and workers before work, without re-enrollment or changing task ownership.

`peerChannels: true` in `/v1/auth` permits compatible peers to advertise the same signed field. When both endpoints support it, control, bulk, and interactive lanes share a WebRTC carrier through separate ordered reliable data channels. Initial channels retain label `control-v1`; added channels use `control-v1/<32-lowercase-hex-session-id>` and perform fresh pinned TLS and yamux setup. The routing envelope is unchanged. Older gateways omit the flag, and older peers use independent connections. Relay lanes remain separate multiplexers on one gateway WebSocket. Admission, buffer limits, and failure behavior are described in [fleet streaming](streaming.md).

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
| `nodes.select` | `labels`, optional `capability`; returns first online, enabled, policy-acknowledged machine match by name; clients are not in this directory |
| `node.describe` | `{}`; returns ID, capabilities, connections, lane/session stream counts, configured agent/MCP names |
| `system.info` | Optional `refresh`; otherwise returns the cached OS, CPU, RAM, and disk sample |
| `activities.list` | Optional `recent`, 0..64; node-wide activity metadata and cached resources, subject to access rules |
| `activities.pool` | Optional `nodes` array and `recent`; local-API-only aggregation, including offline and unavailable machines |
| `capabilities.list` | `{}`; descriptions and input schemas |
| `tasks.start` | A task specification |
| `tasks.get`, `tasks.cancel` | `id` |
| `tasks.logs` | `id`, optional byte `offset`; returns `text`, next `offset`, `terminal`; streaming clients can use peer-only `follow: true` negotiation |
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
| `rpa.run` | Opt-in `actions` array, 1..100 GUI actions; serialized per OS user, two-minute invocation limit; see [GUI automation](rpa.md) |

`artifacts.open` and `tcp.open` are streaming peer methods, not JSON API calls. Use the CLI's `artifact get` and `tunnel` commands, or the local API's `POST /v1/download` and WebSocket `GET /v1/tunnel?target=NODE&address=HOST:PORT`.

### Orchestrator sessions and forwards

`control session` and MCP `control_session` describe the requesting process. The result has `role` of `client` or `node-api`, `ownerId`, `transportId`, a peer-ID-to-transport `connections` map, `sessions`, and process-owned `forwards`. Sessions contain `id`, `peer`, `lane`, `mode`, `outgoingStreams`, and `incomingStreams`. Receiver-side sessions use lane `incoming` because their application methods are independently dispatched. A standalone process keeps its owner across invocations but creates a new transport identity each time. A local API uses its node's identity for both. Inspection does not enroll a machine or change task ownership.

| MCP tool | Parameters and result |
| --- | --- |
| `control_forward_start` | Required `node`, remote `address` as `HOST:PORT`, optional local `listen`, default `127.0.0.1:0`; returns forward metadata immediately |
| `control_forward_list` | `{}`; returns this process's forward metadata array |
| `control_forward_stop` | `id`; closes the listener and all active sockets, returns `stopped: true` |
| `control_session` | `{}`; returns the process session metadata |

Forward metadata contains `id`, `node`, `address`, actual bound `listen`, `activeConnections`, and optional `lastError`. A successful start means the local listener is bound, not that the remote service accepted a connection. No dummy remote connection is opened. Each accepted socket performs a separately authorized `tcp.open`. Errors close that socket and update `lastError` without replay; a later local socket may establish a new peer stream. The worker resolves and dials the remote address. Non-loopback listeners are an explicit choice and expose the service to that local network without additional listener authentication.

Forwards survive the start tool's request context, but belong to that MCP process until stopped or the process exits. The CLI's `control tunnel NODE HOST:PORT [--listen ADDRESS]` uses the same implementation and stays in the foreground until cancelled. Another process cannot list or stop its listeners, but can independently manage tasks sharing the stable owner. At most 32 forwards and 128 active sockets per forward are retained. Stop and shutdown cancel pending dials, close sockets, and join copy goroutines. Negotiated duplex streams preserve the response direction after write-side EOF. Errors and cancellation abort both directions; older raw endpoints retain full-close behavior. UDP, reverse forwarding, PTY sessions, and detached local forwarding daemons are not supported. Closing a forward does not stop its remote service or cancel durable tasks.

New TCP clients request `tcp.open` with `address` and `duplex: 1`. Compatible receivers return `connected: true, duplex: 1`, then exchange four-byte big-endian record lengths and raw payloads of at most 32 KiB. Zero length means write-side EOF; full shutdown still closes the underlying stream. Unknown nonzero versions are rejected. Missing or zero duplex acknowledgement means legacy raw bytes, with no replay or second TCP dial. Local clients request WebSocket `/v1/tunnel?...&duplex=1`; response header `Control-Tunnel-Duplex: 1` negotiates the same records. Each hop negotiates independently.

Streaming log clients send peer `tasks.logs` with `id`, `offset`, and `follow: true`. A compatible receiver checks the existing `tasks.logs` permission and task owner before returning `stream: "task-logs-v1"`. Subsequent length-prefixed JSON records contain base64 `data`, next byte `offset`, and `terminal`, or `error`. Records are limited to 64 KiB of decoded bytes and 128 KiB of framing. Completion is sent only after draining retained bytes. Older receivers return an ordinary snapshot, which the shared client follows with read-only polling. The local API exposes authenticated `GET /v1/logs?target=NODE&id=ID&offset=BYTES` as flushed NDJSON with the same records. A missing older API route permits snapshot polling; permission failures and interrupted streams do not. Standard JSON calls and MCP `control_task_logs` remain snapshot interfaces. Reattach explicitly by offset after a failure; log followers never resubmit or cancel the task.

`control exec NODE [--detach] [--id ID] [--timeout DURATION] [--] COMMAND [ARG...]` submits a durable `exec.run` task. It prints the chosen task ID before submission. Without `--detach` it waits for completion; with it, it returns after acceptance. Timeout defaults to one hour, accepts 1s through 24h, and includes queue/input time. `control task logs NODE ID [--follow] [--offset BYTES]` either returns one structured log chunk or follows text until completion. Following drains remaining terminal chunks. Cancelling a follower or wait never cancels the task; use `control task cancel NODE ID`. Accepted tasks survive client disconnects and remain subject to their existing restart and idempotency rules.

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

The optional `rpa` command configuration registers the built-in `rpa.run` adapter. Its helper receives the same version-1 request envelope below, with a private temporary `workspace` for screenshots. It returns `{"results":[...]}` and optional `error`; completed screenshot results use a basename `image` which the node replaces with an immutable PNG `artifact` reference. The helper must stop at the first action error and must not retry side effects. Control validates all arguments against the shared capability/MCP schema before execution. The supplied Python helper adds OS-specific validation and native accessibility backends. See [GUI automation](rpa.md) for the complete action contract, dependencies, permissions, serialization, and failure semantics.

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
- 256 transport links, 32 concurrent setup operations, 128 streams per multiplexer, and 512 incoming handlers per peer process; outgoing slots are split between control, bulk, and interactive traffic. See [streaming limits](streaming.md#limits-and-cleanup).
- 1 MiB per filesystem read/write, 2 MiB per HTTP response.
- Commands retain the first 2 MiB of each stdout/stderr stream in their result. `truncated` indicates additional output.
- Task logs retain the first 10 MiB and support 64 KiB reads by byte offset.
- Four simultaneous worker tasks by default, configurable through `maxTasks`; at most 256 accepted unfinished tasks.
- Coordination workflows do not consume a worker slot, so a workflow can run a local step with `maxTasks: 1`.
- Task timeouts and artifact/lease TTLs are at most 24 hours. A task defaults to one hour.
- At most 100 steps per workflow. Steps execute sequentially in dependency order.
- Artifacts, completed task metadata, logs, and workspaces remain until explicitly removed. `artifacts.delete` removes an artifact; task/workspace pruning is currently an operator action while the node is stopped.

The runtime enforces concurrency and message-size bounds, but does not implement per-user billing, CPU/memory isolation, or disk quotas.
