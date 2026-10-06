# Configuration and protocol

[Contributor guide](../AGENTS.md) · [Architecture](architecture.md) · [Engineering principles](principles.md) · [Design decisions](decisions.md)

## Node configuration

`control machines add auto --platform OS`, an omitted CLI name, or `auto`/blank in the dashboard selects target-hostname naming. The shared client sends `autoName: true` and an empty `name` in `POST /v1/fleet/installations`. The target resolves its OS hostname, persists it with pending enrollment, and supplies it as signed `name` in redemption. Names are claimed only at redemption and remain fleet-scoped, without replacing another identity or bypassing explicit name reservations. The proof's optional lowercase `name` field is omitted for fixed-name invitations, preserving their signature bytes. Both gateway and pinned installer need automatic-name support; see [installation](installation.md#add-a-machine).

Use [orchestrator, gateway, and worker](terminology.md) for operation roles, and client, node, and gateway for components. These names do not add protocol roles or change configuration identifiers. In particular, `agents` and `agent.run` refer to optional AI integrations.

For generated profiles, background startup, MCP/skill setup, and the invitation API, see [installation and enrollment](installation.md). `control setup` creates a host profile; `control machines add NAME --platform macos|windows|linux` creates an expiring target installer within that user's fleet. The dashboard's `a` wizard creates and copies the same command. Installers detect architecture from the target and select a checksum-pinned binary; signed redemption binds that selection. Windows invitation scripts default to user-login startup under the installing user. Explicit `OS/ARCH` remains supported. Redeemed credentials authorize registration only for their bound user, identity, name, and platform. `CONTROL_PUBLIC_URL` overrides the public gateway address in generated links. See [users and private fleets](users.md) for account APIs and SQLite migration.

