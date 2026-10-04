# Architecture

[Contributor guide](../AGENTS.md) · [Engineering principles](principles.md) · [Design decisions](decisions.md) · [Protocol reference](protocol.md)

```text
AI client / CLI
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

## Components

`internal/installation` manages saved host profiles, user startup, and MCP/skill configuration. `internal/enrollment` defines invitation messages and platform scripts. The gateway persists hashed, expiring tickets and binds each redeemed credential to its signed machine identity. See [installation and enrollment](installation.md) for the setup flow.

`cmd/control` provides gateway, node, CLI, MCP, and tunnel entry points. `internal/client` is shared by CLI and MCP. A persistent node owns the connection and identity so concurrent local tools do not re-enroll or compete for the same connection.

`internal/gateway` authenticates enrollment with a user-scoped credential and an Ed25519 challenge response. SQLite stores accounts, credential hashes, invitations, permanent identity ownership, and the enrolled directory. Names are unique within each user's fleet. Directory queries, offline events, signaling, and relay packets are scoped to that fleet. The gateway is the trusted naming and membership authority. See [users and private fleets](users.md).

`internal/transport` establishes ordered byte streams over Pion WebRTC or the gateway WebSocket relay. It obtains the node's fleet from authenticated gateway metadata and checks incoming and outgoing peers against the scoped directory. Each stream carries mutually authenticated TLS 1.3 pinned to the enrolled key. Yamux multiplexes independent RPC, artifact, and TCP streams over that session. Established direct sessions can continue without the gateway; immutable identity ownership prevents them from becoming cross-user sessions. New peer discovery and signaling require the gateway.

`internal/node` owns capabilities, local MCP sessions, subprocesses, tasks, artifacts, workflow execution, and access checks. Providers know nothing about the connection transport.

`internal/system` samples and caches host resources on startup, periodically, on heavy-task completion, or on request. `internal/dashboard` renders a scrollable terminal view using Bubble Tea. Its client fetches bounded pool snapshots from the local node, including every registered machine's availability and authorized node-wide activity metadata. Dashboard polls read the resource cache rather than collecting metrics themselves. See [dashboard and system metrics](dashboard.md).

`internal/update` validates and stages executable bundles, persists deployment state, and handles node update commands. The gateway coordinates an idle rollout over its existing authenticated connections. `internal/workgate` makes work admission and maintenance reservation mutually exclusive. A built-in service supervisor selects versioned executables from the state directory and preserves the outer process across updates. See [managed updates](updates.md).

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

The requesting connection can disappear after acceptance. The worker continues and retains status, bounded logs, result, and artifact references. Query the task ID after reconnecting. Commands and external services cannot promise exactly-once effects, so incomplete tasks become `interrupted` on node restart and are never silently re-executed.

The workflow provider validates its dependency graph before submission, then executes steps sequentially in dependency order. Inputs can refer to earlier steps' artifacts. A workflow submitted as a task continues after the local CLI disconnects, as long as its hosting node stays running. It does not resume unfinished workflows after a node restart. Child task IDs are written to the workflow log for reconciliation.

Exclusive leases coordinate tracked execution on one node. They are acquired atomically on that node, persisted, and checked at task acceptance. Expiry does not allow a new lease to overlap an existing leased task that is still running. Leases reserve execution capacity; they do not revoke filesystem artifacts or TCP connections.

## Extension boundaries

A Go `Provider` exposes a capability description/schema and a context-aware `Run` method. Configured subprocess providers implement the same behavior using versioned JSON stdin/stdout. Files and large outputs should be artifacts, rather than embedded control messages.

The agent provider wraps an installed CLI, while the MCP provider maintains local sessions through the official Go MCP SDK. Application-specific features, GPU schedulers, desktop helpers, and richer interactive agent adapters can be added as providers without changing the peer transport.
