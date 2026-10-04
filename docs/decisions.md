# Design decisions

[Contributor guide](../AGENTS.md) · [Architecture](architecture.md) · [Engineering principles](principles.md) · [Protocol reference](protocol.md)

This record captures choices in the current implementation. All decisions below are accepted. New decisions should use the next number, state their rationale and consequences, and link any decision they supersede.

## D001: One symmetric node runtime

Use one Go node runtime on Windows, Linux, and macOS. Requester, worker, proxy, and orchestrator are roles within an operation.

This supports worker-to-worker requests and transfers without introducing separate agent and worker protocols. AI reasoning remains optional. OS-specific process handling lives in platform-specific files, while capability and transport contracts remain shared.

## D002: Persistent local node with thin CLI and MCP clients

CLI and AI-facing MCP processes use an authenticated local HTTP API on a persistent node. Their shared implementation lives in `internal/client`.

The node owns its identity, gateway connection, peer sessions, and accepted work. Multiple local tools can operate concurrently without competing to register the same machine. Users must start the local node before using these clients. The local API acts with that node's authority and defaults to loopback.

## D003: Outbound gateway connections, WebRTC, and a WebSocket relay

Nodes maintain outbound gateway connections for registration and signaling. Peer session establishment tries reliable ordered WebRTC data channels, then falls back to relayed WebSocket traffic.

WebRTC allows direct transfers across networks where ICE succeeds. The relay supports networks that prevent direct connectivity. STUN and TURN are deployment configuration; an application-level relay remains available independently of TURN.

Both paths carry the same application protocol. Existing direct sessions can continue during a gateway outage, but new discovery and signaling need the gateway. Re-establishing a transport does not replay an application request.

## D004: Enrolled Ed25519 identities and pinned TLS sessions

Each node persists an Ed25519 key. The gateway checks a fresh signed challenge during registration and serves as the trusted naming directory. Peer sessions use mutually authenticated TLS 1.3 pinned to enrolled public-key fingerprints, over either transport.

This gives execution traffic the same authentication and encryption on direct and relayed paths. The gateway still sees routing and registration metadata. HTTPS/WSS protects enrollment and directory access over the internet.