`machines add --service user|system` and the dashboard's startup-context step send optional `serviceMode` in `POST /v1/fleet/installations`. The gateway validates and persists it in invitation metadata and echoes it in the link, registry, and public info responses. Explicit context overrides script environment defaults. Clients reject missing or mismatched confirmation without retrying the request. Omitted context retains the previous platform defaults. The field does not change signed identity redemption or authorize OS elevation. Unix system mode requires root, uses a systemd system unit or launchd LaunchDaemon, and never falls back to user/process startup. Windows system context maps to `auto`, the existing LocalService service. See [startup contexts and paths](installation.md#choose-a-startup-context).

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
    "@orchestrator": ["*"],
    "*": ["node.describe", "capabilities.list"]
  }
}
```

Use `CONTROL_TOKEN` or the `token` configuration property for the node's user-scoped credential. `dataDir` and `workDir` resolve relative to the config file. Provider/agent/MCP commands resolve through the node's PATH. A command's optional `dir` resolves relative to its execution workspace unless absolute. `env` adds or overrides environment variables.

The configured token can be a user's issued common key or redeemed installation key. The optional gateway bootstrap token belongs only to the legacy fleet. User fleet management uses `CONTROL_USER_KEY`; gateway administration uses a separate `CONTROL_SUPERUSER_KEY`. `GET /v1/auth` returns `role`, `userId`, and management capabilities. Directory records include the authoritative `userId`, and new peers require fleet-aware gateway authentication. See [managed updates](updates.md) for release settings, bundle manifests, and update control messages. Machine registration includes `software` with the running version, OS, architecture, and executable SHA-256. Version JSON and administrative update status also include the embedded `releaseRepo` and UTC `buildTime` when injected. `CONTROL_RELEASE_REPO` overrides the embedded repository; an explicitly empty value disables release polling.

`iceServers` uses Pion's JSON configuration, including `urls`, `username`, and `credential` for TURN. With no ICE servers, WebRTC attempts host connectivity and uses the gateway relay when necessary. `relayOnly` skips WebRTC entirely.

`control update [--version TAG]`, alias `control upgrade`, updates only the invoked CLI from public GitHub Releases without gateway credentials. `CONTROL_RELEASE_REPO` selects the repository and `CONTROL_VERSION` the default tag. The administrative `update push|status|check` subcommands remain superuser-only. Superuser login persists a gateway-scoped `update-admin.json`, independently of the regular fleet login. `make update` first runs `update authorize --check`, which verifies selected credentials without prompting or saving anything. Update commands prefer explicit global `--token`, then `CONTROL_SUPERUSER_KEY`, then matching saved update authorization, before the usual gateway credential selection. Explicit `update authorize` can prompt and save separate authorization; `--key-stdin` accepts a replacement key for noninteractive validation and storage. It cannot be combined with `--check`. See [CLI updates](updates.md#update-the-local-cli) and [development updates](updates.md#push-from-a-development-machine).

An unchanged version skips CLI replacement and managed-service staging/restart, even if a rebuild changes checksums. `POST /v1/admin/updates` with the selected version returns the existing deployment rather than replacing its manifest or restarting its rollout. Node `update.status` reports `applied` for a matching running version or checksum on the target platform. Fresh deployment acknowledgements and idle reservations remain mandatory before any actual managed restart. Downloaded binaries still require exact size, checksum, platform, and executable-version validation. See [D047](decisions.md#d047-skip-updates-for-unchanged-versions).

`metricsIntervalSeconds` controls local resource sampling, independently of dashboard activity polls. The default is 15 seconds; 1..3600 changes the interval and `-1` disables periodic collection. Startup, heavy-task completion, and explicit refresh requests can still sample. See [dashboard and system metrics](dashboard.md).

Omitting `allow` permits same-fleet discovery and account-client execution, not independent worker execution. Setting `"allow": {}` denies incoming calls except previously issued valid delegation or artifact grants. Neither grants nor wildcard rules bypass fleet membership. Patterns use Go `path.Match` syntax. `@orchestrator` selects account-authenticated clients. `tasks.start` also requires permission for its capability. Task status and logs remain owner-scoped. The local API can inspect tasks owned by its node or executed locally. See [delegation](delegation.md).

## Operations

Windows invitation scripts also pass `enroll --firewall`. This configures profile-scoped inbound/outbound UDP and outbound gateway-port TCP rules for the installed executable and stable managed runtime path. A separate `service firewall` invocation requests UAC when needed without elevating a user-login node. Direct enrollment can opt in with `--firewall`; existing hosts can run `service firewall` without restarting. No inbound TCP API port is opened. The pinned installer must support this option before the updated script is served. See [Windows firewall setup and recovery](installation.md#windows-firewall).

### Gateway observation

Machine lists in `GET /v1/nodes` and `GET /v1/status` sort alphabetically by name, ignoring case, with original spelling breaking case-only ties. This ordering does not change case-sensitive name lookup or fleet uniqueness.

Authenticated `GET /v1/status` returns a JSON pool snapshot with `observedAt`, `gateway`, and `nodes`. `gateway` contains the configured public `url`, running `software` build metadata, service `startedAt`, and cached host `system` metrics. The URL may be empty when `CONTROL_PUBLIC_URL` is unset; the CLI displays its configured connection URL. Sampling runs at startup and every 15 seconds independently of requests. Disk paths use the label `state`, and collection errors are sanitized.

`nodes` contains only the authenticated credential owner's fleet, including for superuser requests, which see the legacy fleet. Directory-only entries have status `online` or `offline` and registration-time metrics. Account keys also receive fresh node-health reports with status `summary`, numeric `activeCount`, boolean `leased`, cached `system`, and the gateway receipt time in `observedAt`. These reports contain no active/recent records. Common keys do not receive them. The dashboard optionally enriches matching IDs through account-client `activities.pool` aggregation, producing status `ready` with authorized activity details. Its optional `notice` describes missing live machine health. Every valid credential may read status; unauthenticated, revoked, and disabled credentials receive HTTP 401. The endpoint grants no task control, account administration, or update permissions.

`GET /v1/auth` advertises `nodeHealth: true` when the gateway accepts reports. Compatible nodes send a `node.health` routing packet with empty `to` every five seconds, beginning after connection. Its JSON data is limited to 16 KiB and contains `activeCount`, `leased`, and `system`. The gateway binds the report to the authenticated connection, ignores the supplied sender ID, and never forwards health packets to peers. Reports expire after 20 seconds without receipt and are not persisted. Nodes redact filesystem paths and raw collection errors. Resource sample timestamps retain their original collection times. Reporting stops with node shutdown and is disabled for older gateways that omit the feature flag, preserving nodes-first managed updates.

Managed-update progress uses snapshot status `update`, taking precedence over health and peer observation. Only fresh reports for the selected deployment count as active progress; a current-version node still paused for maintenance also remains `update`. A matching `restarting` report for a node that needs replacement starts a one-minute grace. Directory records carry gateway-owned `updateUntil`, persisted in SQLite and retained on reconnection; supplied enrollment values cannot set it. A restart disconnect extends the grace to one minute after disconnection. A matching report with the selected software version or checksum and `paused: false` clears the grace durably, allowing normal idle/busy observation immediately. Snapshot `online` and `lastSeen` retain actual connection presence, and update readiness still requires fresh acknowledgements. Expired grace falls back to normal status, including `offline` for nodes that have not returned.

### Machine lifecycle

Account keys can `PATCH /v1/fleet/nodes/{id}` with `{"disabled":true}` or `false`, and `DELETE /v1/fleet/nodes/{id}?stop=true` to unregister. These routes are fleet-scoped and commit policy in SQLite before acknowledgement. `/v1/auth` includes the `machines.manage` capability for account keys. The directory and dashboard include `disabled`, `controlPending`, and `software`. Directory records also indicate `managed` once that executable has demonstrated lifecycle support. An online enabled node with pending policy is excluded from `nodes.select` until it acknowledges the current revision.

The same PATCH route accepts `{"name":"render-01"}` instead of `disabled` to rename a registered machine. Names follow the enrollment syntax and cannot claim another registration, a live invitation reservation, or a reserved client name in that fleet. The response includes optional `name` in the persisted machine policy. This overrides signed registration names only after identity and fleet verification. Renaming does not increment the admission-policy revision, require lifecycle support, revoke credentials, or disconnect the node. Original signed invitation names remain intact for redemption recovery; reservation and bound-credential checks use the owner-selected name. Fresh replacement enrollment clears the override transactionally. The CLI uses `control machines rename NAME NEW_NAME`; the dashboard uses `m`, select, then `r`.

After committing a rename, the gateway sends `directory.changed` with sender `@gateway` to connected peers only in that fleet. This packet has no payload and cannot originate from a peer. Compatible peers invalidate name caches, retaining identity-bound sessions and streams. A directory generation prevents an in-flight lookup from caching a pre-rename alias after notification. Gateway reconnection also invalidates names to cover missed notifications. Older peers ignore the packet and may retain cached aliases until restarted. Name changes are asynchronous at cached callers, so stable IDs remain preferable for authorization and in-flight coordination.

Unregister accepts online and offline registrations regardless of `managed`. It removes the directory entry, revokes bound installation credentials, closes the gateway connection, and persists a tombstone that rejects future registration with that identity. Current installations cancel work and uninstall their startup registration and unshared launcher on their next state query. Older lifecycle-capable nodes only stop; nodes without lifecycle support require local cleanup if still running. A new invitation can reinstall a retired local profile using a fresh identity and state directory while retaining its workspace and settings.

Nodes query `POST /v1/node/state` at startup and every two seconds. This endpoint takes `publicKey`, `signedAt`, `revision`, and `signature`, where the signature is Ed25519 over `model.MachineStateProof.Message()`, including purpose `control-machine-state-v1`. Requests are limited to 4 KiB; timestamps must be within five minutes. The signing identity alone selects the record. The response contains monotonic `revision`, `disabled`, and `unregistered`. No account, directory, work, or foreign identity information is returned. Unlike ordinary endpoints, this read uses proof of the node's private key instead of a bearer credential, so an unregistered node can receive its stop policy after credential revocation. A subsequent signed query acknowledges the revision only after local persistence and admission changes.

Installers send optional `inspectOnly: true`, included in the signed proof, to read retirement status without changing lifecycle support or acknowledgements for the running node. Omitting this field preserves the original proof format. Gateways must support this field before accepting new installers; existing nodes keep using the original format.

Nodes persist policy in `machine-state.json`. Disable rejects new work independently of managed-update reservations while allowing already accepted work to finish, status queries, cancellation, lease cleanup, and idempotent task recovery. Unregister cancels the node context and cleanly stops its supervisor/service, then an independent helper removes the installed startup registration and launcher. The same cleanup runs when startup reads a previously persisted tombstone, without requiring gateway access. It checks the original identity under installation/runtime/node locks, preserves a replacement profile and shared binaries, and retains local data. Tombstones prevent re-registration with the same identity even using another valid key. Offline changes apply when the node can reach the gateway. Old gateways returning 404 for the state endpoint remain compatible with new nodes; local persisted restrictions are retained. Installations made before retirement cleanup support need one current-CLI `service start` to record their launcher and configure Windows self-removal permissions; unmanaged node processes still only stop. See [registration management](installation.md#manage-registrations).

### Node operations

Default remote CLI/MCP operations use an outbound-only account client session, even when a local node is running. `--api` or `CONTROL_API` explicitly selects that node's local API, which can execute locally and discover peers but cannot independently execute peer work. Client sessions use the same fleet checks, pinned TLS, framed requests, and streaming methods without enrolling as machines. Backend selection happens before work and never retries uncertain operations. See [delegation and upgrades](delegation.md).

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
| `activities.pool` | Optional `nodes` array and `recent`; client or local-API aggregation, not a peer RPC; includes offline and unavailable machines |
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
| `http.request` | `url`, optional `method`, `headers`, text `body`, `headerSecrets`; at most 32 header names mapped to `{secret, prefix?}`, worker-local resolution, masked response text, no redirects with references; see [secrets](secrets.md) |
| `files.list` | Optional `path` |
| `files.read` | `path`, optional byte `offset`, `limit` |
| `files.write` | `path`, base64 `data`, optional byte `offset`, `truncate` |
| `workflow.run` | `steps`; see `examples/workflow-task.json` |
| `peers.call` | `target`, `method`, `params`; CLI/MCP first obtains destination-owned delegation for this exact instruction |
| `access.grant` | `subject` worker name/ID, `method`, `params`; task grants require an explicit task ID; optional workflow `inputsFrom` and `deliverTo`; returns an instruction-bound grant with one-hour idle expiry |
| `access.list` | `{}`; original orchestrator owner's grants at this destination |
| `access.revoke` | `id`; owner-only, idempotent when absent; cancels associated tasks and streams |
| `rpa.run` | Opt-in `actions` array, 1..100 GUI actions; serialized per OS user, two-minute invocation limit; see [GUI automation](rpa.md) |

`artifacts.open`, `tcp.open`, and `tcp.listen` are streaming peer methods, not JSON API calls. Use the CLI's `artifact get` and `tunnel` commands, or the local API's `POST /v1/download`, WebSocket `GET /v1/tunnel?target=NODE&address=HOST:PORT`, and reverse WebSocket `GET /v1/listener?target=NODE&listen=HOST:PORT`.

`rpa.run` actions `setValue` and `type` accept `secret: NAME` instead of `text`. References resolve only on the worker; resolved values are never added to task specifications. `control secrets set NAME [--stdin]`, `list`, and `delete NAME` manage the selected profile locally, with no secret management RPC or MCP tool. See [worker-local secrets](secrets.md) for storage, masking, and limitations.

### Clipboard pastes

CLI `control clipboard paste NODE [--reverse] [--dir PATH] [--timeout DURATION]` and MCP `control_clipboard_paste` explicitly transfer the current source clipboard. MCP accepts `node`, optional `reverse`, and optional `dir`. Text goes to the destination OS clipboard; regular files stream into an existing directory, default `.`. Forward destinations are relative to the worker's `workDir`; reverse destinations are local paths. Results contain `kind`, file basenames, and `bytes`, never clipboard text or source paths. CLI timeout defaults to one hour and accepts 1s..24h. See [clipboard pastes](clipboard.md) for desktop dependencies, authorization, and failure handling.

`node.describe` includes `clipboard: {protocol: "clipboard-v1", methods: ["clipboard.open", "clipboard.paste"]}`. These are streaming peer methods, not normal JSON capabilities or durable tasks. Both require the named method's permission and existing fleet membership. `clipboard.open` takes `{protocol: "clipboard-v1"}` and returns `{protocol, content}` where content is `{kind: "text", text}` or `{kind: "files", files: [{name, size}]}`. It waits for a framed `{ready: true}` before sending bytes. The clipboard source paths are read only from the worker's OS clipboard and never accepted from caller arguments.

`clipboard.paste` takes `{protocol: "clipboard-v1", content, dir?}`. After validating content and any file destination, it returns `{protocol: "clipboard-v1"}` before receiving bytes or writing text. File payloads run in manifest order, each with exactly `size` raw bytes followed by 32 raw SHA-256 bytes. Both methods finish with a framed ordinary `result` or `error` response. Text has no streamed payload. Each file is published only after verification, without overwriting an existing entry. There is no resume, automatic retry, source deletion, or native desktop paste interception.

Authenticated local WebSocket `GET /v1/clipboard?target=NODE&method=clipboard.open|clipboard.paste` accepts a framed clipboard request, returns the peer acknowledgement, and relays subsequent bytes unchanged. Empty or self targets use the local node's clipboard service. Other methods are rejected; the route is not a generic RPC proxy. Both methods use the bulk traffic lane and block maintenance while active. Limits are 1 MiB UTF-8 text, 128 regular files, 1 TiB total file bytes, and eight active clipboard streams per receiving node. Portable basenames must be case-insensitively unique. Directories, symlinks, images, and other formats are unsupported.

### Orchestrator sessions and forwards

Persistent host-owned tunnels use `control tunnel start NODE HOST:PORT --listen HOST:PORT [--reverse] [--id ID] [--ttl DURATION]`, `tunnel list`, and `tunnel dispose ID`, with `stop` as a disposal alias. MCP `control_tunnel_start` requires `id`, `node`, `address`, and `listen`, with optional `reverse` and `ttlSeconds`, 0..31536000. `control_tunnel_list` and `control_tunnel_dispose` inspect/remove the same host login's definitions. Start durably accepts a `starting` definition; list reports `starting`, `active`, or `retrying`, active socket counts, last error, pinned `nodeId`, creation, and absolute expiry. Identical ID/specification reconciles without renewing TTL; changed specifications fail. Fixed listen ports are required. Default TTL is zero, meaning until disposed. Definitions survive CLI/MCP exits and restore through a separate user client service; interrupted sockets are never replayed. Existing foreground/process-owned forwards below retain their lifetimes. See [persistent tunnel startup, state, limits, and examples](tunnels.md).

Authorized peer activity snapshots optionally include per-machine `tunnels: {forward, reverse}` for live forward connections and reverse listeners across all owners. Forward counts include `tcp.open` and `tcp.accept`; reverse counts include `tcp.listen`, once per listener rather than per accepted socket. Counts are computed before pool detail truncation. Dashboards derive counts from active records for older nodes without the field, so those fallback counts are limited to returned details. Missing peer observation leaves live counts unknown.

Dashboard JSON separately includes `retainedTunnels: {forward, reverse}` for this host login's retained, unexpired definitions. These counts include retrying definitions and remain available for offline targets. Dashboard polling reads local desired state without starting the service or exposing addresses or credentials. This changes no peer routing envelopes or gateway telemetry.

`control session` and MCP `control_session` describe the requesting process. The result has `role` of `client` or `node-api`, `ownerId`, `transportId`, a peer-ID-to-transport `connections` map, `sessions`, and process-owned `forwards`. Sessions contain `id`, `peer`, `lane`, `mode`, `outgoingStreams`, and `incomingStreams`. Receiver-side sessions use lane `incoming` because their application methods are independently dispatched. A standalone process keeps its owner across invocations but creates a new transport identity each time. A local API uses its node's identity for both. Inspection does not enroll a machine or change task ownership.

| MCP tool | Parameters and result |
| --- | --- |
| `control_forward_start` | Required `node`, destination `address` as `HOST:PORT`, optional `listen`, default `127.0.0.1:0`, optional `reverse`, default false; reverse binds on `node` and dials `address` on the orchestrator; returns forward metadata immediately |
| `control_forward_list` | `{}`; returns this process's forward metadata array |
| `control_forward_stop` | `id`; closes the listener and all active sockets, returns `stopped: true` |
| `control_session` | `{}`; returns the process session metadata |

Forward metadata contains `id`, `node`, `address`, actual bound `listen`, `activeConnections`, optional `reverse: true`, and optional `lastError`. A successful start means the listener is bound, not that the destination service accepted a connection. No dummy destination connection is opened. Ordinary forwards authorize a separate `tcp.open` for each socket; the worker resolves and dials `address`. Reverse forwards authorize `tcp.listen` when creating the remote listener; the client resolves and dials its configured `address` locally for each received socket. Errors close that socket and update `lastError` without replay. Non-loopback listeners are an explicit choice and expose the service to the listener machine's network without additional listener authentication.

Forwards survive the start tool's request context, but belong to that MCP process until stopped or the process exits. The CLI's `control tunnel NODE HOST:PORT [--reverse] [--listen ADDRESS]` uses the same implementation and stays in the foreground until cancelled. Another process cannot list or stop its listeners, but can independently manage tasks sharing the stable owner. At most 32 forwards and 128 active sockets per forward are retained. Each receiving node also caps reverse listeners at 32. Stop and shutdown cancel pending dials, close sockets, and join copy goroutines. Negotiated duplex streams preserve the response direction after write-side EOF. Errors and cancellation abort both directions; older raw endpoints retain full-close behavior. UDP, PTY sessions, and detached local forwarding daemons are not supported. Closing a forward does not stop its destination service or cancel durable tasks.

For a dev server on local port 3000, run `control tunnel worker 127.0.0.1:3000 --reverse --listen 127.0.0.1:3000`, then open `http://localhost:3000` on `worker`. WebSocket upgrades and hot reload use the same TCP forward. Reverse mode requires an upgraded receiving node, and an upgraded local node when using its API. Unsupported receivers fail without fallback. No gateway protocol change is needed.

