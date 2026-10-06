# Architecture

[Contributor guide](../AGENTS.md) · [Engineering principles](principles.md) · [Design decisions](decisions.md) · [Protocol reference](protocol.md)

## Roles and components

An orchestrator coordinates work through a CLI or MCP client. A worker is a node executing requested work. The gateway handles enrollment, discovery, signaling, encrypted relay fallback, and fleet administration, not application execution. A node can perform both orchestrator and worker roles. AI agents are optional callers or providers, not Control service types. See [terminology](terminology.md) for naming rules.

```text
Orchestrator's client / CLI
      |
local authenticated API
      |
    Node A -------- WebRTC / TLS / yamux -------- Node B
      |                                            |
      +---- outbound WSS ---- Gateway ---- WSS ----+
                              |
                        identity directory
                        WebRTC signaling
                        encrypted relay
```

The diagram shows a client using a local node. A standalone client connects to the gateway and worker through the same peer transport without enrolling its own machine or running a local node.

## Components

`internal/installation` manages saved host profiles, background startup, and MCP/skill configuration. Windows invitation scripts default to `user` mode, using a login scheduled task with the installing user's limited interactive token and the update supervisor, without storing an account password. Scripts pass the selected mode explicitly to enrollment, overriding saved startup unless `CONTROL_SERVICE_MODE` supplies another selection. Direct `control setup` defaults to an automatic SCM service under LocalService, with a native service handler around the supervisor. Stop and shutdown controls cancel the supervisor and wait for its child to exit. Windows persists the selected startup mode beside the profile. Linux and macOS use per-user service managers. `internal/enrollment` defines invitation messages and platform scripts. The gateway persists hashed, expiring tickets and binds each redeemed credential to its signed machine identity. See [installation and enrollment](installation.md) for the setup flow.

OS-only invitations pin both architecture assets from a single deployment manifest. The target script detects the native architecture and verifies the selected binary's checksum. Signed redemption fixes that architecture in the persisted registration credential. The dashboard creates invitations through the shared client and delegates local clipboard access to platform-specific helpers in `internal/clipboard`.