The initial trust model used one pool with a shared enrollment token and optional per-node access rules. D014 superseded the single-token assumption. [D016](#d016-sqlite-backed-users-and-isolated-fleets) supersedes the shared-pool assumption with user-owned fleets and immutable identity ownership. Stable IDs are preferable to names in access rules. Execution sandboxing remains outside the node's contract.

## D005: Protobuf routing envelopes, JSON operations, and streamed bytes

Use a generated Protobuf envelope for gateway routing and signaling. Inside peer TLS sessions, yamux carries independent streams. Operations use length-prefixed JSON requests and responses; artifact and TCP streams continue with raw bytes.

JSON supports dynamic provider arguments and MCP schemas without regenerating the entire protocol for each integration. Protobuf gives the gateway a small typed envelope. Length prefixes keep control decoding from consuming subsequent streamed data.

There are two encoding layers to maintain. Changes to the routing envelope require regenerating `control.pb.go`; changes to operation contracts require updating their schemas and the protocol reference.

## D006: Providers are the application extension point

Built-in Go providers, configured subprocess providers, installed AI CLIs, and attached MCP servers run behind node capability dispatch. External providers use versioned JSON stdin/stdout rather than Go's native plugin mechanism.

Subprocesses allow integrations in other languages and avoid platform-specific Go plugin loading. Their tradeoffs include process startup cost and explicit lifecycle management. Stdout carries the structured result; stderr carries diagnostics. Large outputs belong in artifacts.

The MCP adapter uses the official Go SDK and maintains sessions across calls. It supports the documented tool, resource, and prompt operations. This choice does not imply support for every MCP callback or interactive agent protocol.

## D007: Durable acceptance with conservative restart recovery

Persist a task before returning acceptance. Bind its submission ID to the owner and normalized specification. Repeated matching submissions return the existing task, while conflicting reuse fails.

Accepted tasks run independently of the submitting connection. A node restart marks unfinished tasks `interrupted` rather than replaying them. This prevents automatic duplication of external effects whose completion cannot be determined safely.

Callers reconcile uncertain submissions by task ID. Automatic process checkpointing and exactly-once external execution are outside this contract.

## D008: Immutable artifacts and recipient-initiated transfers

Artifacts use SHA-256 content identities. A destination pulls from the source using a resumable offset, then checks the complete checksum. Delivery asks the destination to pull; it does not route bytes through the orchestrator.

The artifact owner can issue a signed grant naming the recipient, artifact, and expiry. This permits a specific transfer without granting general filesystem or execution access.

Artifacts and partial transfers occupy local disk until completion or cleanup. Retention is currently explicit; there is no automatic eviction or disk quota policy.

## D009: Single-instance metadata in locked local directories

Persist identities, enrollment metadata, tasks, leases, and artifact metadata in local files. Use directory locks to prevent concurrent owners and atomic replacement for JSON records. Keep the build free of CGO.

The gateway enrollment/credential persistence portion is superseded by [D016](#d016-sqlite-backed-users-and-isolated-fleets). Node-owned tasks, leases, artifacts, and executable rollout files retain this design.

This supports the initial single-gateway deployment without an external database. It makes metadata inspectable and keeps installation small. It also means the gateway does not support shared-state horizontal scaling, and state migration or coordinated database access needs a future decision.

## D010: Destination-owned leases and a small workflow executor

Machine selection returns an online label/capability match. Exclusive execution leases are acquired atomically on the selected node, persisted there, and checked when tasks are accepted. Selection itself is advisory.

A lease cannot be released while its tasks remain active. Expiry prevents new submissions under that lease, but does not let another owner overlap a still-running task. Leases reserve execution capacity rather than revoking existing artifact or TCP access.

The workflow provider validates a dependency graph and runs steps sequentially in dependency order. Coordination does not consume a worker slot, so a workflow can submit a local step even with one worker slot. A workflow can outlive its requesting CLI when submitted as a task, but does not automatically resume after its hosting node restarts. Parallel DAG scheduling and automatic workflow recovery require additional lifecycle rules.

## D011: Compose for cross-container integration tests

Use Docker Compose to run a gateway, three nodes with separate storage, and a test runner on an internal network. Install FFmpeg in the node test image and run the same cross-container suite with direct WebRTC required and with relay-only connections required.

This verifies peer routing and artifact movement between distinct processes and network addresses, using the shipped CLI and APIs. Separate volumes prevent a shared filesystem from masking transfer bugs. Health checks gate startup, and the test script propagates failures and cleans up each deployment.

Application-dependent tests use the `compose` build tag. The core Go suite remains runnable without Docker or FFmpeg. Compose runs Linux containers, including on Docker Desktop, so native Windows and macOS CI tests remain necessary. See [Docker Compose testing](testing.md) for commands and coverage.

## D012: Node-owned activity snapshots and a local CLI dashboard

Track tasks, synchronous operations, transfers, and tunnels on the node performing them. Expose read-only node-wide metadata through `activities.list`, independently of owner-scoped task control and results. Aggregate through a local-only `activities.pool` request using the orchestrator node's identity and bounded concurrent peer calls.

This makes delegated and peer-to-peer work visible without sending execution telemetry through the gateway or requiring every task to originate on the observer. Offline directory entries remain visible, while denied or failed snapshots are labeled unavailable. A failed pool refresh leaves the previous terminal view marked stale.

Use Bubble Tea for terminal lifecycle, keyboard input, resize handling, and scrolling. Support one-shot text and JSON for non-interactive use. Poll snapshots rather than introducing an event broker. In-memory recent completions make short operations visible, but do not provide a durable audit trail.

## D013: Sample host resources separately from activity polling

Use a cross-platform, CGO-free system collector with a cached snapshot. Collect on startup, every 15 seconds by default, after execution/agent tasks or tasks lasting over a second complete, and on explicit request. Allow the periodic interval to be changed or disabled. Coalesce completion triggers and serialize collection.

This keeps dashboard refreshes from repeatedly querying CPU, memory, and disk APIs. Every snapshot includes its sampling time and collection errors. CPU utilization uses deltas between samples, so the first sample establishes a baseline. Registration includes the initial system snapshot; current readings remain node-owned and are fetched when requested.

These metrics describe host-visible capacity and usage. They do not implement task accounting, container quotas, or load-aware scheduling. See [dashboard and system metrics](dashboard.md) for configuration and observation permissions.

## D014: Superuser-authorized gateway updates and idle reservations

Extend D004's enrollment model with hashed, revocable common keys and a separately configured superuser key. Only the superuser can issue common keys, upload executable bundles, publish deployments, or trigger release checks. CLI administration help requires a verified superuser response; server-side authorization enforces the permission independently.

[D016](#d016-sqlite-backed-users-and-isolated-fleets) supersedes the common-key issuance restriction: account owners can issue keys for their own fleet. Executable publication and global rollout administration remain superuser-only.

The gateway can fetch stable GitHub releases from a configured repository or accept development bundles built by `make update`. Each manifest binds platform-specific executables to size and SHA-256. Nodes accept update commands only from the gateway, stage and validate their own binary, and acknowledge a local idle reservation before applying it. Track outgoing work, accepted task lifetimes, transfers, tunnels, and leases so coordination and peer-to-peer traffic also delay maintenance.

Persist installation participants and reservations across service restarts. Update nodes first and wait for their version acknowledgments, then restart the gateway. Expiring reservations release admissions if coordination is lost. Offline nodes catch up on reconnection. This introduces short maintenance pauses and lets a busy pool postpone an update indefinitely.

Use a stable supervisor and versioned executables in the state directory rather than replacing running Windows binaries. The original binary remains the launcher, and standalone CLI/MCP installations retain their own version. Selection is durable, shutdown is graceful, and the previous selection is retained for manual recovery. Startup failure does not automatically replay work or roll back state. See [managed updates](updates.md).

## D015: Single-machine invitations and per-user host setup

Extend D004 and D014 with superuser-created installation invitations. A hashed, expiring ticket reserves a name and pins a platform binary. The target generates its private identity and credential locally, then signs a redemption that binds the credential hash to its identity, name, OS, and architecture. Downloading the script does not consume the ticket. Atomic redemption permits one identity, with repeat requests from the same persisted identity and credential to recover a lost response.

[D016](#d016-sqlite-backed-users-and-isolated-fleets) supersedes the invitation permission and global name reservation: owners create invitations within their fleet, and SQLite persists ownership and redemption.

This avoids distributing a shared pool token in installation scripts. Revocation blocks future authentication and disconnects gateway sessions, but does not terminate established direct peer connections. Forgotten offline registrations retain their local state. Hash-only ticket storage means the gateway cannot reconstruct a lost installation URL.

Use per-user startup through systemd, launchd, or Windows login registration, with a reported process-only fallback where user startup is unavailable. Wait for local identity and online directory checks before reporting successful setup. Keep CLI/MCP credentials in one saved profile. Store host administration separately and configure the host to deny remote execution and file/network access under its administrative account. See [installation and enrollment](installation.md).

## D016: SQLite-backed users and isolated fleets

Each gateway user owns a private fleet. The operator provisions user accounts with a high-entropy key; account owners issue common keys and installation invitations for their fleet. Credentials determine user ownership. User-supplied fields cannot select another fleet. Gateway-wide software publication remains an operator responsibility.

Scope discovery, offline events, name uniqueness, enrollment management, and every signaling/relay packet by user. Verify scoped directory membership before peer setup and node dispatch, including streaming artifact grants. Use the existing pinned mutual TLS on both transports. Keep permanent identity ownership after forgetting a node so existing direct sessions cannot cross a later ownership change. Account/key revocation prevents future gateway authentication but does not terminate accepted work or existing same-fleet direct sessions.

Persist accounts, credential hashes, invitations, registrations, and identity ownership in SQLite with foreign keys, per-user name uniqueness, WAL, and full synchronous commits. Use the pure-Go driver to retain all supported CGO-free builds. Protected in-memory maps cache the database; commit mutations before acknowledging them. Preserve the existing filesystem storage for node-owned work and global executable assets/rollout state.

Import existing JSON registration state into one legacy fleet transactionally, retaining the source files without reading them again after migration. There is no evidence for assigning old shared-pool nodes to separate people, so migrating them into new accounts requires fresh identities. Upgrade the gateway before new nodes because peers now require fleet-aware authentication. This decision supersedes the shared-pool and gateway JSON assumptions in D004/D009 and the fleet-management permissions in D014/D015. See [users and private fleets](users.md).