Peer `tcp.listen` takes `listen`, default `127.0.0.1:0`, and `protocol: "tcp-listen-v1"`. A successful response contains the actual `listen` and matching `protocol`, followed by a nested yamux session. The receiver opens one stream per accepted socket, each carrying version-1 duplex records. The local WebSocket route returns headers `Control-Tunnel-Listen` and `Control-Tunnel-Protocol` and relays these multiplexed bytes unchanged. Listener admission and traffic are tracked on the receiving node and any local relaying node, including idle periods. Stream closure or failure releases the listener and sockets without replay. Peer streams have the existing maximum 24-hour lifetime; restart the forward explicitly after expiry.

New TCP clients request `tcp.open` with `address` and `duplex: 1`. Compatible receivers return `connected: true, duplex: 1`, then exchange four-byte big-endian record lengths and raw payloads of at most 32 KiB. Zero length means write-side EOF; full shutdown still closes the underlying stream. Unknown nonzero versions are rejected. Missing or zero duplex acknowledgement means legacy raw bytes, with no replay or second TCP dial. Local clients request WebSocket `/v1/tunnel?...&duplex=1`; response header `Control-Tunnel-Duplex: 1` negotiates the same records. Each hop negotiates independently.

Streaming log clients send peer `tasks.logs` with `id`, `offset`, and `follow: true`. A compatible receiver checks the existing `tasks.logs` permission and task owner before returning `stream: "task-logs-v1"`. Subsequent length-prefixed JSON records contain base64 `data`, next byte `offset`, and `terminal`, or `error`. Records are limited to 64 KiB of decoded bytes and 128 KiB of framing. Completion is sent only after draining retained bytes. Older receivers return an ordinary snapshot, which the shared client follows with read-only polling. The local API exposes authenticated `GET /v1/logs?target=NODE&id=ID&offset=BYTES` as flushed NDJSON with the same records. A missing older API route permits snapshot polling; permission failures and interrupted streams do not. Standard JSON calls and MCP `control_task_logs` remain snapshot interfaces. Reattach explicitly by offset after a failure; log followers never resubmit or cancel the task.

