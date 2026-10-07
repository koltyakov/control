# Design decisions

[Contributor guide](../AGENTS.md) · [Architecture](architecture.md) · [Engineering principles](principles.md) · [Protocol reference](protocol.md)

This record captures choices in the current implementation. All decisions below are accepted. New decisions should use the next number, state their rationale and consequences, and link any decision they supersede.

Use the current [terminology](terminology.md) when reading this record. Older entries sometimes use agent for the node service; this does not identify a separate runtime type.

## D001: One symmetric node runtime

Unrestricted peer initiation is superseded by [D045](#d045-account-client-authority-and-instruction-bound-worker-delegation). The symmetric runtime remains unchanged.

Use one Go node runtime on Windows, Linux, and macOS. Requester, worker, proxy, and orchestrator are roles within an operation.

This supports worker-to-worker requests and transfers without introducing separate agent and worker protocols. AI reasoning remains optional. OS-specific process handling lives in platform-specific files, while capability and transport contracts remain shared.

## D002: Persistent local node with thin CLI and MCP clients

Default remote routing and pool aggregation through the local node's identity are superseded by [D045](#d045-account-client-authority-and-instruction-bound-worker-delegation).

The local-node requirement for gateway observation is superseded by [D018](#d018-direct-gateway-observation-with-optional-peer-activity). [D028](#d028-command-scoped-outbound-peers-for-cli-and-mcp) supersedes the local-node requirement for remote execution.

CLI and AI-facing MCP processes use an authenticated local HTTP API on a persistent node. Their shared implementation lives in `internal/client`.

The node owns its identity, gateway connection, peer sessions, and accepted work. Multiple local tools can operate concurrently without competing to register the same machine. Users must start the local node before using these clients. The local API acts with that node's authority and defaults to loopback.

## D003: Outbound gateway connections, WebRTC, and a WebSocket relay

Nodes maintain outbound gateway connections for registration and signaling. Peer session establishment tries reliable ordered WebRTC data channels, then falls back to relayed WebSocket traffic.

WebRTC allows direct transfers across networks where ICE succeeds. The relay supports networks that prevent direct connectivity. STUN and TURN are deployment configuration; an application-level relay remains available independently of TURN.

Both paths carry the same application protocol. Existing direct sessions can continue during a gateway outage, but new discovery and signaling need the gateway. Re-establishing a transport does not replay an application request.

## D004: Enrolled Ed25519 identities and pinned TLS sessions

Fleet membership alone no longer permits execution. [D045](#d045-account-client-authority-and-instruction-bound-worker-delegation) adds account-client authority and destination-owned grants without changing identity or encryption.

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

[D045](#d045-account-client-authority-and-instruction-bound-worker-delegation) adds revocable destination-owned access to signed artifact grants. Content identity and direct transfer remain unchanged.

Artifacts use SHA-256 content identities. A destination pulls from the source using a resumable offset, then checks the complete checksum. Delivery asks the destination to pull; it does not route bytes through the orchestrator.

The artifact owner can issue a signed grant naming the recipient, artifact, and expiry. This permits a specific transfer without granting general filesystem or execution access.

Artifacts and partial transfers occupy local disk until completion or cleanup. Retention is currently explicit; there is no automatic eviction or disk quota policy. [D059](#d059-artifact-owner-holds) scopes listing and deletion to the owners holding the content.

## D009: Single-instance metadata in locked local directories

Persist identities, enrollment metadata, tasks, leases, and artifact metadata in local files. Use directory locks to prevent concurrent owners and atomic replacement for JSON records. Keep the build free of CGO.

The gateway enrollment/credential persistence portion is superseded by [D016](#d016-sqlite-backed-users-and-isolated-fleets). Node-owned tasks, leases, artifacts, and executable rollout files retain this design.

This supports the initial single-gateway deployment without an external database. It makes metadata inspectable and keeps installation small. It also means the gateway does not support shared-state horizontal scaling, and state migration or coordinated database access needs a future decision.

## D010: Destination-owned leases and a small workflow executor

Sequential-only workflow execution and first-match selection are superseded by [D060](#d060-bounded-parallel-workflows-and-load-preferring-selection). Leases are unchanged.

Machine selection returns an online label/capability match. Exclusive execution leases are acquired atomically on the selected node, persisted there, and checked when tasks are accepted. Selection itself is advisory.

A lease cannot be released while its tasks remain active. Expiry prevents new submissions under that lease, but does not let another owner overlap a still-running task. Leases reserve execution capacity rather than revoking existing artifact or TCP access.

The workflow provider validates a dependency graph and runs steps sequentially in dependency order. Coordination does not consume a worker slot, so a workflow can submit a local step even with one worker slot. A workflow can outlive its requesting CLI when submitted as a task, but does not automatically resume after its hosting node restarts. Parallel DAG scheduling and automatic workflow recovery require additional lifecycle rules.

## D011: Compose for cross-container integration tests

Use Docker Compose to run a gateway, three nodes with separate storage, and a test runner on an internal network. Install FFmpeg in the node test image and run the same cross-container suite with direct WebRTC required and with relay-only connections required.

This verifies peer routing and artifact movement between distinct processes and network addresses, using the shipped CLI and APIs. Separate volumes prevent a shared filesystem from masking transfer bugs. Health checks gate startup, and the test script propagates failures and cleans up each deployment.

Application-dependent tests use the `compose` build tag. The core Go suite remains runnable without Docker or FFmpeg. Compose runs Linux containers, including on Docker Desktop, so native Windows and macOS CI tests remain necessary. See [Docker Compose testing](testing.md) for commands and coverage.

## D012: Node-owned activity snapshots and a local CLI dashboard

The dashboard's gateway connection and directory fallback are superseded by [D018](#d018-direct-gateway-observation-with-optional-peer-activity). Node-owned activity and authorization remain unchanged.

Track tasks, synchronous operations, transfers, and tunnels on the node performing them. Expose read-only node-wide metadata through `activities.list`, independently of owner-scoped task control and results. Aggregate through a local-only `activities.pool` request using the orchestrator node's identity and bounded concurrent peer calls.

This makes delegated and peer-to-peer work visible without sending execution telemetry through the gateway or requiring every task to originate on the observer. Offline directory entries remain visible, while denied or failed snapshots are labeled unavailable. A failed pool refresh leaves the previous terminal view marked stale.

Use Bubble Tea for terminal lifecycle, keyboard input, resize handling, and scrolling. Support one-shot text and JSON for non-interactive use. Poll snapshots rather than introducing an event broker. In-memory recent completions make short operations visible, but do not provide a durable audit trail.

## D013: Sample host resources separately from activity polling

Use a cross-platform, CGO-free system collector with a cached snapshot. Collect on startup, every 15 seconds by default, after execution/agent tasks or tasks lasting over a second complete, and on explicit request. Allow the periodic interval to be changed or disabled. Coalesce completion triggers and serialize collection.

This keeps dashboard refreshes from repeatedly querying CPU, memory, and disk APIs. Every snapshot includes its sampling time and collection errors. CPU utilization uses deltas between samples, so the first sample establishes a baseline. Registration includes the initial system snapshot; current readings remain node-owned and are fetched when requested.

These metrics describe host-visible capacity and usage. They do not implement task accounting, container quotas, or load-aware scheduling. See [dashboard and system metrics](dashboard.md) for configuration and observation permissions.

## D014: Superuser-authorized gateway updates and idle reservations

Same-version update selection and acknowledgements are refined by [D047](#d047-skip-updates-for-unchanged-versions). Authorization and idle reservations remain unchanged.

Indefinite gateway binary retention is superseded by [D061](#d061-completed-rollout-binary-retention-and-selected-release-downloads). Supervisor recovery and node-local storage remain unchanged.

Extend D004's enrollment model with hashed, revocable common keys and a separately configured superuser key. Only the superuser can issue common keys, upload executable bundles, publish deployments, or trigger release checks. CLI administration help requires a verified superuser response; server-side authorization enforces the permission independently.

[D016](#d016-sqlite-backed-users-and-isolated-fleets) supersedes the common-key issuance restriction: account owners can issue keys for their own fleet. Executable publication and global rollout administration remain superuser-only.

The gateway can fetch stable GitHub releases from a configured repository or accept development bundles built by `make update`. Each manifest binds platform-specific executables to size and SHA-256. Nodes accept update commands only from the gateway, stage and validate their own binary, and acknowledge a local idle reservation before applying it. Track outgoing work, accepted task lifetimes, transfers, tunnels, and leases so coordination and peer-to-peer traffic also delay maintenance.

Persist installation participants and reservations across service restarts. Update nodes first and wait for their version acknowledgments, then restart the gateway. Expiring reservations release admissions if coordination is lost. Offline nodes catch up on reconnection. This introduces short maintenance pauses and lets a busy pool postpone an update indefinitely.

Use a stable supervisor and versioned executables in the state directory rather than replacing running Windows binaries. The original binary remains the launcher, and standalone CLI/MCP installations retain their own version. [D025](#d025-local-cli-self-updates) adds explicit updates for that installation. Selection is durable, shutdown is graceful, and the previous selection is retained for manual recovery. Startup failure does not automatically replay work or roll back state. See [managed updates](updates.md).

## D015: Single-machine invitations and per-user host setup

The requirement to reserve every invitation's name before installation is superseded for automatic names by [D034](#d034-target-hostname-for-automatic-installation-names). Fixed-name reservations remain unchanged.

The Windows login-startup choice is superseded by [D021](#d021-automatic-windows-node-service).

The Unix per-user-only startup choice is extended by [D043](#d043-explicit-installation-context-and-unix-system-startup).

The refusal to replace existing invitation-based installations is superseded by [D026](#d026-replace-enrollment-within-the-existing-machine-profile).

Extend D004 and D014 with superuser-created installation invitations. A hashed, expiring ticket reserves a name and pins a platform binary. The target generates its private identity and credential locally, then signs a redemption that binds the credential hash to its identity, name, OS, and architecture. Downloading the script does not consume the ticket. Atomic redemption permits one identity, with repeat requests from the same persisted identity and credential to recover a lost response.

[D016](#d016-sqlite-backed-users-and-isolated-fleets) supersedes the invitation permission and global name reservation: owners create invitations within their fleet, and SQLite persists ownership and redemption.

[D020](#d020-target-selected-installation-architecture) supersedes selecting a single architecture at invitation creation for the default flow.

[D023](#d023-durable-owner-controlled-machine-lifecycle) supersedes metadata-only removal for the new unregister command with a durable stop policy and a retired identity.

This avoids distributing a shared pool token in installation scripts. Revocation blocks future authentication and disconnects gateway sessions, but does not terminate established direct peer connections. Forgotten offline registrations retain their local state. Hash-only ticket storage means the gateway cannot reconstruct a lost installation URL.

Use per-user startup through systemd, launchd, or Windows login registration, with a reported process-only fallback where user startup is unavailable. Wait for local identity and online directory checks before reporting successful setup. Keep CLI/MCP credentials in one saved profile. Store host administration separately and configure the host to deny remote execution and file/network access under its administrative account. See [installation and enrollment](installation.md).

## D016: SQLite-backed users and isolated fleets

Each gateway user owns a private fleet. The operator provisions user accounts with a high-entropy key; account owners issue common keys and installation invitations for their fleet. Credentials determine user ownership. User-supplied fields cannot select another fleet. Gateway-wide software publication remains an operator responsibility.

Scope discovery, offline events, name uniqueness, enrollment management, and every signaling/relay packet by user. Verify scoped directory membership before peer setup and node dispatch, including streaming artifact grants. Use the existing pinned mutual TLS on both transports. Keep permanent identity ownership after forgetting a node so existing direct sessions cannot cross a later ownership change. Account/key revocation prevents future gateway authentication but does not terminate accepted work or existing same-fleet direct sessions.

Persist accounts, credential hashes, invitations, registrations, and identity ownership in SQLite with foreign keys, per-user name uniqueness, WAL, and full synchronous commits. Use the pure-Go driver to retain all supported CGO-free builds. Protected in-memory maps cache the database; commit mutations before acknowledging them. Preserve the existing filesystem storage for node-owned work and global executable assets/rollout state.

Import existing JSON registration state into one legacy fleet transactionally, retaining the source files without reading them again after migration. There is no evidence for assigning old shared-pool nodes to separate people, so migrating them into new accounts requires fresh identities. Upgrade the gateway before new nodes because peers now require fleet-aware authentication. This decision supersedes the shared-pool and gateway JSON assumptions in D004/D009 and the fleet-management permissions in D014/D015. See [users and private fleets](users.md).

## D017: Git-tag-based software versions

Use `git describe --tags --always --dirty` for local binaries and update bundles, matching expose. Release builds use the pushed `v*.*.*` tag explicitly. Untagged repositories report a short commit hash; builds without Git metadata report `dev`. Make's `VERSION` and the builder's `--version` flag allow explicit overrides.

This ties version labels to source history rather than build time. Repeated builds of one checkout can share a version label; UTC build timestamps and executable checksums still identify individual builds. CLI, node, gateway, and MCP software identification share the embedded version. Protocol versions remain independent. See [managed updates](updates.md).

## D018: Direct gateway observation with optional peer activity

Read gateway identity, uptime, cached host metrics, and the user's directory directly through authenticated `GET /v1/status`. Any valid fleet credential may read gateway capacity; the directory remains scoped to that credential's user. The gateway samples its host independently of polling and conceals private disk paths and raw collection errors.

This lets the dashboard identify and monitor the remote gateway even when the observer has no local node. Live execution metadata still comes from authorized peer observation through the local API. Merge it only for IDs in the directly authenticated directory. Missing peer observations remain unknown, while gateway failures retain a stale prior view. No execution telemetry or task control moves into the gateway. This partially supersedes D002 and D012. See [dashboard and system metrics](dashboard.md).

[D022](#d022-owner-visible-machine-health-without-a-local-node) supersedes the directory-only machine observation limitation with owner-scoped aggregate health.

## D019: Persistent CLI login independent of node setup

Separate saved update authorization extends this design in [D035](#d035-separate-saved-authorization-for-development-updates).

`control login` validates an API key against the selected gateway and saves both in the existing private `admin.json` profile. Later gateway commands load that credential automatically. Explicit credentials override saved ones; saved keys are never reused for another gateway URL. Failed validation leaves the previous login intact. This follows expose's saved server/key model while adding validation before saving.

Directory listing, invitations, and gateway observation need no local node. `setup` reuses the login to issue a separate node credential and start the host for peer execution. This extends D015 and D018 without putting account keys in node configuration or MCP entries. See [installation](installation.md).

## D020: Target-selected installation architecture

Default invitations select an OS and pin its amd64 and arm64 assets from one published manifest. Bash or PowerShell detects the target architecture, downloads that candidate, and verifies its pinned checksum. Signed redemption includes the chosen architecture and persists it with the identity and credential. Recovery cannot change that binding. Existing explicitly pinned invitations keep their original proof format.

This removes architecture and lifetime questions from the dashboard wizard while retaining reproducible downloads and platform-bound registration. It requires both architecture binaries in the published bundle and compatible gateway/installer versions. The wizard uses the shared invitation client and native clipboard tools, displays the command on copy failure, and never retries an uncertain invitation request automatically. See [installation](installation.md) and [dashboard](dashboard.md).

## D021: Automatic Windows node service

The Windows `user`-mode alias and exclusive system-service startup choice are superseded by [D033](#d033-explicit-windows-user-login-startup). Automatic mode retains this decision.

Install Windows nodes as automatic Service Control Manager services instead of per-user Run entries. Each absolute profile path identifies one service. The service wraps the existing versioned supervisor, starts before login, handles stop/shutdown through context cancellation, and uses SCM recovery after failures. Background child processes suppress console creation, including executable validation and provider commands.

Installation requires Administrator PowerShell and checks elevation before issuing setup credentials or redeeming an invitation. Automatic mode never falls back to a detached process on Windows. LocalService runs the node with explicit access to its runtime files rather than LocalSystem privileges or an interactive user's credentials. Providers therefore need service-account paths and credentials. An explicit process mode remains available for development. Existing profiles and identities survive migration; service start stops the old supervisor and removes its matching login entry. Linux and macOS retain per-user startup. See [installation](installation.md).

## D022: Owner-visible machine health without a local node

Nodes publish aggregate active-work count, lease presence, and cached system metrics over their existing authenticated gateway connection every five seconds. The gateway retains one report per connection, expires it after 20 seconds, and returns it only to that fleet's account key. The superuser sees reports only for its legacy fleet. Common keys still require authorized peer observation.

This lets a logged-in dashboard show machine CPU, idle/busy state, and work counts without a local node. Reports contain no operation records, owner IDs, arguments, credentials, or private filesystem paths. Task control and detailed activity remain on the peer API. Sampling runs independently of reporting and dashboard polling. Feature negotiation through `/v1/auth` prevents reports from disconnecting older gateways during nodes-first updates. This extends D018 and preserves fleet isolation and task ownership. See [dashboard](dashboard.md) and [protocol](protocol.md).

## D023: Durable owner-controlled machine lifecycle

The lifecycle-support requirement for offline unregistration is superseded by [D024](#d024-offline-unregistration-without-lifecycle-support).

Retention of startup registration and the installed executable after unregistration is superseded by [D049](#d049-self-uninstall-after-durable-machine-unregistration).

Store disabled state and permanent unregistration tombstones by immutable identity in SQLite. Expose fleet-scoped enable/disable/unregister commands through the shared client and dashboard. Nodes fetch their own policy at startup and every two seconds using timestamped Ed25519 proofs, persist it, and acknowledge the revision. Proof-based reads remain possible after the machine credential is revoked, without granting directory access or policy mutation.

Disable excludes a node from selection and rejects new execution at its work-admission gate. Existing accepted work can finish and remain observable and cancellable. This gate is independent of managed-update reservations. Unregister removes the directory entry, cancels work, stops the service cleanly, and prevents that identity from returning. Offline agents apply the stop when connectivity returns; service entries and local files are retained. Older agents must demonstrate support before the gateway accepts remote-stop commands. This supersedes D015's metadata-only removal contract for the new unregister command. See [installation](installation.md#manage-registrations) and [protocol](protocol.md#machine-lifecycle).

## D024: Offline unregistration without lifecycle support

The remaining online support requirement is superseded by [D027](#d027-unregister-older-agents-and-reinstall-retired-profiles).

Allow fleet owners to unregister any offline registration, even when the agent never demonstrated lifecycle support. Requiring an upgrade and reconnection prevents removal of machines that are no longer accessible. Keep the same durable identity tombstone and installation-credential revocation as online unregistration.

Online unregister still requires lifecycle support. Compatible offline agents receive the stop policy when they next contact the gateway. Older agents cannot rejoin with the retired identity, but require a local stop if still running. This supersedes only D023's support requirement for offline unregistration. See [registration management](installation.md#manage-registrations).

## D025: Local CLI self-updates

[D047](#d047-skip-updates-for-unchanged-versions) adds a version-based skip before binary download and replacement.

Bare `control update` replaces the invoked CLI with a verified public GitHub release; `upgrade` is an alias. No gateway role is required because this operation uses local filesystem authority and the same release publisher trusted by the installers. Keep `update push`, `status`, and `check` as superuser-only fleet administration. This supersedes D014's restriction on the bare update command and its help, while retaining gateway publication and rollout authorization.

Validate the manifest, pin the download to its version, check size and checksum, and execute `version --json` before replacing the CLI. Serialize updates with a local lock. Use atomic replacement on Unix and rename the old executable aside on Windows, preserving permissions and restoring the old path if installation fails. A locked Windows backup remains until removed after its processes exit. Running services retain their existing process and managed runtime selection, so a local CLI update does not bypass idle reservations or change an active rollout. See [CLI updates](updates.md#update-the-local-cli).

## D026: Replace enrollment within the existing machine profile

[D027](#d027-unregister-older-agents-and-reinstall-retired-profiles) extends this flow to profiles whose identities were already retired.

[D037](#d037-replace-unfinished-enrollment-with-a-new-invitation) supersedes the requirement to finish pending enrollment before using a new invitation.

Running a new invitation against an existing local profile replaces its registration, including when the name changes. Reuse the profile's Ed25519 identity and execution settings. Stop the existing service before replacing its executable or profile, and retain pending enrollment for recovery. The gateway atomically renames the directory entry, binds the new credential, and revokes previous invitation credentials. This supersedes D015's refusal to replace existing installations.

The local identity identifies the machine, rather than its display name or a hardware fingerprint. An invitation for an already registered name reserves that identity; another machine cannot claim it. Replacement preserves immutable fleet ownership, disabled policy, and retired-identity rejection. Installed identities use their current bound credential, so an older common-key configuration cannot restore the previous name. Users keep their workspace and task history, but running work is interrupted by the explicit re-registration. Separate profiles remain separate installations. See [installation and enrollment](installation.md#add-a-machine).

## D027: Unregister older agents and reinstall retired profiles

The compatible-node stop-only behavior is extended to self-uninstall by [D049](#d049-self-uninstall-after-durable-machine-unregistration). Older-node retirement and fresh-identity replacement remain unchanged.

Allow owners to unregister online agents without requiring lifecycle support. Registration removal and identity retirement are gateway operations; requiring a remote-stop implementation prevents owners from removing old agents. Commit the tombstone and credential revocation before closing the gateway connection. Compatible agents stop through their policy channel. Older agents need a local stop, and existing direct sessions can continue until then. This supersedes D023/D024's remaining online-unregistration restriction.

When a new invitation runs against a retired local profile, create a fresh identity in a separate state directory. Preserve the workspace, profile settings, and old state files, and persist the chosen identity for recovery. This extends D026 without reviving retired identities or reassigning their fleet ownership. Inspect retirement through a signed `inspectOnly` policy query so an installer cannot accidentally advertise lifecycle support for an old running agent. Upgrade the gateway before using this installer proof format. See [installation](installation.md#add-a-machine).

## D028: Command-scoped outbound peers for CLI and MCP

The local-API preference and local-node requirement for activity aggregation are superseded by [D045](#d045-account-client-authority-and-instruction-bound-worker-delegation).

Its machine-registration contract is superseded by [D029](#d029-authenticated-client-sessions-are-not-fleet-machines). Command-scoped transport, persistent task ownership, and pre-submission backend selection are retained.

Allow authenticated CLI and MCP processes to execute remote work without a separately running local node. Prefer the local API, but probe it with a read-only request before submitting work and use a command-scoped peer only on connection refusal. Explicit API settings disable fallback. Never switch backends after an uncertain submission or retry an application operation automatically. This supersedes D002's local-node requirement for remote execution, artifact downloads, and tunnels.

Reuse the existing signed registration and peer transport so deployed gateways and nodes need no protocol upgrade. A stable outbound-only identity is stored separately for each gateway and authenticated fleet. It appears as a capability-free `cli-<identity-prefix>` registration, remains in the directory after disconnecting, and retains task and lease ownership across standalone commands. It exposes no local listener or execution providers. Worker selection excludes capability-free peers. Remote access rules still apply; an account key does not bypass them.

The current gateway permits one connection per identity. An exclusive file lock rejects overlapping standalone processes instead of replacing their connection or changing task ownership. Use a persistent local node for concurrent CLI/MCP clients. Standalone identities do not receive managed binary updates; update the CLI itself. Pool activity aggregation and local execution still require a local node. Tasks submitted through a local node remain owned by that node, not by the standalone identity. See [standalone CLI and MCP](installation.md#standalone-cli-and-mcp).

## D029: Authenticated client sessions are not fleet machines

The one-connection-per-owner and process-long file lock are superseded by [D030](#d030-concurrent-client-connections-with-stable-task-owners). Fleet separation remains unchanged.

The orchestrator's CLI/MCP is a requesting client, not a fleet execution agent. Replace D028's machine registration with a separate authenticated client-session endpoint and role-bound challenge proof. Persist only immutable account ownership and client role; keep connection metadata transient. Client sessions do not create fleet entries, dashboard rows, health samples, lifecycle policy, update-platform requirements, or rollout participants.

Workers authenticate live client identities through an account-scoped peer lookup, then enforce the same pinned TLS, access rules, task ownership, and artifact grants as other peer callers. Fleet machine discovery and selection never include clients. Credentials bound to an installed machine cannot open client sessions, and client identities cannot later enroll as machines. Limit each account to 64 active client sessions. Existing one-connection-per-identity and local file-lock behavior remain unchanged.

This contract requires gateway and worker support. Advertise `clientSessions` in authenticated gateway metadata and compatible machine records; reject unsupported standalone execution before work is sent. Preserve old machine registration proofs when connected to an older gateway. Schema version 3 stores client roles independently of nodes. Gateway startup moves only the previous implementation's exact unbound, capability-free CLI registrations to client identities, retaining their account and task-owner identity. No retired identity is restored. See [standalone usage](installation.md#standalone-cli-and-mcp) and [database upgrades](users.md#persistence-and-upgrades).

## D030: Concurrent client connections with stable task owners

The full-close forwarding and single outbound-session transport are extended by [D031](#d031-isolated-traffic-lanes-shared-webrtc-carriers-and-duplex-streams). [D042](#d042-process-owned-reverse-tcp-forwarding) adds reverse listeners. Stable ownership and client/fleet separation remain unchanged.

A long-lived tunnel or MCP process must not block another CLI process from querying logs or cancelling a task. Retain the gateway/account-scoped persistent client key as the task and lease owner, but generate a separate TLS identity per requesting process. Hold the local owner-key lock only during key loading or creation. The owner signs a purpose-separated challenge proof authorizing that transport identity and metadata. The transport identity also signs the existing client-session proof. An account credential alone cannot impersonate another task owner.

Persist immutable transport-to-owner bindings in SQLite schema version 4 before connection acknowledgement. Workers derive stable ownership exclusively from authenticated peer metadata. Access rules may match the owner ID or its existing derived client name; TLS pinning and artifact grants remain bound to the transport recipient. Machine identities cannot become client owners, and client owners or transport identities cannot enroll as machines. Discovery, health, updates, and the 64-live-client limit remain as in D029. Negotiate `clientOwners` on both gateway and target before sending work, retaining old single-identity client compatibility on upgraded servers.

Keep TCP forwards process-owned in the shared client, with loopback defaults, at most 32 listeners and 128 active connections per listener. MCP start returns promptly and survives tool-call completion. Stop and process shutdown close listeners and sockets and join forwarding goroutines. Forwarded TCP remains full-close on either direction ending; half-close, UDP, reverse listeners, PTYs, and detached local forwarding services are not implemented. Durable tasks provide long-running execution independently of client lifetimes, with CLI detached submission, timeout selection, and offset-based log following. Never reconnect and replay an existing TCP socket or uncertain task submission. This supersedes D028/D029's concurrency restriction, without turning an orchestrator into a fleet worker.

## D031: Isolated traffic lanes, shared WebRTC carriers, and duplex streams

Use independently multiplexed control, bulk, and interactive lanes per outgoing peer. Bound stream and setup admission, coalesce setup by destination/lane, and let cancelled waiters leave without blocking other peers or cancelling shared setup. Preserve established streams rather than replacing live sessions when name and ID resolution race. Stable-ID authentication uses the scoped identity endpoint without fetching the whole fleet catalog.

Negotiate `peerChannels` in authenticated gateway and signed peer metadata. Compatible peers reuse an ICE/DTLS/SCTP carrier and open separate ordered reliable data channels for lane-specific pinned TLS/yamux sessions. Older peers keep independent connections. Relay lanes still share the gateway WebSocket; this is not strict bandwidth QoS. Individual lane closure preserves siblings, but carrier failure can affect all its channels. Larger bounded caller-side bulk receive windows and reusable packet/copy buffers reduce flow-control limits and allocation churn.

Negotiate bounded TCP data/EOF records through `tcp.open` and the local WebSocket handshake. This adds write-side EOF without relying on yamux's short half-close timeout or WebSocket close frames. Framed task-log following uses the existing `tasks.logs` permission and immutable owner checks, with persisted offsets and append/completion notifications instead of polling. Local HTTP follows NDJSON records. Unsupported receivers retain raw TCP or read-only log polling; failures never trigger automatic application replay or stream reconnection.

Serialize gateway WebSocket writes through a bounded process-owned queue. An in-progress write uses the peer lifetime with a write timeout; application cancellation stops waiting without closing the shared WebSocket. Keep each queued packet bound to its original socket so gateway reconnection cannot replay it.

This extends D003/D005/D030 without changing the gateway routing envelope or moving execution into the gateway. Keep resource sampling, maintenance admission, and client/fleet roles unchanged. See [streaming behavior, scenarios, limits, and remaining gaps](streaming.md).

## D032: Operation roles and component terminology

Use orchestrator for the caller coordinating work, worker for a node executing requested work, and gateway for the enrollment, discovery, signaling, relay, and fleet-administration service. Client names the CLI/MCP component and node names the enrolled execution service. Reserve agent for AI software. Apply the [terminology](terminology.md) to documentation, code, CLI/dashboard text, and AI-facing instructions.

This clarifies D001 and D029/D030 without changing their contracts. Orchestrator and worker remain operation roles, not separate runtimes or fixed machine categories. A node can perform both roles; a standalone client can coordinate without enrolling a machine. Keep existing command, configuration, protocol, authentication-role, and session-backend identifiers. Rename internal non-AI helpers such as the node updater rather than calling them agents. Historical decision wording remains readable under the current glossary.

## D033: Explicit Windows user-login startup

The invitation-script default and its reuse of saved startup mode are superseded by [D038](#d038-user-login-startup-by-default-in-windows-invitation-scripts). Direct setup and service commands retain this decision.

Keep Windows `auto` startup as an automatic LocalService SCM service. Make explicit `user` startup a scheduled task at the installing user's login, with a limited interactive token and no stored password. A hidden PowerShell launcher waits for the existing node supervisor and propagates its exit status for bounded Task Scheduler recovery. Log output remains in `node.log`. Profile-derived task names and user/description checks keep startup mutations scoped to the current profile and OS user.

This allows providers to access that user's Documents, application credentials, and desktop session. It also gives unrestricted execution that user's authority. The node is unavailable before login and after logout. Automatic mode never silently falls back to user mode. New user installations need no elevation; a system-to-user migration requires Administrator PowerShell under the intended user before enrollment side effects. Migration stops and disables SCM without replacing the identity or workspace, retaining the service registration for explicit rollback. Selecting automatic mode removes the current user's login task before SCM resumes.

Persist the Windows startup selection beside the configuration and reuse it for service commands and re-enrollment unless explicitly overridden. This supersedes D021's `user` alias and exclusive system-service startup choice, not its automatic-service contract. See [installation and migration](installation.md#switch-windows-to-user-login-startup) and [native verification](testing.md#local-development-commands). Login/logout behavior and console visibility require Windows runtime verification.

## D034: Target hostname for automatic installation names

Use the target machine's OS hostname for an automatic installation name, selected by `machines add auto`, an omitted CLI name, or a blank dashboard name. Send an explicit `autoName` flag with an empty name so an older gateway rejects the request instead of reserving the literal name `auto`. Resolve the name only in the target's pinned installer, preserve it in pending enrollment, and sign it in redemption. Do not derive it from the orchestrator, alter its case, add suffixes, or silently retry a collision.

Automatic invitations reserve no name before redemption. Multiple such invitations can coexist. At redemption, check name validity, private-fleet ownership, client-name reservations, existing registrations, and active explicit invitations under the gateway lock. Commit the selected name, credential, and identity ownership in the existing SQLite transaction before acknowledgment. Response recovery must repeat the resolved name. Fixed-name invitations keep their reservation and legacy proof format; automatic proofs add an optional signed name field. Neither flow may revive retired identities or change fleets.

This supersedes D015's pre-install name reservation only for automatic invitations and extends D020/D026's target-selected metadata and replacement behavior. A valid hostname collision requires choosing an explicit name or resolving the existing reservation. Both gateway and pinned installers must support the new field. See [installation contracts](installation.md#add-a-machine) and [protocol](protocol.md).

## D035: Separate saved authorization for development updates

The automatic key prompt in `make update` is superseded by [D036](#d036-persist-operator-login-and-do-not-prompt-during-updates). Separate gateway-scoped update authorization remains unchanged.

Make `make update` authorize before platform bundle builds. When the current login is not a superuser, securely request the gateway superuser key once, verify it with `/v1/auth`, and persist it separately in `update-admin.json`. Do not replace the user's fleet login or grant update permission to an ordinary account. Explicit global `--token` and `CONTROL_SUPERUSER_KEY` override cached update authorization; cached credentials remain bound to the selected gateway. Failed validation leaves existing credentials intact.

This lets development updates run as one command without exports or switching the dashboard into the operator's legacy fleet. Only update administration reads the new profile. Ordinary CLI help, fleet commands, execution, and MCP retain their current authority. Noninteractive callers can provide an already authorized credential or use `update authorize --key-stdin`. This extends D014/D019 without changing superuser-only publication, fleet boundaries, or idle rollout. See [development updates](updates.md#push-from-a-development-machine).

## D036: Persist operator login and do not prompt during updates

Persist a validated superuser login in both the regular login profile and the separate update authorization profile. Subsequent fleet-account logins leave the update authorization intact. Repeated login to the same gateway revalidates the saved key rather than requesting it again; explicit key input selects a replacement. Saved keys remain bound to their gateway.

When a normal login replaces an operator key saved by an older CLI, revalidate that previous key against the same gateway and preserve it for updates before overwriting the regular profile. Only a positively verified superuser key is retained.

Run `update authorize --check` from `make update` so builds and publication reuse persisted authorization without an automatic key prompt. Missing permission stops the target before platform builds. Keep explicit `update authorize` as an opt-in credential entry command. This supersedes D035's automatic prompt, not its separate credential storage or superuser-only publication. A normal fleet login is not silently promoted into gateway update authority. See [login persistence](installation.md#log-in-once) and [development updates](updates.md#push-from-a-development-machine).

## D037: Replace unfinished enrollment with a new invitation

Allow an explicit new invitation to replace pending enrollment in the same profile, gateway, and fleet. An expired or revoked ticket must not prevent installation with a valid replacement. Validate the invitation, pinned executable, profile, and identity before atomically saving new recovery state under the installation lock. Keep the active profile until redemption succeeds. Repeating the same invitation retains its credential and resolved name; selecting a new invitation creates a new credential.

Preserve any replacement identity already selected by an unfinished attempt, since a lost response may mean the gateway has enrolled it. The new redemption replaces that identity's credential instead of creating another machine. If the selected identity was subsequently retired, inspect its policy and choose fresh state as in D027. Gateway ownership, name reservations, single-use tickets, and identity proofs remain enforced. This extends D026/D027 and supersedes the requirement to finish an old pending attempt first. See [enrollment recovery](installation.md#single-use-and-recovery).

## D038: User-login startup by default in Windows invitation scripts

Explicit invitation context takes precedence over this default under [D043](#d043-explicit-installation-context-and-unix-system-startup). Invitations without a context retain this decision.

Generated Windows invitation scripts select `user` startup unless `CONTROL_SERVICE_MODE` explicitly selects another mode. Pass that selection as `enroll --service` rather than changing the caller's environment or inheriting saved system startup. This applies to both new installations and replacement enrollment. Direct `control setup`, direct enrollment without an override, and service commands keep their existing defaults and saved-mode behavior.

Machines added for remote execution commonly need the installing user's Documents and application credentials. LocalService cannot read those files by default. User-login startup provides that user's normal permissions without a password or elevation on new installations, but also gives unrestricted providers that user's authority and makes the node unavailable outside their login session. Boot-time execution remains available through an explicit `auto` override. Migration from a system service still requires Administrator PowerShell under the intended user before enrollment side effects.

This supersedes D033 only for the invitation-script default and saved-mode reuse. Identity, profile, ownership, checksum verification, and service migration checks remain unchanged. The gateway must serve the updated scripts, and their pinned installer must support user-login startup. Existing running nodes are not changed by deploying the script update. See [installation](installation.md#add-a-machine).

## D039: Dashboard-owned selection with automatic local clipboard copy

Use terminal mouse reporting to handle left-button drag selection in dashboard tables and dialogs. Native terminal selections are invisible to the application and can be cleared by live redraws, so the dashboard captures its visible rendered screen on press. Polling continues while that screen stays fixed during the drag. On release, resume the live display and copy only the selected plain text to the orchestrator's local clipboard. Preserve whole Unicode graphemes and remove terminal styling and trailing line padding.

Clipboard commands use the existing platform-specific helpers with a five-second context deadline. Permit only one copy at a time, report failures in the footer, and retain the selection for an explicit `c` retry. A single click copies nothing; keyboard input and resizing cancel an unfinished drag. No snapshot fields outside the displayed cells are copied. This enables copy-on-selection without changing terminal settings, but terminals must support mouse reporting. Shift-drag remains a native-selection override where supported, and `--once` provides ordinary static terminal output. See [dashboard usage](dashboard.md).

## D040: Opt-in desktop automation through a serialized helper

Expose GUI automation as `rpa.run` only when a node explicitly configures an `rpa` command. Keep OS libraries in a version-1 subprocess helper so Control retains CGO-free builds and providers remain independent of transport. The supplied Python helper uses native accessibility targeting on Windows, macOS, and Linux, with explicit PyAutoGUI coordinate/keyboard actions. Linux coordinate input initially requires X11, and execution requires a logged-in desktop and its OS permissions.

Validate the whole batch before execution, cap it at 100 actions and two minutes, and serialize it with a shared file lock under the OS user's Control configuration directory. Native selector actions must resolve exactly one element in a complete bounded scan and must not silently fall back to coordinates. Import screenshots as bounded PNG artifacts from an invocation-owned temporary directory. Retain existing fleet authorization, tracked-task ownership and idempotency, leases, and update admission.

GUI batches are not atomic transactions. Earlier actions and even a failed action may have changed an application. Stop at the first error and retain partial results in tracked tasks; never replay automatically after failure, disconnect, or restart. A node lease coordinates multi-batch callers on that node, but neither it nor the desktop lock blocks humans or unrelated automation. External helper dependencies and permissions need native runtime verification. See [GUI automation](rpa.md).

## D041: Worker-local secret references for credential entry

Manage credentials only through local `control secrets set|list|delete`, with hidden terminal entry or explicit stdin outside AI conversations. Store private atomic files under the worker profile's data directory, outside `workDir`. Do not add a value-read command, peer operation, MCP tool, or resource. Resolve names only in GUI `setValue`/`type` input and HTTP `headerSecrets` at execution. Keep persisted task specifications and idempotent submission comparisons reference-only. The gateway never stores or synchronizes these credentials.

Mask configured values and common textual encodings in supported GUI/HTTP results and errors. Suppress raw desktop-helper task logs while credentials exist to avoid chunk-boundary and truncation leaks. Never follow redirects on secret-bearing HTTP requests. Existing capability authorization permits credential use and existing task, lease, and admission semantics remain unchanged. Queued work uses the value present at execution, not acceptance.

This prevents accidental prompt and tool-result exposure, not hostile extraction. Private files are not encrypted or OS-keychain-backed. Screenshots, unrestricted execution, application files, arbitrary transformations, historical values after rotation, and malicious destinations remain outside the protection. Stronger isolation would require an approved-destination credential broker and narrower provider authority. See [secrets and limits](secrets.md).

## D042: Process-owned reverse TCP forwarding

Add `control tunnel --reverse` and MCP `control_forward_start` with `reverse: true` to expose an orchestrator-local service on a remote machine's loopback port. Require separate `tcp.listen` authorization on that node. Preserve fleet checks and work admission, and default to loopback. An explicitly selected non-loopback listener exposes the service to the remote network without additional listener authentication.

Carry accepted sockets through a bounded nested yamux session inside one authenticated interactive `tcp.listen` stream, with duplex EOF records per socket. The client dials only the destination chosen when creating its forward. This permits standalone clients without enrollment or general incoming execution authority and works through the local node API without moving local service access into that node. HTTP and WebSocket traffic remain ordinary TCP bytes.

Listeners belong to the requesting process, not a task or durable worker service. Cap them at 32 per client and 32 per receiving node, with 128 active sockets per listener. Idle listeners still block node updates. Stop, shutdown, expiry, or transport failure closes listeners and sockets and joins their goroutines; never recreate a listener or replay a socket automatically. A lost start acknowledgement can leave a listener briefly bound until its stream closes. There is no automatic retry. This supersedes D030's reverse-listener exclusion, not its UDP, PTY, or detached-service limits. See [protocol](protocol.md#orchestrator-sessions-and-forwards).

## D043: Explicit installation context and Unix system startup

Ask for User context or System context after the dashboard's platform selection. Keep user as the wizard default and expose the same choice through `machines add --service user|system`. Persist optional `serviceMode` in invitation metadata, validate it before creating the ticket, and require client-side response confirmation. Scripts honor explicit invitation context ahead of environment defaults. Omitting the field keeps previous platform defaults. Identity, ownership, name reservation, checksums, and redemption proofs remain unchanged.

Linux user services can stop when the last login session ends. System context provides boot-time startup that survives logout without requiring user-manager linger. Linux uses profile-scoped systemd units under `/etc/systemd/system`; macOS uses profile-scoped LaunchDaemons in launchd's system domain. Install both as root with separate system profile paths and retain the selected mode for later service commands. System installation requires elevation before enrollment and never silently falls back to a user service or detached process. Windows maps system context to the existing automatic LocalService service.

Root execution on Unix gives unrestricted providers root authority and does not provide the installing user's desktop or application credentials. The wizard warns about this difference. User mode remains appropriate for personal files and desktop integrations; Linux linger can retain an existing user installation across logout without changing its identity or execution account. Unix user-to-system identity migration is not automatic. This extends D015's Unix startup choice and D038's invitation defaults without changing Windows migration behavior. Native boot/logout behavior still requires runtime verification. See [installation contexts](installation.md#choose-a-startup-context).

## D043: Owner-selected machine routing aliases

Permit fleet owners to rename online and offline registrations through the shared client, CLI, and dashboard, without restarting a node or replacing its credential. Store an optional routing-name override with the gateway's existing identity policy and commit it together with the directory name. Apply it after signed identity and fleet checks on reconnection. Preserve admission revisions, work, identity ownership, and original signed installation proof names. Use the override for enrollment reservations and bound-credential authorization; fresh replacement enrollment clears it transactionally.

Names obey existing enrollment syntax and fleet uniqueness, invitation reservations, and client-name reservations. Notify only connected peers in that fleet with a gateway-only `directory.changed` packet. Compatible peers clear name caches without closing identity-bound sessions, and prevent lookups started before notification from reintroducing stale aliases. Reconnection clears caches when notifications were missed. Older peers can retain cached aliases until restarted. Name-based access rules and scripts need updating; stable IDs do not. This extends D023's owner controls and D026's replacement enrollment without changing their stop or credential-rotation contracts. See [registration management](installation.md#manage-registrations).

## D044: Explicit clipboard pastes with streamed files

Expose bidirectional clipboard transfer as an explicit CLI/MCP paste, not a background watcher or a native desktop paste hook. Keep source desktop access in platform-specific clipboard helpers and shared orchestration in `internal/client`. Text replaces the destination OS clipboard; file references become regular-file pastes into an existing directory. Standalone clients use the existing authenticated peer transport without enrollment or inbound execution handlers.

Use separate `clipboard.open` and `clipboard.paste` permissions, with fleet checks, bounded bulk-lane streams, and maintenance admission. A source reads only OS-selected file references, never paths supplied by the caller. Send basenames and sizes first, wait for destination acceptance, then stream file bytes and SHA-256 trailers. Keep bytes out of control frames and avoid artifact staging or whole-file buffering. Receivers confine remote paths to `workDir` and publish verified files atomically without replacing existing entries.

Clipboard transfer exposes the desktop user's data, including selected files outside the workspace, and is not secret-masked. Default trusted-fleet rules still apply; sensitive hosts should restrict the new permissions. Native clipboard access requires that user's desktop session. File transfers need hard-link support, support regular files only, and do not delete cut sources. Cancellation removes unfinished temporary files, but completed files and uncertain text side effects require explicit reconciliation. Never resume or replay automatically. This extends D005's streamed operations and D039's local clipboard helpers without changing gateway routing or durable task semantics. See [clipboard behavior and limits](clipboard.md).

## D045: Account-client authority and instruction-bound worker delegation

Authenticate execution initiation separately from fleet membership. The gateway assigns authority to account-authenticated client sessions, not enrolled nodes or common-key clients. Default CLI/MCP routing always uses this client identity; explicitly selecting a local API retains local execution and discovery only. Nodes remain symmetric runtimes and can coordinate delegated operations without gaining ambient peer authority.

Let destinations issue subject- and owner-bound grants for exact instructions. CLI/MCP prepares grants before instructing a worker through `peers.call`, workflows, or artifact delivery. Task grants include one explicit task's lifecycle; dynamic workflow inputs require signed upstream-task/output provenance. Do not put authority in worker-wide access rules, provider environments, or persisted task specifications. Keep child tasks under the original client owner. Single-use synchronous instructions prevent replaying side effects under a still-live grant.

Keep grant records in bounded destination-owned memory. Expire them after an hour idle, keep accepted tasks active, and renew streams only on actual traffic. Revocation cancels admitted tasks and streams, including derived artifact access. Restart loses grants and marks unfinished tasks interrupted rather than restoring authority. This trades grant continuity across restart for fail-closed recovery and needs no gateway scheduling or database migration. Account clients need the live gateway for new undelegated authority checks; issued grants remain destination-owned. Revocation requires contacting each destination and cannot undo effects.

Negotiate support and refuse older targets rather than silently using their old trust rules. Upgrade the gateway and every node before relying on worker isolation. Keep account credentials off workers; unrestricted providers and fully compromised hosts remain outside an execution sandbox. This supersedes unrestricted peer initiation in D001/D004, default local-node routing in D002/D028, and D008's stateless artifact-grant revocation limitation. See [delegation](delegation.md).

## D046: Windows firewall rules and a stable verified runtime path

Generated Windows worker installers request scoped firewall setup explicitly through `enroll --firewall`. Elevate only the firewall operation with UAC when necessary, passing the installed executable and exact profile by absolute path. Keep enrollment and user-login execution under the installing user's account. Failed or declined configuration is an installation error with pending enrollment retained. Direct CLI installations can opt in; `service firewall` repairs an existing profile without restarting it.

Allow inbound/outbound UDP for WebRTC and outbound TCP to the gateway port for the launcher and one profile-specific stable runtime path, on all Windows network profiles. Do not open inbound TCP, disable firewall protection, or change unrelated rules. Rule names and ownership metadata are deterministic per profile; repeated setup refreshes only owned rules. Keep rules after service uninstall for profile restoration, with explicit removal documented.

Windows firewall application filters use exact executable paths. Versioned update paths would need new privileged rules for every deployment, while a machine-wide UDP port rule would authorize unrelated processes. Instead, the Windows supervisor copies each checksum-verified selection to `<dataDir>\runtime\control.exe` before launch and only after the previous child exits. Preserve versioned source binaries and selection/rollback records; never replace a running child or automatically roll back an uncertain update. Unix execution paths stay unchanged. This extends D014's supervisor implementation and D038's worker-installation flow without changing identity, execution authority, startup context, or idle reservations. Older supervisors need one installed-CLI update and restart. See [firewall behavior](installation.md#windows-firewall).

## D047: Skip updates for unchanged versions

Treat the software version as the update boundary. Publishing the selected version returns its existing deployment unchanged, even when build timestamps or asset checksums differ. Nodes and the gateway skip staging and restart if their running version matches the target on the same platform. The CLI self-updater skips binary download and replacement after validating a same-version release manifest. Exact checksum matches retain their existing skip behavior.

Rebuilding one checkout must not restart a fleet merely because build metadata changed. This refines D014's checksum-based rollout acknowledgements and extends D025's CLI skip behavior. Keep checksum matching strict for binary validation, and preserve fresh deployment acknowledgements, participant tracking, and idle reservations whenever any service actually needs a restart. Offline nodes still catch up using the retained manifest.

Version labels must change to deploy new code. Reused `dev` and `-dirty` labels are skipped too; use an explicit new `VERSION` for another development rollout. Build and upload steps may still run, but same-version publication does not replace pinned assets. See [managed updates](updates.md).

## D048: Managed-update display grace

The minimum display delay after confirmed resumption is superseded by [D050](#d050-end-update-display-on-confirmed-resumption). Disconnect grace and persistence remain unchanged.

Show `update` during managed-update progress and retain it for at least one minute across an actual restart disconnect. Persist a gateway-owned deadline in the machine's directory record, retain it on reconnection, and ignore enrollment-supplied deadlines. Start restart grace only for the selected deployment when the running software needs replacement. Same-version skips do not start it.

This prevents expected update restarts from appearing as ordinary outages, including across gateway restart. Display grace does not change the online flag, routing, scheduling, last-seen time, or rollout acknowledgements. Peer activity enrichment cannot overwrite `update`. Once grace expires, a disconnected machine shows `offline` again. This extends D014/D022's observation behavior without changing their admission or authority rules. See [dashboard availability](dashboard.md#availability-is-separate-from-observability).

## D049: Self-uninstall after durable machine unregistration

Treat the existing identity-bound unregistration policy as an uninstall instruction for current installations. Keep the gateway's transactional tombstone, credential revocation, fleet isolation, and signed policy channel unchanged. Online nodes apply it on their next query; offline nodes receive it when connectivity returns. Persist retirement before cancelling work. A saved tombstone also triggers cleanup at startup without gateway access.

Record each installed profile's identity, launcher, and startup mode. Copy a cleanup executable into retained state and launch it independently of the node's service-manager process group. The helper takes enrollment, runtime, and node locks, verifies the retired identity again, removes only that profile's startup registration, and deletes the launcher only when no other installed profile shares it. Retain configuration, identities, workspaces, task/artifact and runtime state, MCP entries, skills, and firewall rules. Never recursively delete profile or workspace directories. These choices preserve reinstall recovery and avoid deleting a new enrollment through a delayed instruction.

On Linux use a transient systemd service; on macOS use a separate one-shot launchd job. Windows helpers inherit the node token. Installation enables a profile-specific service SID and grants only that SID access to delete/query its own service and delete the launcher and its binary-registration file. It does not elevate the node or grant general service administration. Existing Windows system installations need a current installed CLI and an elevated stop/start to acquire those permissions. Installations without a launcher record and older nodes retain stop-only behavior. Cleanup failures remain visible in node logs and durable retirement survives a later retry.

This supersedes D023's retained-startup/launcher contract and extends D027's compatible-node shutdown, without changing identity retirement or data-preserving reinstallation. See [registration management](installation.md#manage-registrations).

## D050: End update display on confirmed resumption

Clear a machine's restart display deadline when its authenticated connection reports the selected deployment, matching software version or checksum, and released maintenance. Persist the cleared deadline before accepting the report so a gateway restart cannot restore stale update display. Return to actual health or peer observation rather than forcing idle when work is running.

Keep the one-minute grace for disconnected or unconfirmed nodes. Ignore older-deployment progress for active update display, and keep current-version nodes labeled `update` while maintenance remains paused. This supersedes D048's minimum display delay after resumption without relaxing fresh rollout acknowledgements, idle reservations, fleet isolation, or actual presence tracking. See [dashboard availability](dashboard.md#availability-is-separate-from-observability).

## D051: Host-owned persistent tunnel definitions

The dashboard counter naming is superseded by [D052](#d052-separate-live-tunnel-activity-from-retained-definitions). Definition ownership and persistence remain unchanged.

Retain forward and reverse listener definitions in a separate orchestrator-host user client service. CLI and MCP submit/list/dispose through an authenticated loopback API, with private atomically written desired state and a single-runtime lock. Pin the destination's immutable ID and preserve an absolute optional TTL. Commit creation and removal before acknowledgement; identical IDs/specifications reconcile, while conflicting reuse fails. Keep the original foreground and process-owned APIs unchanged.

Restart or reconnect restores only listeners for new connections. Never replay, resume, or automatically resubmit interrupted TCP traffic. Reverse bind attempts can fail while an earlier listener is closing; bounded backoff retries the explicit retained intent without bypassing normal authorization. Every definition owns cancellable forwarding resources, and service shutdown joins them. Expiry closes listeners even if its state cleanup write fails, and persisted expiry prevents restoration later.

User startup restores definitions after login or user-manager startup without enrolling the host or requiring a node. Explicit process-only startup has no reboot/crash guarantee. Gateway URL and credential scope isolate services, permitting local listing/disposal during a gateway outage without borrowing another account's authority. This copies the selected account credential into protected client configuration; keep it outside worker-accessible state. Credential rotation does not migrate definitions implicitly. Native startup and permission behavior require OS runtime smoke tests.

Dashboard `Tunnels` counts this host login's retained forward/reverse definitions by machine ID, including retrying definitions but excluding expired ones. Count definitions rather than sockets, and do not copy addresses or credentials into fleet telemetry. Other hosts' definitions are intentionally not aggregated. This extends D031/D042's forwarding lifetimes without changing their process-owned contracts, fleet membership, task ownership, or peer transport. See [persistent tunnels](tunnels.md).

## D052: Separate live tunnel activity from retained definitions

Use dashboard `Tunnels` and peer snapshot `tunnels` for tracked live forward connections and reverse listeners across owners. Count `tcp.open` and `tcp.accept` as forward connections and `tcp.listen` as reverse listeners, once per listener regardless of socket count. Compute counts before aggregate detail truncation; older-node fallback counts use only returned active records. Unknown peer observation stays unknown rather than displaying zero.

Keep this host login's unexpired persistent definitions in dashboard `retainedTunnels` and a separate lower-priority `Saved` column. Retrying definitions are not live connections, and foreground or detached tunnel processes have no persistent definitions. Mixing the two made established listeners display zero. Idle local forward listeners are not observable on workers, so forward activity counts describe connected sockets, not local listener definitions. This supersedes D051's dashboard field and column naming without changing tunnel lifetimes, restoration, authorization, gateway health reports, or telemetry privacy. See [dashboard counts](dashboard.md).

## D053: Full carrier packets and negotiated packet size

yamux writes each frame header separately from its body, TLS seals every write in its own records, and the packet adapter split a full 16,406-byte TLS record into 16 KiB plus a 22-byte tail. An 8 MiB transfer produced 1,299 carrier messages, 782 under 128 bytes, and each relayed message is a protobuf packet through the gateway. Hold the packet adapter's output while one multiplexer frame is sealed, then send it in full packets; write duplex records and control frames in one call; join nested reverse-listener frame headers to their bodies. This needs no wire change because packets carry an arbitrary slice of the encrypted stream.

Every current receiver accepts 64 KiB packets with an 8 MiB per-link receive bound. Senders use 64 KiB only toward peers whose signed directory record advertises `largePackets`, negotiated through `/v1/auth` like `peerChannels`; older receivers reject larger messages and keep 16 KiB. Raise bulk caller windows to 4 MiB, interactive caller and accepted-session windows to 1 MiB, and the SCTP receive buffer to 4 MiB, raising per-stream and per-carrier ceilings on high-latency paths with bounded memory. Cap STUN candidate gathering at two seconds, or two fifths of the direct timeout, because Pion's five-second default equalled the direct timeout and one unreachable STUN server forced every session to the relay. Trickle ICE remains unimplemented: new signaling kinds would disconnect senders on older gateways.

Receivers bind an added lane's data channel inside Pion's `OnDataChannel` callback, which Pion completes before it delivers that channel's messages; only the TLS handshake runs asynchronously. Binding it in a goroutine could drop the dialer's first TLS record, stalling the lane until the direct timeout and pushing it onto the relay.

Hard-linking task inputs was considered and rejected. A provider can modify an input in place, which would alter the content-addressed store; inputs remain copies. This extends [D031](#d031-isolated-traffic-lanes-shared-webrtc-carriers-and-duplex-streams) without changing authentication, lanes, or replay rules. See [fleet streaming](streaming.md).

## D054: Cached identity lookups with offline eviction

Workers authenticated every non-delegated request with a gateway `/v1/peers/{id}` lookup, sometimes two or three per request, and delegated calls resolved names by downloading the whole directory. Polling task waits multiplied this into tens of thousands of gateway requests per long task, and established direct sessions could not run account-client work during a gateway outage. Reuse successful lookups of online records for up to 15 seconds. Evict an identity on the gateway's existing `offline` notification and clear everything on `directory.changed` or reconnection; never cache offline records, so a returning peer is never reported offline. A directory read populates name and ID entries for every listed machine.

A disconnected client's execution authority now ends at its offline notification, or within 15 seconds if that best-effort packet is lost. Account revocation already closed its gateway connection and so produces the notification. Membership entries for identities without links or sessions are forgotten on offline notifications and, beyond 1,024 entries, after ten idle minutes, bounding memory for per-process client transport identities. This refines D031's stable-ID lookup without changing who may execute work.

## D055: Incremental gateway commits and separate hot paths

Each connect, disconnect, and administrative change rewrote every gateway record in one fully synchronous transaction while holding the mutex that also routed every relayed packet. Per-process client transport bindings are permanent, so that cost grew with every CLI invocation, and every node reconnecting after a gateway restart repeated it. Commit only the records a mutation names: upsert present records, delete absent ones, and keep identity and transport bindings insert-only with their immutability checks. Mutations still commit before acknowledgement; the full rewrite remains only for startup migration.

Authenticate bearer credentials through an atomically published index keyed by credential SHA-256, rebuilt after committed user, key, or invitation changes, instead of scanning every credential under the gateway mutex. Credentials are 256-bit random values, so digest lookup leaks nothing useful through timing; the superuser and bootstrap tokens keep constant-time comparison. Route relay packets through a separate read-locked connection map so commits and administration never stall forwarding.

Bound each destination queue by 1,024 packets and 8 MiB of relay payload. A data packet that does not fit closes only that relay session: the sender is told to close it, and the session's remaining packets are dropped until the sender's close arrives. Previously the destination's whole gateway connection was closed, dropping every session, signaling, and health report. A receiver that stops reading is still disconnected by its writer timeout. Nodes retry the gateway every 0.5–1.5 seconds, jittered so they do not return in lockstep, then back off exponentially to 30 seconds after ten consecutive failures, logging persistent failures as warnings. Backing off from the first failure left clients asleep for seconds after a restarted gateway returned. Superuser `/v1/admin/metrics` exposes aggregate counters without fleet details. This preserves [D016](#d016-sqlite-backed-users-and-isolated-fleets)'s commit-before-acknowledgement and fleet scoping.

## D056: Signed registration bytes and tolerant health reports

Registration signatures covered the gateway's re-encoding of the node record, so any field an older gateway did not know made verification fail, and each new field needed its own `/v1/auth` negotiation. Gateways that advertise `nodeBytes` accept `Hello.nodeBytes`, the exact signed registration, with a domain-separated message, verify it before decoding, and ignore unknown fields. Older gateways keep the original encoding and per-field negotiation, which nodes still perform. Fields the gateway must store or publish still require that gateway to know them.

Health reports previously rejected unknown fields, so a node adding any system metric would be disconnected by an older gateway every five seconds. Gateways now decode only known fields and retain nothing else, which still keeps operation records, identities, and arguments out of reports. This extends [D022](#d022-owner-visible-machine-health-without-a-local-node).

## D057: Blocking task waits and acceptance outside the node lock

`tasks.get` accepts optional `waitSeconds` and returns when the task is terminal, after at most 60 seconds, or one second before the request deadline. Older nodes ignore the parameter and answer at once, so callers keep their poll interval whenever a call returns early; no negotiation is needed. Delegated task grants already match `tasks.get` by ID. Waiting requests use the interactive lane, keeping control-lane stream budgets for short RPCs.

Task records were written and fsynced while holding the node mutex, and every subprocess log write took that mutex to wake followers. Reserve the task ID in memory, write the record outside the mutex, and publish it afterwards; duplicate submissions wait for the reservation, the queue bound counts reservations, and lease acquisition and release treat reserved tasks as active. Shutdown during acceptance removes the unacknowledged record. Later state changes are written by the task's own goroutine before being published. Per-task notifiers wake log followers and waiters without the node mutex. Health, activity, and lease checks use the active-task set instead of scanning history. This extends D031's notification-driven log following.

## D058: Terminal task retention with tombstones

Task records, logs, and workspaces accumulated forever; startup loaded them all and listing copied them all. Remove terminal tasks after `taskRetentionHours` (default 168) and beyond `maxRetainedTasks` (default 10,000), checking at startup and every ten minutes. Queued, running, and reserved tasks, and tasks named by live delegations, are kept. Artifacts are not pruned.

Pruning must not weaken idempotent submission, so a durable tombstone with the ID, owner, specification digest, final state, and timestamps is appended and fsynced before the record is removed. Resubmitting a pruned ID with the same owner and specification returns the tombstone as a terminal `pruned` task without executing; other owners or specifications are rejected. Owners can still reconcile the outcome, but not the result, artifacts list, or logs. Tombstones are kept for 180 days and at most 50,000 per node; reusing an ID after that is a new submission. Workspace removal goes through the filesystem root so links cannot redirect it.

## D059: Artifact owner holds

Any authorized caller could list every artifact on a node and delete content another owner's task still needed. Record the task owners holding each content-addressed artifact in its local metadata, not in artifact references. Listing returns the caller's holdings and shared records; deletion releases only the caller's hold and removes content when no other owner holds it. The node's own identity keeps full visibility. Records written before this change, and records exceeding 256 owners, are shared and keep the previous behavior. Streaming access by grant or authorization is unchanged, since content IDs are already unguessable digests.

## D060: Bounded parallel workflows and load-preferring selection

Workflows accept optional `maxParallel` from 1 to 16, defaulting to 1 so existing workflows keep their sequential order. Ready steps launch in the same dependency order as before, up to the limit. After a failure no further step starts, but steps already running finish rather than being cancelled mid-effect; the result includes every started step. Each worker still admits its step through its own slots, leases, and maintenance gate.

Selection preferred the first match by name, sending every selected task to one machine. It now prefers the match with the lowest `activeCount` in owner-visible health summaries and chooses randomly among ties or when summaries are unavailable, as for common keys. This is a placement preference, not a reservation; destination admission is unchanged. Resource-based placement remains future work. This supersedes [D010](#d010-destination-owned-leases-and-a-small-workflow-executor)'s sequential execution and first-match selection.

## D061: Completed-rollout binary retention and selected-release downloads

Gateway update uploads and staged binaries accumulated across every development push. Prune obsolete checksum-named entries only after the gateway and online participants acknowledge the selected software and completion is durably recorded. Repeat every minute while complete, including after restart, so expired invitation pins and abandoned uploads are eventually removed. Failed or unfinished rollouts keep their files. Cleanup failure does not turn a successful update into a failed deployment.

Retain selected-manifest assets for offline nodes, assets pinned by live installation invitations, and current/previous/requested gateway runtimes for local recovery. Pin active repository uploads and release downloads, and give unpublished uploads one hour to finish publication. Validate invitation binaries again under the same lock as pruning before committing a new invitation. Unreadable runtime records fail closed, and rooted removal cannot follow links outside update storage.

Serve general authenticated binary downloads only from the selected manifest. Pinned installation links retain their existing download authority until redemption, revocation, or expiry; recovery binaries are not generally downloadable. This replaces D014's indefinite gateway storage retention without changing superuser publication, idle reservations, rollout acknowledgements, node-local retention, or supervisor recovery. See [gateway binary retention](updates.md#gateway-binary-retention).