Automatic-name invitations defer naming until the target installer reads its OS hostname. The installer persists the resolved name locally and includes it in its signed redemption proof. Under the gateway lock, SQLite commits the name claim, identity ownership, and credential binding together, rejecting occupied or reserved names within that fleet. Fixed-name invitations still reserve names before installation. See [D034](decisions.md#d034-target-hostname-for-automatic-installation-names).

Re-enrollment replaces the selected local profile's registration using its existing identity. The installer persists a pending replacement profile, stops the old service, redeems the new invitation, then writes the profile and restarts. SQLite commits the directory rename and invitation-credential rotation together. Same-name invitations reserve the existing identity; new-name invitations can rename a signing identity only within its immutable fleet. See [D026](decisions.md#d026-replace-enrollment-within-the-existing-machine-profile).

Unregistration also accepts connected nodes without lifecycle support. The gateway removes their registrations and closes their connections; local shutdown still requires node support or a local stop. Installers inspect policy without acknowledging it or claiming runtime support. When replacing a retired profile, they persist a new identity in a separate state directory and preserve the old files. See [D027](decisions.md#d027-unregister-older-agents-and-reinstall-retired-profiles).

`control login` validates and persists a gateway credential independently of node setup. Directory and fleet-management commands use that saved login directly. Setup reuses the account key to issue a separate node key. Execution uses the local node API when available, or a command-scoped peer when no local API is listening. Pool activity aggregation still uses the local node.

Fleet owners manage machine admission through gateway policy. SQLite stores monotonic disabled/unregistered state per immutable identity. Nodes read only their own policy using short-lived Ed25519 proofs, persist it locally, and acknowledge its revision. This channel survives revocation of installation credentials so an unregistered node can still receive its stop instruction. Disabled admission is separate from updater reservations, and both must permit new execution. Directory selection excludes disabled and policy-pending machines. Unregistration tombstones prevent an old process from recreating a removed registration.

`cmd/control` provides gateway, node, CLI, MCP, and tunnel entry points. `internal/client` is shared by CLI and MCP. A persistent node owns the connection and identity so concurrent local tools do not re-enroll or compete for the same connection. Without one, the client probes the local API with `node.describe` before submitting work. Only connection refusal enables standalone routing; explicit API configuration, authentication failures, timeouts, and application errors never trigger fallback. The chosen backend remains fixed for the process lifetime.

Standalone routing uses `internal/transport` directly, with the same WebRTC/relay, pinned TLS, account checks, and framed RPCs as nodes. `internal/wire` owns the shared JSON frame format. The client authenticates its account before loading its gateway/account-scoped owner identity under `clients` in the Control configuration directory. A short file lock protects key creation only. Each process generates a separate TLS identity and proves that its persistent owner authorized that connection. Tasks and leases use the stable owner, while TLS and artifact-grant recipients use the live transport identity. Concurrent processes can monitor or cancel work without displacing a long-lived MCP or tunnel connection.

Client sessions use `/v1/client/connect` with a role-bound challenge signature, not machine enrollment. The gateway persists immutable account ownership, client role, and transport-to-owner bindings in SQLite before acknowledging the connection, but stores routing metadata only in memory. `/v1/nodes`, gateway status, worker selection, and managed rollout operate exclusively on enrolled machines. Nodes authenticate a client's transport identity and stable owner through an account-scoped `/v1/peers/{id}` lookup, available only while that client is connected. The lookup never grants task control or bypasses receiver access rules. Client metadata disappears on disconnect; there is no offline machine entry. Clients have no execution API, providers, health reporter, lifecycle policy, or update handler. Gateway and worker support are negotiated before standalone work is sent. See [D029](decisions.md#d029-authenticated-client-sessions-are-not-fleet-machines), [D030](decisions.md#d030-concurrent-client-connections-with-stable-task-owners), and [standalone usage](installation.md#standalone-cli-and-mcp).

`internal/client` owns process-local port forwards for both CLI and MCP. Each listener creates independent `tcp.open` streams through the selected backend, with bounded active connections. MCP start calls return immediately; the listener lifetime follows the MCP process rather than the tool request. Stop and shutdown close listeners and sockets and join their copy goroutines. Existing sockets are never replayed after a transport failure. Log followers poll owner-scoped task logs by offset; cancelling a follower does not cancel accepted work.

`internal/gateway` authenticates enrollment with a user-scoped credential and an Ed25519 challenge response. SQLite stores accounts, credential hashes, invitations, permanent identity ownership, and the enrolled directory. Names are unique within each user's fleet. Directory queries, offline events, signaling, and relay packets are scoped to that fleet. The gateway is the trusted naming and membership authority. See [users and private fleets](users.md).

`internal/transport` establishes ordered byte streams over Pion WebRTC or the gateway WebSocket relay. It obtains the node's fleet from authenticated gateway metadata and checks incoming and outgoing peers against the scoped directory. Each stream carries mutually authenticated TLS 1.3 pinned to the enrolled key. Yamux multiplexes independent RPC, artifact, and TCP streams over that session. Established direct sessions can continue without the gateway; immutable identity ownership prevents them from becoming cross-user sessions. New peer discovery and signaling require the gateway.

Outgoing control, bulk, and interactive lanes use separate TLS/yamux sessions. Compatible peers negotiate `peerChannels` and share one WebRTC ICE/DTLS/SCTP carrier across reliable ordered data channels. Each additional channel is independently pinned to the same peer and holds reference-counted carrier ownership. Older receivers use separate connections, while relay lanes share the gateway socket. Session setup coalesces by peer/lane rather than holding a process-wide dial lock. Setup and stream admission are bounded, with cancellation-aware waits and shutdown ownership. Session inspection exposes lane and active-stream counts. See [fleet streaming](streaming.md) for scenarios, limits, and compatibility.

`internal/node` owns capabilities, local MCP sessions, subprocesses, tasks, artifacts, workflow execution, and access checks. Providers know nothing about the connection transport.

`internal/system` samples and caches host resources. Nodes collect on startup, periodically, on heavy-task completion, or on request. The gateway collects on startup and every 15 seconds, and stops its collector on shutdown. `internal/dashboard` renders a scrollable terminal view using Bubble Tea. Its shared client reads `/v1/status` directly from the gateway for service identity, cached gateway metrics, and the authenticated user's directory. Every five seconds nodes send aggregate work/lease state and cached resources over their authenticated gateway connection. The gateway retains one report per connection for up to 20 seconds and exposes it only to the fleet owner. An available local node enriches directory entries with bounded, authorized peer activity snapshots. Dashboard polls and health reports read resource caches rather than collecting metrics themselves. See [dashboard and system metrics](dashboard.md).

The dashboard handles left-button drag selection through terminal mouse reporting. It captures the visible rendered screen on press so polling cannot change the selected text during a drag. Release resumes the live display and submits only the selected plain text to the local clipboard through a bounded command. Clipboard operations are serialized; resizing or keyboard input cancels an unfinished drag. This does not change fleet observation or expose hidden snapshot fields.

`internal/update` validates and stages executable bundles, persists deployment state, and handles node update commands. The gateway coordinates an idle rollout over its existing authenticated connections. `internal/workgate` makes work admission and maintenance reservation mutually exclusive. A built-in service supervisor selects versioned executables from the state directory and preserves the outer process across updates. See [managed updates](updates.md).

Development updates run `update authorize --check` before bundle builds, verifying selected credentials without prompting. Superuser login validates and saves update authorization through `internal/installation` in `update-admin.json`; a later normal login does not overwrite it. Explicit `update authorize` can also obtain and save the key. This gateway-scoped credential is used only by update commands, independently of the regular `admin.json` fleet login. Fleet observation, execution, and general CLI help retain the normal account's authority. See [D035](decisions.md#d035-separate-saved-authorization-for-development-updates) and [D036](decisions.md#d036-persist-operator-login-and-do-not-prompt-during-updates).

`control update`, also available as `control upgrade`, uses `internal/installation` to replace the invoked CLI from a public GitHub release without gateway authentication. It reuses manifest and executable validation from `internal/update`, serializes replacement with a local file lock, and preserves executable permissions. Windows renames a running executable aside before replacement. This does not change the supervisor's persisted runtime selection.

## Connection lifecycle

1. A node loads or generates its Ed25519 key in its data directory.
2. It opens an outbound gateway WebSocket, answers a fresh challenge, and registers its metadata.
3. A caller resolves a destination name or ID through the directory.
4. It tries a reliable ordered WebRTC data channel. Configure ICE servers for STUN/TURN as required by the deployment.
5. If direct establishment fails or times out, it opens an opaque relayed stream over the existing gateway connections.
6. Both ends authenticate with TLS, then open a multiplexed session.
7. Broken sessions are discarded. A later operation creates a new session. It does not automatically replay the previous operation.

Gateway connections reconnect automatically after loss. WebSocket pings detect stale connections. Relay backlogs and session queues are bounded; a stalled connection is closed rather than blocking unrelated peers. WebRTC uses data-channel buffered-amount backpressure.

Gateway packets are generated Protobuf messages in `internal/protocol/control.proto`. Inside the encrypted session, a request has a four-byte big-endian length and a JSON object containing `version`, `method`, `params`, and an optional `deadline`. Its response uses the same framing with `result` or `error`. Artifact and TCP methods continue with raw streamed bytes. JSON keeps dynamic capability arguments and MCP schemas independent of generated code.

## Identity and authority

Each user owns a private fleet. User account keys manage only that fleet's enrollment invitations, common keys, and registrations. Common keys belong to one user and permit enrollment and discovery only in that fleet. The optional bootstrap token belongs to the legacy operator fleet. Each node's configured token also authenticates its loopback API, which acts with that node's full authority. Keep that API on loopback. A separate superuser key authorizes user provisioning and gateway-wide executable updates.

Nodes trust enrolled peers in their own fleet by default. Configure `allow` to restrict incoming methods and capabilities by stable node ID or name. Fleet membership is checked before these rules and before artifact grants, so neither wildcard rules nor grants authorize cross-fleet connections. IDs remain stable across renames and are preferred for access rules. Commands run with the node user's normal OS privileges; the node is not an execution sandbox.

A peer connection proves possession of the enrolled key. The gateway can observe routing, names, labels, and advertised capabilities, but execution payloads in the relay are TLS encrypted. HTTPS/WSS protects enrollment and the naming directory when the gateway is on the internet.

Artifact grants are signed by the artifact-owning node. A grant names one artifact, one authenticated recipient, and an expiry. It permits downloading that artifact even when the recipient otherwise lacks `artifacts.open` permission. `artifacts.deliver` creates this grant and asks the recipient to pull the bytes directly. General capability delegation is expressed through node access rules, rather than transferable bearer authority.

Workflow and agent providers run under the executor node's authority. Grant access to those capabilities only to callers intended to use that authority. AI CLIs do not automatically receive a dynamically scoped network credential.

## Tasks and recovery

A task is persisted before acceptance is returned. The tuple of task ID, owner, and normalized specification prevents duplicate execution on repeated submissions. Task timeouts include queue and input-transfer time. Task workspaces live at `workDir/tasks/TASK_ID`.

The requesting connection can disappear after acceptance. The worker continues and retains status, bounded logs, result, and artifact references. Query the task ID after reconnecting. Log followers capture an append/completion wakeup channel before reading retained bytes, avoiding lost wakeups and per-follower polling on compatible nodes. Slow readers consume bounded chunks from disk without subscriber queues. Commands and external services cannot promise exactly-once effects, so incomplete tasks become `interrupted` on node restart and are never silently re-executed.

The workflow provider validates its dependency graph before submission, then executes steps sequentially in dependency order. Inputs can refer to earlier steps' artifacts. A workflow submitted as a task continues after the local CLI disconnects, as long as its hosting node stays running. It does not resume unfinished workflows after a node restart. Child task IDs are written to the workflow log for reconciliation.

Exclusive leases coordinate tracked execution on one node. They are acquired atomically on that node, persisted, and checked at task acceptance. Expiry does not allow a new lease to overlap an existing leased task that is still running. Leases reserve execution capacity; they do not revoke filesystem artifacts or TCP connections.

## Extension boundaries

A Go `Provider` exposes a capability description/schema and a context-aware `Run` method. Configured subprocess providers implement the same behavior using versioned JSON stdin/stdout. Files and large outputs should be artifacts, rather than embedded control messages.

The agent provider wraps an installed CLI, while the MCP provider maintains local sessions through the official Go MCP SDK. Application-specific features, GPU schedulers, desktop helpers, and richer interactive agent adapters can be added as providers without changing the peer transport.

An explicitly configured `rpa` helper registers `rpa.run`. The Go adapter validates batches, serializes them with a per-OS-user file lock, caps execution at two minutes, and imports bounded PNG screenshots as artifacts. The supplied Python helper uses Windows UI Automation, macOS AX, or Linux AT-SPI for exact element targeting, with explicitly requested PyAutoGUI input as a fallback. GUI execution retains existing task durability, authorization, lease checks, and maintenance admission. See [GUI automation](rpa.md).