`control exec NODE [--detach] [--id ID] [--timeout DURATION] [--] COMMAND [ARG...]` submits a durable `exec.run` task. It prints the chosen task ID before submission. Without `--detach` it waits for completion; with it, it returns after acceptance. Timeout defaults to one hour, accepts 1s through 24h, and includes queue/input time. `control task logs NODE ID [--follow] [--offset BYTES]` either returns one structured log chunk or follows text until completion. Following drains remaining terminal chunks. Cancelling a follower or wait never cancels the task; use `control task cancel NODE ID`. Accepted tasks survive client disconnects and remain subject to their existing restart and idempotency rules.

### Instruction-bound delegation

Authenticated gateway metadata advertises `delegation: true`; compatible machines advertise `instructionDelegation: true`. The gateway assigns `executionAuthority: true` only to account-authenticated live clients, never enrolled machines or common-key clients. Default account routing refuses older targets. Worker membership permits only `nodes.list`, `nodes.select`, `node.describe`, `capabilities.list`, and `mcp.discover` without a grant, still subject to `allow`.

Peer requests can carry `delegation` and an outgoing `delegations` array. Destination-owned records bind `id`, `target`, `subject`, `owner`, `method`, and `params`. Task grants also cover status, logs, cancellation, and matching idempotent submission for one task. Workflow `inputsFrom` entries have `node`, `path`, `taskId`, and `artifact` index; `deliverTo` holds authorized recipient IDs. Dynamic artifact grants must sign the expected upstream output and recipient. A caller cannot change a method, arguments, worker, or task by editing the envelope.

Authority contexts survive client disconnects for accepted tasks but are not persisted or exposed as local-worker permissions. Synchronous instructions are single-use. Authorized requests and stream bytes refresh idle expiry; accepted queued/running tasks keep it active. Expiry and revocation close streams and cancel tasks. At most 4,096 records are held per destination, lost on restart. There is no grant-management delegation or nested delegated coordination. See [usage, revocation, and limits](delegation.md).

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
