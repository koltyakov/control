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

Upload and clipboard admission is tightened by [D131](#d131-lease-sensitive-upload-and-clipboard-admission). The artifact/TCP exemptions below and leased-task lifetime rules remain unchanged.

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

The resident supervisor is superseded by [D132](#d132-single-process-services-and-temporary-update-handoffs). Update authority, verification and idle admission remain unchanged.

The nodes-first order and indefinite disconnected-participant wait are superseded by [D096](#d096-gateway-first-updates-with-deferred-offline-participants). Idle admission and binary verification remain mandatory.

Indefinite node-local binary and previous-selection retention is superseded by [D065](#d065-remove-obsolete-node-binaries-after-successful-startup). Gateway recovery retention remains unchanged.

Same-version update selection and acknowledgements are refined by [D047](#d047-skip-updates-for-unchanged-versions). Authorization and idle reservations remain unchanged.

Indefinite gateway binary retention is superseded by [D061](#d061-completed-rollout-binary-retention-and-selected-release-downloads). Supervisor recovery and node-local storage remain unchanged.

Extend D004's enrollment model with hashed, revocable common keys and a separately configured superuser key. Only the superuser can issue common keys, upload executable bundles, publish deployments, or trigger release checks. CLI administration help requires a verified superuser response; server-side authorization enforces the permission independently.

[D016](#d016-sqlite-backed-users-and-isolated-fleets) supersedes the common-key issuance restriction: account owners can issue keys for their own fleet. Executable publication and global rollout administration remain superuser-only.

The gateway can fetch stable GitHub releases from a configured repository or accept development bundles built by `make update`. Each manifest binds platform-specific executables to size and SHA-256. Nodes accept update commands only from the gateway, stage and validate their own binary, and acknowledge a local idle reservation before applying it. Track outgoing work, accepted task lifetimes, transfers, tunnels, and leases so coordination and peer-to-peer traffic also delay maintenance.

Persist installation participants and reservations across service restarts. Update nodes first and wait for their version acknowledgments, then restart the gateway. Expiring reservations release admissions if coordination is lost. Offline nodes catch up on reconnection. This introduces short maintenance pauses and lets a busy pool postpone an update indefinitely.

Use a stable supervisor and versioned executables in the state directory rather than replacing running Windows binaries. The original binary remains the launcher, and standalone CLI/MCP installations retain their own version. [D025](#d025-local-cli-self-updates) adds explicit updates for that installation. Selection is durable, shutdown is graceful, and the previous selection is retained for manual recovery. Startup failure does not automatically replay work or roll back state. See [managed updates](updates.md).

## D015: Single-machine invitations and per-user host setup

Installation ticket length is refined by [D090](#d090-short-one-time-installation-tickets). Permanent credentials and signed redemption remain unchanged.

The pending-name reservation guard is extended by [D074](#d074-explicit-atomic-retraction-of-pending-name-invitations), which allows explicit retraction while preserving enrolled identities.

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

[D194](#d194-opt-in-local-vault-for-cli-and-mcp-authority) supersedes automatic credential loading for Vault frontends.

`control login` validates an API key against the selected gateway and saves both in the existing private `admin.json` profile. Later gateway commands load that credential automatically. Explicit credentials override saved ones; saved keys are never reused for another gateway URL. Failed validation leaves the previous login intact. This follows expose's saved server/key model while adding validation before saving.

Directory listing, invitations, and gateway observation need no local node. `setup` reuses the login to issue a separate node credential and start the host for peer execution. This extends D015 and D018 without putting account keys in node configuration or MCP entries. See [installation](installation.md).

## D020: Target-selected installation architecture

Default invitations select an OS and pin its amd64 and arm64 assets from one published manifest. Bash or PowerShell detects the target architecture, downloads that candidate, and verifies its pinned checksum. Signed redemption includes the chosen architecture and persists it with the identity and credential. Recovery cannot change that binding. Existing explicitly pinned invitations keep their original proof format.

This removes architecture and lifetime questions from the dashboard wizard while retaining reproducible downloads and platform-bound registration. It requires both architecture binaries in the published bundle and compatible gateway/installer versions. The wizard uses the shared invitation client and native clipboard tools, displays the command on copy failure, and never retries an uncertain invitation request automatically. See [installation](installation.md) and [dashboard](dashboard.md).

## D021: Automatic Windows node service

The SCM-hosted supervisor is superseded by [D132](#d132-single-process-services-and-temporary-update-handoffs), which directly hosts the stable runtime.

The Windows `user`-mode alias and exclusive system-service startup choice are superseded by [D033](#d033-explicit-windows-user-login-startup). Automatic mode retains this decision.

Install Windows nodes as automatic Service Control Manager services instead of per-user Run entries. Each absolute profile path identifies one service. The service wraps the existing versioned supervisor, starts before login, handles stop/shutdown through context cancellation, and uses SCM recovery after failures. Background child processes suppress console creation, including executable validation and provider commands.

Installation requires Administrator PowerShell and checks elevation before issuing setup credentials or redeeming an invitation. Automatic mode never falls back to a detached process on Windows. LocalService runs the node with explicit access to its runtime files rather than LocalSystem privileges or an interactive user's credentials. Providers therefore need service-account paths and credentials. An explicit process mode remains available for development. Existing profiles and identities survive migration; service start stops the old supervisor and removes its matching login entry. Linux and macOS retain per-user startup. See [installation](installation.md).

## D022: Owner-visible machine health without a local node

Nodes publish aggregate active-work count, lease presence, and cached system metrics over their existing authenticated gateway connection every five seconds. The gateway retains one report per connection, expires it after 20 seconds, and returns it only to that fleet's account key. The superuser sees reports only for its legacy fleet. Common keys still require authorized peer observation.

This lets a logged-in dashboard show machine CPU, idle/busy state, and work counts without a local node. Reports contain no operation records, owner IDs, arguments, credentials, or private filesystem paths. Task control and detailed activity remain on the peer API. Sampling runs independently of reporting and dashboard polling. Feature negotiation through `/v1/auth` prevents reports from disconnecting older gateways during nodes-first updates. This extends D018 and preserves fleet isolation and task ownership. See [dashboard](dashboard.md) and [protocol](protocol.md).

## D023: Durable owner-controlled machine lifecycle

Startup retry behavior is extended by [D062](#d062-retry-transient-gateway-failures-during-node-startup), without bypassing the initial policy check.

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

The fixed client-count limit is superseded by [D101](#d101-bounded-gateway-admission-and-aggregate-packet-retention), which retains 64 as the configurable default and adds aggregate admission. Role and identity boundaries remain unchanged.

The one-connection-per-owner and process-long file lock are superseded by [D030](#d030-concurrent-client-connections-with-stable-task-owners). Fleet separation remains unchanged.

The orchestrator's CLI/MCP is a requesting client, not a fleet execution agent. Replace D028's machine registration with a separate authenticated client-session endpoint and role-bound challenge proof. Persist only immutable account ownership and client role; keep connection metadata transient. Client sessions do not create fleet entries, dashboard rows, health samples, lifecycle policy, update-platform requirements, or rollout participants.

Workers authenticate live client identities through an account-scoped peer lookup, then enforce the same pinned TLS, access rules, task ownership, and artifact grants as other peer callers. Fleet machine discovery and selection never include clients. Credentials bound to an installed machine cannot open client sessions, and client identities cannot later enroll as machines. Limit each account to 64 active client sessions. Existing one-connection-per-identity and local file-lock behavior remain unchanged.

This contract requires gateway and worker support. Advertise `clientSessions` in authenticated gateway metadata and compatible machine records; reject unsupported standalone execution before work is sent. Preserve old machine registration proofs when connected to an older gateway. Schema version 3 stores client roles independently of nodes. Gateway startup moves only the previous implementation's exact unbound, capability-free CLI registrations to client identities, retaining their account and task-owner identity. No retired identity is restored. See [standalone usage](installation.md#standalone-cli-and-mcp) and [database upgrades](users.md#persistence-and-upgrades).

## D030: Concurrent client connections with stable task owners

The full-close forwarding and single outbound-session transport are extended by [D031](#d031-isolated-traffic-lanes-shared-webrtc-carriers-and-duplex-streams). [D042](#d042-process-owned-reverse-tcp-forwarding) adds reverse listeners. Stable ownership and client/fleet separation remain unchanged.

A long-lived tunnel or MCP process must not block another CLI process from querying logs or cancelling a task. Retain the gateway/account-scoped persistent client key as the task and lease owner, but generate a separate TLS identity per requesting process. Hold the local owner-key lock only during key loading or creation. The owner signs a purpose-separated challenge proof authorizing that transport identity and metadata. The transport identity also signs the existing client-session proof. An account credential alone cannot impersonate another task owner.

Persist immutable transport-to-owner bindings in SQLite schema version 4 before connection acknowledgement. Workers derive stable ownership exclusively from authenticated peer metadata. Access rules may match the owner ID or its existing derived client name; TLS pinning and artifact grants remain bound to the transport recipient. Machine identities cannot become client owners, and client owners or transport identities cannot enroll as machines. Discovery, health, updates, and the 64-live-client limit remain as in D029. Negotiate `clientOwners` on both gateway and target before sending work, retaining old single-identity client compatibility on upgraded servers.

Keep TCP forwards process-owned in the shared client, with loopback defaults, at most 32 listeners and 128 active connections per listener. MCP start returns promptly and survives tool-call completion. Stop and process shutdown close listeners and sockets and join forwarding goroutines. Forwarded TCP remains full-close on either direction ending; half-close, UDP, reverse listeners, PTYs, and detached local forwarding services are not implemented in this decision. [D192](#d192-opt-in-interactive-shell-streams) adds interactive PTYs separately from TCP forwarding. Durable tasks provide long-running execution independently of client lifetimes, with CLI detached submission, timeout selection, and offset-based log following. Never reconnect and replay an existing TCP socket or uncertain task submission. This supersedes D028/D029's concurrency restriction, without turning an orchestrator into a fleet worker.

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

The persistent startup wrapper is superseded by [D132](#d132-single-process-services-and-temporary-update-handoffs). User-token and profile-ownership rules remain unchanged.

The invitation-script default and its reuse of saved startup mode are superseded by [D038](#d038-user-login-startup-by-default-in-windows-invitation-scripts). Direct setup and service commands retain this decision.

The PowerShell task wrapper is superseded by [D063](#d063-console-free-windows-node-login-bootstrap). User context, ownership, and recovery rules remain unchanged.

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

Superseded by [D126](#d126-retire-the-rpa-adapter-in-favor-of-agent-desktop). The RPA adapter and Python helper are removed; native desktop retains the shared lock and admission contracts.

Expose GUI automation as `rpa.run` only when a node explicitly configures an `rpa` command. Keep OS libraries in a version-1 subprocess helper so Control retains CGO-free builds and providers remain independent of transport. The supplied Python helper uses native accessibility targeting on Windows, macOS, and Linux, with explicit PyAutoGUI coordinate/keyboard actions. Linux coordinate input initially requires X11, and execution requires a logged-in desktop and its OS permissions.

Validate the whole batch before execution, cap it at 100 actions and two minutes, and serialize it with a shared file lock under the OS user's Control configuration directory. Native selector actions must resolve exactly one element in a complete bounded scan and must not silently fall back to coordinates. Import screenshots as bounded PNG artifacts from an invocation-owned temporary directory. Retain existing fleet authorization, tracked-task ownership and idempotency, leases, and update admission.

GUI batches are not atomic transactions. Earlier actions and even a failed action may have changed an application. Stop at the first error and retain partial results in tracked tasks; never replay automatically after failure, disconnect, or restart. A node lease coordinates multi-batch callers on that node, but neither it nor the desktop lock blocks humans or unrelated automation. External helper dependencies and permissions need native runtime verification. See [GUI automation](rpa.md).

## D041: Worker-local secret references for credential entry

The GUI/HTTP-only resolution restriction is superseded by [D145](#d145-worker-local-command-secret-injection), adding invocation-local command environment and stdin references without value reads.

The RPA `setValue`/`type` path is removed by [D126](#d126-retire-the-rpa-adapter-in-favor-of-agent-desktop). Native desktop value setting and HTTP references remain supported.

The local-only management restriction is superseded by [D116](#d116-encrypted-owner-managed-worker-secrets). Value-read prohibitions, local storage and reference-only execution remain unchanged.

The native agent-desktop secret-reference path extends this decision in [D102](#d102-native-agent-desktop-through-a-private-pipe).

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

<a id="d043-owner-selected-machine-routing-aliases"></a>

## D043a: Owner-selected machine routing aliases

This entry originally shared D043 with the startup-context decision. The suffix disambiguates it without renumbering later decisions; the original heading anchor remains available.

The rejection of pending destination-name invitations is superseded for explicit force requests by [D074](#d074-explicit-atomic-retraction-of-pending-name-invitations). Registered and redeemed identity reservations remain unchanged.

Permit fleet owners to rename online and offline registrations through the shared client, CLI, and dashboard, without restarting a node or replacing its credential. Store an optional routing-name override with the gateway's existing identity policy and commit it together with the directory name. Apply it after signed identity and fleet checks on reconnection. Preserve admission revisions, work, identity ownership, and original signed installation proof names. Use the override for enrollment reservations and bound-credential authorization; fresh replacement enrollment clears it transactionally.

Names obey existing enrollment syntax and fleet uniqueness, invitation reservations, and client-name reservations. Notify only connected peers in that fleet with a gateway-only `directory.changed` packet. Compatible peers clear name caches without closing identity-bound sessions, and prevent lookups started before notification from reintroducing stale aliases. Reconnection clears caches when notifications were missed. Older peers can retain cached aliases until restarted. Name-based access rules and scripts need updating; stable IDs do not. This extends D023's owner controls and D026's replacement enrollment without changing their stop or credential-rotation contracts. See [registration management](installation.md#manage-registrations).

## D044: Explicit clipboard pastes with streamed files

Expose bidirectional clipboard transfer as an explicit CLI/MCP paste, not a background watcher or a native desktop paste hook. Keep source desktop access in platform-specific clipboard helpers and shared orchestration in `internal/client`. Text replaces the destination OS clipboard; file references become regular-file pastes into an existing directory. Standalone clients use the existing authenticated peer transport without enrollment or inbound execution handlers.

Use separate `clipboard.open` and `clipboard.paste` permissions, with fleet checks, bounded bulk-lane streams, and maintenance admission. A source reads only OS-selected file references, never paths supplied by the caller. Send basenames and sizes first, wait for destination acceptance, then stream file bytes and SHA-256 trailers. Keep bytes out of control frames and avoid artifact staging or whole-file buffering. Receivers confine remote paths to `workDir` and publish verified files atomically without replacing existing entries.

Clipboard transfer exposes the desktop user's data, including selected files outside the workspace, and is not secret-masked. Default trusted-fleet rules still apply; sensitive hosts should restrict the new permissions. Native clipboard access requires that user's desktop session. File transfers need hard-link support, support regular files only, and do not delete cut sources. Cancellation removes unfinished temporary files, but completed files and uncertain text side effects require explicit reconciliation. Never resume or replay automatically. This extends D005's streamed operations and D039's local clipboard helpers without changing gateway routing or durable task semantics. See [clipboard behavior and limits](clipboard.md).

## D045: Account-client authority and instruction-bound worker delegation

[D069](#d069-enforce-authority-on-local-coordination-artifact-deadlines-and-mcp-discovery) tightens local coordination, absolute artifact expiry, and executable MCP discovery boundaries.

Authenticate execution initiation separately from fleet membership. The gateway assigns authority to account-authenticated client sessions, not enrolled nodes or common-key clients. Default CLI/MCP routing always uses this client identity; explicitly selecting a local API retains local execution and discovery only. Nodes remain symmetric runtimes and can coordinate delegated operations without gaining ambient peer authority.

Let destinations issue subject- and owner-bound grants for exact instructions. CLI/MCP prepares grants before instructing a worker through `peers.call`, workflows, or artifact delivery. Task grants include one explicit task's lifecycle; dynamic workflow inputs require signed upstream-task/output provenance. Do not put authority in worker-wide access rules, provider environments, or persisted task specifications. Keep child tasks under the original client owner. Single-use synchronous instructions prevent replaying side effects under a still-live grant.

Keep grant records in bounded destination-owned memory. Expire them after an hour idle, keep accepted tasks active, and renew streams only on actual traffic. Revocation cancels admitted tasks and streams, including derived artifact access. Restart loses grants and marks unfinished tasks interrupted rather than restoring authority. This trades grant continuity across restart for fail-closed recovery and needs no gateway scheduling or database migration. Account clients need the live gateway for new undelegated authority checks; issued grants remain destination-owned. Revocation requires contacting each destination and cannot undo effects.

Negotiate support and refuse older targets rather than silently using their old trust rules. Upgrade the gateway and every node before relying on worker isolation. Keep account credentials off workers; unrestricted providers and fully compromised hosts remain outside an execution sandbox. This supersedes unrestricted peer initiation in D001/D004, default local-node routing in D002/D028, and D008's stateless artifact-grant revocation limitation. See [delegation](delegation.md).

## D046: Windows firewall rules and a stable verified runtime path

Supervisor-owned runtime publication is superseded by [D132](#d132-single-process-services-and-temporary-update-handoffs). The stable firewall path remains unchanged.

Retention of obsolete node source binaries and rollback records is superseded by [D065](#d065-remove-obsolete-node-binaries-after-successful-startup). The stable execution path and current versioned source remain unchanged.

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

Keep this host login's unexpired persistent definitions in dashboard `retainedTunnels` and a separate lower-priority `P.Tun.` column, meaning permanent tunnels. Retrying definitions are not live connections, and foreground or detached tunnel processes have no persistent definitions. Mixing the two made established listeners display zero. Idle local forward listeners are not observable on workers, so forward activity counts describe connected sockets, not local listener definitions. This supersedes D051's dashboard field and column naming without changing tunnel lifetimes, restoration, authorization, gateway health reports, or telemetry privacy. See [dashboard counts](dashboard.md).

## D053: Full carrier packets and negotiated packet size

The complete-SDP-only signaling choice is superseded by [D068](#d068-trickle-ice-deployment-stun-defaults-and-non-disruptive-direct-re-probes) for negotiated peers. Legacy gathering bounds remain unchanged.

yamux writes each frame header separately from its body, TLS seals every write in its own records, and the packet adapter split a full 16,406-byte TLS record into 16 KiB plus a 22-byte tail. An 8 MiB transfer produced 1,299 carrier messages, 782 under 128 bytes, and each relayed message is a protobuf packet through the gateway. Hold the packet adapter's output while one multiplexer frame is sealed, then send it in full packets; write duplex records and control frames in one call; join nested reverse-listener frame headers to their bodies. This needs no wire change because packets carry an arbitrary slice of the encrypted stream.

Every current receiver accepts 64 KiB packets with an 8 MiB per-link receive bound. Senders use 64 KiB only toward peers whose signed directory record advertises `largePackets`, negotiated through `/v1/auth` like `peerChannels`; older receivers reject larger messages and keep 16 KiB. Raise bulk caller windows to 4 MiB, interactive caller and accepted-session windows to 1 MiB, and the SCTP receive buffer to 4 MiB, raising per-stream and per-carrier ceilings on high-latency paths with bounded memory. Cap STUN candidate gathering at two seconds, or two fifths of the direct timeout, because Pion's five-second default equalled the direct timeout and one unreachable STUN server forced every session to the relay. Trickle ICE remains unimplemented: new signaling kinds would disconnect senders on older gateways.

Receivers bind an added lane's data channel inside Pion's `OnDataChannel` callback, which Pion completes before it delivers that channel's messages; only the TLS handshake runs asynchronously. Binding it in a goroutine could drop the dialer's first TLS record, stalling the lane until the direct timeout and pushing it onto the relay.

Hard-linking task inputs was considered and rejected. A provider can modify an input in place, which would alter the content-addressed store; inputs remain copies. This extends [D031](#d031-isolated-traffic-lanes-shared-webrtc-carriers-and-duplex-streams) without changing authentication, lanes, or replay rules. See [fleet streaming](streaming.md).

## D054: Cached identity lookups with offline eviction

Workers authenticated every non-delegated request with a gateway `/v1/peers/{id}` lookup, sometimes two or three per request, and delegated calls resolved names by downloading the whole directory. Polling task waits multiplied this into tens of thousands of gateway requests per long task, and established direct sessions could not run account-client work during a gateway outage. Reuse successful lookups of online records for up to 15 seconds. Evict an identity on the gateway's existing `offline` notification and clear everything on `directory.changed` or reconnection; never cache offline records, so a returning peer is never reported offline. A directory read populates name and ID entries for every listed machine.

A disconnected client's execution authority now ends at its offline notification, or within 15 seconds if that best-effort packet is lost. Account revocation already closed its gateway connection and so produces the notification. Membership entries for identities without links or sessions are forgotten on offline notifications and, beyond 1,024 entries, after ten idle minutes, bounding memory for per-process client transport identities. This refines D031's stable-ID lookup without changing who may execute work.

## D055: Incremental gateway commits and separate hot paths

The single destination queue is refined by [D072](#d072-prioritized-hot-path-performance-work) and [D073](#d073-committed-observation-views-and-safe-performance-boundaries), which separate bounded signaling/control and relay queues while preserving data/close/offline order. D073 also moves list/status readers to a separate committed observation view.

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

Journal validation and cleanup retries are refined by [D075](#d075-bounded-task-data-and-durable-cleanup).

Task records, logs, and workspaces accumulated forever; startup loaded them all and listing copied them all. Remove terminal tasks after `taskRetentionHours` (default 168) and beyond `maxRetainedTasks` (default 10,000), checking at startup and every ten minutes. Queued, running, and reserved tasks, and tasks named by live delegations, are kept. Artifacts are not pruned.

Pruning must not weaken idempotent submission, so a durable tombstone with the ID, owner, specification digest, final state, and timestamps is appended and fsynced before the record is removed. Resubmitting a pruned ID with the same owner and specification returns the original terminal state with `pruned: true` without executing; other owners or specifications are rejected. Owners can still reconcile the outcome, but not the result, artifacts list, or logs. Startup discards tombstones older than 180 days, and compaction keeps at most 50,000 per node; reusing an ID after its tombstone is removed is a new submission. Workspace removal goes through the filesystem root so links cannot redirect it.

## D059: Artifact owner holds

Owner overflow and deletion protection are superseded by [D076](#d076-artifact-budgets-and-reference-protection).

Any authorized caller could list every artifact on a node and delete content another owner's task still needed. Record the task owners holding each content-addressed artifact in its local metadata, not in artifact references. Listing returns the caller's holdings and shared records; deletion releases only the caller's hold and removes content when no other owner holds it. The node's own identity keeps full visibility. Records written before this change, and records exceeding 256 owners, are shared and keep the previous behavior. Streaming access by grant or authorization is unchanged, since content IDs are already unguessable digests.

## D060: Bounded parallel workflows and load-preferring selection

Parallel-ready ordering is superseded by [D125](#d125-bounded-capacity-aware-workflow-ready-ordering). Sequential order, dependency/failure rules and destination admission remain unchanged.

Placement and explicit recovery are extended by [D078](#d078-resource-placement-explicit-reconciliation-and-release-signatures).

Workflows accept optional `maxParallel` from 1 to 16, defaulting to 1 so existing workflows keep their sequential order. Ready steps launch in the same dependency order as before, up to the limit. After a failure no further step starts, but steps already running finish rather than being cancelled mid-effect; the result includes every started step. Each worker still admits its step through its own slots, leases, and maintenance gate.

Selection preferred the first match by name, sending every selected task to one machine. It now prefers the match with the lowest `activeCount` in owner-visible health summaries and chooses randomly among ties or when summaries are unavailable, as for common keys. This is a placement preference, not a reservation; destination admission is unchanged. Resource-based placement remains future work. This supersedes [D010](#d010-destination-owned-leases-and-a-small-workflow-executor)'s sequential execution and first-match selection.

## D061: Completed-rollout binary retention and selected-release downloads

Verification under the pruning lock is superseded by [D070](#d070-bounded-recent-activity-and-pinned-binary-verification-outside-shared-locks). Temporary pins now protect assets through verification and durable reference publication without holding the repository or gateway mutex during hashing.

Gateway update uploads and staged binaries accumulated across every development push. Prune obsolete checksum-named entries only after the gateway and online participants acknowledge the selected software and completion is durably recorded. Repeat every minute while complete, including after restart, so expired invitation pins and abandoned uploads are eventually removed. Failed or unfinished rollouts keep their files. Cleanup failure does not turn a successful update into a failed deployment.

Retain selected-manifest assets for offline nodes, assets pinned by live installation invitations, and current/previous/requested gateway runtimes for local recovery. Pin active repository uploads and release downloads, and give unpublished uploads one hour to finish publication. Validate invitation binaries again under the same lock as pruning before committing a new invitation. Unreadable runtime records fail closed, and rooted removal cannot follow links outside update storage.

Serve general authenticated binary downloads only from the selected manifest. Pinned installation links retain their existing download authority until redemption, revocation, or expiry; recovery binaries are not generally downloadable. This replaces D014's indefinite gateway storage retention without changing superuser publication, idle reservations, rollout acknowledgements, node-local retention, or supervisor recovery. See [gateway binary retention](updates.md#gateway-binary-retention).

## D062: Retry transient gateway failures during node startup

A network delay at login could exhaust Windows Task Scheduler's finite restart attempts and leave a node offline until manually started. Keep the managed node process alive while its initial signed policy query or authenticated gateway connection fails transiently. Retry network errors, HTTP 408/429, and HTTP 5xx with jittered delays, backing off after ten failures and capping each delay at 30 seconds. Bound each request and stop the retry loop on node cancellation. Log the first failure, every tenth failure, and recovery rather than flooding the startup log.

Do not open execution before policy validation. Check cached retirement immediately, fetch policy before registration, and fetch it again after connecting because a long connection wait can make the earlier policy stale. Keep peer work admission blocked throughout startup, including after transport connection while policy is still being checked. Persist restrictions before acknowledging them. Authentication rejection, TLS validation, protocol errors, and local persistence failures still stop startup. Only connectivity and identity-bound policy reads are retried; accepted work, uncertain commands, and client submissions are never replayed.

Apply this in the shared node/transport implementation on all three platforms, not in a Windows launcher. Existing managed installations can receive the fix through a child update without replacing their startup registration. Command-scoped CLI/MCP clients retain fail-fast startup, and installation/service readiness commands retain their 30-second bound. A readiness timeout does not stop a background node's connectivity recovery or report installation success. This extends D003/D023/D033 while preserving retirement, fleet isolation, and task-recovery semantics. See [startup behavior](installation.md#files-and-startup).

## D063: Console-free Windows node login bootstrap

The persistent bootstrap is superseded by [D132](#d132-single-process-services-and-temporary-update-handoffs), which directly runs a verified GUI-derived runtime.

`powershell.exe -WindowStyle Hidden` does not prevent console allocation and can leave a blank Windows Terminal window at login. Make the node login task start a native GUI-subsystem bootstrap directly. Derive it locally from the installed Go CLI by changing only the PE Subsystem field, then publish a checksum-verified, content-addressed copy under the profile's state directory. No separate release asset, compiler, scripting host, or account password is required.

The bootstrap starts the unchanged installed CLI with `CREATE_NO_WINDOW`, waits for its supervisor, and captures output in `node.log`. Keeping the original CLI as the supervisor preserves runtime-selection validation, release checksums, firewall paths, and the installation's shared-launcher registry. Changing the bootstrap's filename when its content changes avoids overwriting a running executable. Old copies remain with retained runtime state. CLI invocations keep their normal console behavior, and automatic SCM startup remains unchanged.

Retain the login task's limited interactive token, exact user/profile ownership checks, restart policy, and single-instance setting. A new installed CLI and saved-mode stop/start refresh an older task; a managed child update cannot change its PowerShell action. Do not migrate to LocalService to suppress windows. This supersedes only D033's PowerShell wrapper and preserves its startup context and graceful service lifecycle. Native tests inspect console allocation and child exit/log behavior without installing tasks; Windows Terminal startup still requires a desktop smoke test. See [Windows startup and refresh](installation.md#files-and-startup).

## D064: On-demand authenticated peer connection tests

Measure Control's actual peer path rather than running an external internet speed test or staging diagnostic artifacts. Use one bulk-lane stream for warmed RTT echoes and sequential, checksum-verified synthetic traffic in each direction. Report its exact WebRTC/relay mode, setup time, RTT statistics, and per-direction application throughput. Bound payloads to 256 MiB per direction, samples to 100, total lifetime to two minutes, and active tests to two per node. Fixed byte counts bound bandwidth consumption even on fast hosts, but short tests may underestimate available capacity.

CLI and MCP share the client implementation. A standalone orchestrator needs no local node; a selected worker source runs `connection.test` using one exact `connection.open` destination grant prepared by that account client. Reject local API mode rather than mislabeling a node-to-worker measurement as the orchestrator's path. Keep receiver fleet membership, method permission, revocation, cancellation, and maintenance admission unchanged. Clean up diagnostic grants after the synchronous call without replay. No gateway wire change, ambient worker permission, nested delegated coordination, durable task capability, disk staging, or background sampling is introduced. These measurements include encryption, hashing, flow control, load, and relay limits, not raw link capacity. See [connection tests](connections.md).

## D065: Remove obsolete node binaries after successful startup

Nodes kept every downloaded executable indefinitely. Prune obsolete checksum-named directories in node update storage at successful startup, after update offers, and every minute. Serialize cleanup with the updater's command loop to protect downloads. Preserve the running software, current and requested runtime selections, and pending staged binary. Match both version and checksum to the persisted selection and verify its source before removing the previous selection and binary. This keeps recovery available for failed startup without keeping old binaries after successful updates.

Run cleanup in the managed child, after gateway connection and startup policy checks, so existing launchers receive it through an ordinary managed update. Keep the installed launcher and Windows stable launch copy plus its current versioned source. Do not prune bootstraps, CLI backups, or unrelated node data. Fail closed on unreadable runtime records, skip symlinks, confine removal to update storage, and log/retry cleanup errors without failing a successful update.

This supersedes D014/D046's node-local recovery retention after successful startup. Gateway retention under D061, idle reservations, persisted runtime selection, and the prohibition on automatic rollback remain unchanged. See [node binary retention](updates.md#node-binary-retention).

## D066: Project-local scenario migration on client renames

The naming and storage layout below are superseded by [D106](#d106-shared-and-machine-specific-runbooks-in-approved-stores). Rename durability and no-replay rules remain unchanged.

Extend the shared CLI/dashboard rename method to migrate project-local machine instruction folders in the invoking working directory and update supported Markdown/JSON routing references. Resolve old aliases through the authenticated fleet directory, including when the request uses a stable ID. Keep secret references and unrelated file content unchanged. Local alias-based folders belong to their project's selected fleet; do not search other projects or orchestrator hosts, synchronize through the gateway, or add an instruction execution provider.

Preflight under a local lock rejects destination collisions, symlinks, special files, malformed JSON, and bounded-tree violations before the remote mutation. Apply local changes only after gateway acknowledgment, using rooted filesystem operations and atomic text replacement. Preserve detected concurrent edits. The gateway and filesystem cannot commit together; report post-acknowledgment local failures explicitly and never replay or reverse the gateway rename automatically. A crash can require manual local reconciliation. This extends the owner-selected routing-alias decision without changing its gateway protocol, identity, admission, or enrollment contracts. See [current runbook scope and recovery](runbooks.md).

## D067: Network switch recovery and end-to-end gateway liveness

The raw interface/source-address sampling policy below is superseded by [D100](#d100-gateway-egress-fingerprints-ignore-ipv6-privacy-rotation). Recovery, generation checks, carrier grace, and heartbeat behavior remain unchanged.

VPN and interface changes can leave direct sessions cached until ICE or multiplexer timeouts. A stale gateway connection can also block a returning node with `identity already connected` while a proxy answers WebSocket pings. Sample active interface addresses and route-selected gateway source addresses every two seconds, using local UDP connects without sending packets. On a change, retire sessions, cancel setups, clear lookup caches, and wake gateway reconnect backoff. Generation checks prevent old setups from returning to caches. Sampling errors do not imply a switch. Changes that preserve all sampled addresses still rely on transport failure detection.

Exclude disconnected WebRTC carriers from reuse and inspection immediately, allowing three seconds for transient recovery before retirement. Rebuild connections on subsequent operations, never by replaying uncertain commands or sockets. Healthy direct sessions still survive gateway-only outages. Accepted task durability, owner checks, and persistent tunnel listener recovery remain unchanged. Keep connectivity sampling separate from hardware metrics.

Negotiate application-level gateway nonce challenges through an authenticated capability and signed handshake flag. Challenge every five seconds with a ten-second response deadline, without pausing relay traffic while waiting. A proxy answering WebSocket control pings cannot satisfy the challenge. Keep the duplicate-identity guard instead of replacing a live registration. Retain legacy proof bytes and WebSocket pings when negotiation is absent. Preserve fresh health as workload information while exposing individual peer-probe errors in the dashboard. This extends D003/D022/D031 without changing fleet membership or replay rules. See [connection lifecycle](architecture.md#connection-lifecycle) and [protocol details](protocol.md#node-operations).

## D068: Trickle ICE, deployment STUN defaults, and non-disruptive direct re-probes

Waiting for each peer's STUN gathering can consume four seconds of a five-second WebRTC establishment budget. Negotiate `trickleICE` through authenticated gateway metadata and signed peer records. Send SDP immediately and exchange bounded `ice` packets while connectivity checks run. Queue outgoing candidates until their SDP is sent, and incoming candidates for known pending setups until remote SDP is installed. Keep processing after data-channel establishment. Bound packets to 4 KiB and candidates to 128 per link; candidate workers are link-owned and joined on shutdown. Older gateways and endpoints retain complete-SDP signaling. This supersedes D053's no-trickle choice only for negotiated peers, without changing the routing envelope or pinned mutual TLS.

Publish operator-configured STUN URLs through authenticated `/v1/auth`, allowing nodes and standalone clients to share deployment defaults. Omitted or null peer `iceServers` inherits them; explicit empty or nonempty lists override them. No third-party provider is selected silently. Gateway defaults contain only STUN URLs, not shared TURN credentials. Session and speed-test route diagnostics distinguish direct, TURN, gateway relay, and unknown using candidate types without exposing addresses, SDP, or credentials. Historical transport identifiers and counters retain their meanings.

Share a 30-second failed-WebRTC backoff across lanes. Later operations can trigger a bounded background direct probe while continuing over their working relay; idle peers do not probe. A successful replacement affects only future streams and drains the old relay before closing it. Reuse connected shared carriers immediately and clear backoff on network changes. Last-waiter cancellation removes pending aliases before cleanup so a later caller cannot attach to a cancelled setup. These changes preserve fleet authorization, accepted-task durability, ownership, and the rule against replaying uncertain application effects. See [ICE configuration](protocol.md#node-configuration), [connection tests](connections.md), and [lifecycle](architecture.md#connection-lifecycle).

## D069: Enforce authority on local coordination, artifact deadlines, and MCP discovery

Require the same destination-owned instruction grant when remotely coordinated work targets the coordinating node itself. Check delegated instruction authority before granting the self identity local permission, and retain the original orchestrator owner when selecting local grants. This closes self-directed `peers.call` and local workflow execution through a coordinating-only `allow` rule. The trusted local API keeps local execution permission; shared CLI/MCP preparation already supplies self-target grants.

Give artifact-derived access a cancellation deadline equal to the signed token's absolute expiry. Check that deadline independently of idle renewal and active work, and report the earlier idle or absolute expiry. A recipient can resume using an access ID without gaining a longer lifetime by omitting the proof or continuing traffic. Parent revocation still cancels derived access, and expiry closes active transfers.

Treat all `mcp.discover` requests as execution instructions, including static server listing. Selecting a server can start a configured subprocess or open an application connection as the node's OS user. Require account authority or an exact delegation and the existing method permission before discovery. Worker discovery remains available for node and capability metadata. This deliberately gives the method one authority contract regardless of its parameters.

This refines D045 without changing the peer envelope, gateway persistence, grant limits, or accepted-task durability. Upgrade every execution node to enforce these corrections; existing delegation capability flags do not distinguish this patch from earlier implementations. See [delegation](delegation.md) and [protocol](protocol.md#instruction-bound-delegation).

## D070: Bounded recent activity and pinned binary verification outside shared locks

The shared reader/mutation lock and no-cache observation choices below are superseded by [D072](#d072-prioritized-hot-path-performance-work) and [D073](#d073-committed-observation-views-and-safe-performance-boundaries). Current list/status readers use a separate committed view, and automatic dashboard polls reuse peer detail for up to five seconds. Explicit refresh bypasses that reuse.

Activity polls with recent completions scanned every retained task and sorted all terminal metadata, even when only five recent records were requested. Maintain an index of the newest 64 terminal task metadata records instead. Update it with terminal state publication, reconstruct it at startup, and refill it after pruning. Poll active tasks separately through the active-task set. Merge task and non-task completions by finish time with deterministic ID ties. Arguments, results, and logs never enter this index, and owner-scoped task control is unchanged. See [dashboard retention](dashboard.md#retention-and-limits) and [performance benchmarks](testing.md#performance-benchmarks).

Publication and invitation creation hashed executable assets under mutexes used by observation and fleet management. Pin selected assets before full verification, hash them outside those mutexes, and retain the pins until their durable reference commits. Cleanup can win before pin acquisition, in which case verification fails rather than publishing an absent binary. Once pinned, an asset cannot be pruned before publication. Duplicate references within a verification pass are hashed once. Only verified uploads replace checksum-named blobs; pins do not permit arbitrary byte replacement or skip executable validation. Gateway status also reads one selected-deployment snapshot before taking its directory lock.

This refines D055 and D061 without relaxing full synchronous SQLite commits, commit-before-acknowledgement, invitation lifetime, or checksum validation. Gateway database commits and cleanup still hold the gateway mutex; separating durable mutation ordering from reader locks remains future work. No snapshot TTL, new observation permission, or protocol change is introduced.

## D071: Route-aware dashboard connection counts

Split dashboard `D/R` into `D/T/R` so TURN paths no longer appear direct. Add optional `connectionRoutes` to authorized activity snapshots rather than changing historical `directSessions` WebRTC totals or transport identifiers. Classify usable sessions using existing selected-candidate metadata and count each peer once across lanes, preferring direct, TURN, unknown WebRTC, then gateway relay. This keeps draining fallback sessions from inflating counts after a successful re-probe.

Older snapshots with WebRTC peers and unknown ICE paths show an unavailable direct/TURN split, not zero TURN usage. Confirmed counts remain in JSON alongside the unknown count. No new network probes, gateway health fields, observation permissions, addresses, or credentials are introduced. This extends D068's route diagnostics without changing connectivity or execution behavior. See [dashboard counts](dashboard.md) and [protocol fields](protocol.md#orchestrator-sessions-and-forwards).

## D072: Prioritized hot-path performance work

The metadata-based executable verification cache and local result assignment proposed here are superseded by [D073](#d073-committed-observation-views-and-safe-performance-boundaries). Verification must check actual bytes, and local results must retain independent ownership. Relay close/offline ordering and admission waits are also refined by D073.

Write node metadata records as compact JSON; they are machine-read only, so indentation only added bytes to every synced task, lease, and artifact write. Atomic rename and file/directory sync are unchanged, as are full synchronous SQLite commits and commit-before-acknowledgement at the gateway. A gateway reconnect that changes no enrollment record (identity ownership, client bindings, or registration fields) skips its commit and only updates volatile presence in memory; disconnects still commit offline state and last-seen time. ([D166](#d166-batched-disconnect-last-seen-persistence) later batches those disconnect writes.)

Split queued relay data from signaling and control at both relay writers. Both queues remain bounded. Prioritization reduces waiting behind queued data, but cannot preempt an in-progress WebSocket write or provide strict bandwidth QoS. Nodes report changed update status immediately and unchanged status every five to six seconds instead of every two seconds. The gateway throttles repeated rollout commands to at most once per five seconds per node and kind, skips acknowledged staging/preparation and unnecessary completed-rollout resumes, and retries failed enqueue/report attempts without marking them delivered. Throttle records are scoped to the deployment and current participants. The gateway's eight-second status freshness window is unchanged.

The dashboard reuses a snapshot render between update and view passes within the same second. Local node calls marshal their parameters once, but retain JSON result conversion. Lease writes run outside the node mutex, serialized on their own lock. Artifact imports, pulls, and deletes use the artifact's per-ID transfer lock rather than holding the global artifact mutex through I/O. Dashboard peer observations are reused for up to five seconds per query and credential scope while gateway status is fetched on every poll; direct activity calls are never cached. Concurrent refreshes coalesce, cancellation stops waiting, and cached results are decoded into independent storage. Newer gateway health takes precedence over cached counts and resource samples. Relay sends marshal into recycled buffers, receivers take ownership of decoded protobuf payloads, and sends read the gateway socket from an atomic mirror rather than taking the session mutex per chunk. See [performance measurements](testing.md#performance-benchmarks) for measured benefits and limits.

## D073: Committed observation views and safe performance boundaries

The oversized WebSocket scratch-buffer retention timing below is superseded by [D144](#d144-conditional-directory-reads-and-bounded-relay-reuse). Other durability, ordering and ownership constraints remain unchanged.

Keep gateway mutation validation, SQLite transactions, rollback, and credential publication serialized under the existing mutation mutex with `synchronous=FULL`. Publish changed machine records to a separate fleet-indexed observation view only after successful commits. Volatile presence, health, update status, and policy acknowledgements update the same view under a short independent lock. List/status readers copy this view without waiting for fsync, and fleet name ordering changes only when membership or names change. Reads during a pending mutation may return the previous committed state, never its uncommitted fields. This refines [D055](#d055-incremental-gateway-commits-and-separate-hot-paths) without changing acknowledgement durability or fleet ownership. Persistent mutations still serialize; group commit is not implemented.

Relay data, close, and offline packets retain FIFO order so a prioritized close cannot discard earlier bytes. Signaling/control get bounded priority bursts, leaving opportunities for bulk traffic and liveness checks. An in-progress write retains the shared socket's lifetime and timeout. Concurrent relay producers reserve payload bytes atomically before enqueue and release reservations on failure.

Lease-sensitive admission waits are cancellable and return with the node mutex held, closing the gap between persistence and the admission check. Maintenance treats a pending lease write as busy rather than blocking on storage. Local dispatch retains JSON result conversion because a matching Go struct type can still contain mutable task arguments, maps, or pointers into node state.

Executable verification hashes actual bytes every time. Size and modification time can be preserved after a content change, so they cannot authorize cached verification. Existing pinned-asset deduplication within one verification pass remains valid. File and parent-directory sync protect different crash-recovery steps and are both retained. This supersedes the unsafe verification-cache and direct-assignment parts of D072. Regression tests cover preserved-mtime corruption, lease ordering/cancellation, relay ordering/byte bounds, failed report retries, and committed reads during mutations.

Allocation profiles of the 16 MiB artifact benchmark identified growing WebSocket message buffers as the largest allocation source. Gateway and peer readers now reuse borrowed buffers, retaining at most 128 KiB between ordinary reads and discarding larger storage on the next read. Protobuf decoding retains independent payload ownership, and WebSocket message limits are unchanged. The gateway's serial writer reuses its protobuf output buffer too. Explicit dashboard refresh bypasses peer-detail caching; artifact-lock waits are cancellable. See [measurement scope](testing.md#performance-benchmarks).

## D074: Explicit atomic retraction of pending name invitations

Allow fleet owners to force a rename or fixed-name invitation creation when an unredeemed invitation reserves the destination name. Expose `--force` in the CLI and `force: true` in the corresponding gateway requests. The dashboard offers force only after a definite pending-invitation conflict and requires separate confirmation that the old installation URL will stop working. Never retry automatically after an uncertain response.

Under the gateway mutation lock, validate all name and identity constraints before changing records. Commit pending-ticket revocation and the requested mutation together in SQLite, rolling back both on failure. Old URLs reject subsequent script, metadata, binary, and redemption requests immediately after commit, including after restart. Downloads already admitted are not recalled, but their tickets cannot redeem after revocation.

Force cannot steal another registration, revoke a redeemed enrollment, bypass a client-name reservation, or affect another fleet. Same-name creation for an existing machine retains its `replaceId` signature requirement. Automatic invitations reserve no name at creation and cannot force a hostname takeover at redemption. This extends D015 and the owner-selected routing-alias decision without changing identity ownership, accepted work, or credential recovery. See [installation](installation.md) and [protocol](protocol.md#machine-lifecycle).

## D075: Bounded task data and durable cleanup

The cleanup-completion rewrite is superseded by [D155](#d155-read-terminal-payloads-on-demand-and-append-cleanup-outcomes). The journal still records intent before file removal.

Reject corrupt complete tombstone records instead of skipping them and allowing task-ID reuse. Add checksums to new records while accepting valid legacy records. Keep durable cleanup intent through failed removals and restart, exempting pending entries from tombstone eviction. Compaction updates the in-memory index only after its file replacement succeeds.

Add `tasks.page` with bounded metadata and stable task-ID cursors. Keep legacy listings array-shaped but summary-only, rejecting more than 100 records. Bound new task specifications and store results above 1 MiB as separate JSON result artifacts, preserving workflow output indexes. Oversized RPC responses return a bounded structured error with unknown outcome. Neither transport errors nor reconciliation trigger automatic replay. See [protocol limits and recovery](protocol.md#bounded-task-results-and-recovery).

MCP setup coalesces per configured server under the node lifetime. Waiters can cancel independently, unrelated servers initialize concurrently, and shutdown owns setup and session-wait goroutines.

## D076: Artifact budgets and reference protection

Account for blob, staging and resumable-partial bytes together. A configurable logical cap rejects writes instead of evicting owned content; zero retains unlimited storage. Check actual free space periodically with a default 256 MiB metadata reserve. This is a guard, not a reservation against external disk use. Reclaim abandoned unpublished files after a configurable age under per-artifact transfer locks, rechecking age after lock acquisition.

Reject owner overflow rather than weakening ownership. Retained task references, live grants and active readers prevent final deletion. Persist deletion intent before removing content and metadata, then retry failed removals. Existing shared legacy records remain shared. This refines D058/D059 without changing content digests or direct artifact delivery.

## D077: Bounded transport buffers and identity caches

Data-only relay byte charging is superseded by [D101](#d101-bounded-gateway-admission-and-aggregate-packet-retention)'s full-packet and aggregate budgets. Session fairness, offline ordering and durable identity-cache behavior remain unchanged.

Detach Pion data channels and run peer-owned receive loops with reusable bounded buffers. TLS still consumes independently owned pooled packets; authentication, packet-size negotiation and receive bounds stay intact. Count overflow only for valid packets rejected by a live queue.

Schedule gateway relay sessions round-robin, with at most 768 queued packets and 6 MiB per session inside the existing 1,024-packet, 8 MiB destination bound. The session limit accommodates a 4 MiB bulk window plus framing while leaving capacity for other sessions. Preserve data/close order within each session and treat offline notifications as FIFO barriers. Signaling retains bounded priority bursts. This refines D073's destination-wide FIFO without promising bandwidth QoS or preemption of an in-progress write.

Keep permanent client identity and transport history in SQLite. Load bounded caches, hydrate before identity checks and query reserved names through an index. Keep live client records pinned; inactive cache eviction never changes immutable fleet ownership or client roles.

## D078: Resource placement, explicit reconciliation and release signatures

The Sigstore-specific release trust below is superseded by [D153](#d153-pinned-ed25519-release-manifests). Placement, reconciliation, and rejection of unauthenticated public executables remain unchanged.

Selection excludes fresh known leases and update state, then scores active work per logical CPU plus CPU and memory pressure. Explicit memory, disk, CPU and freshness constraints reject missing or stale health. Unconstrained discovery can use directory-only candidates. Selection remains advisory; destination admission and leases own reservations.

Expose explicit account-client workflow reconciliation by reading the persisted plan and querying stable child IDs. Return bounded summaries and unresolved query errors. Do not reconstruct grants, resume interrupted parents, or resubmit children. This extends D060 without treating absence as proof that execution never happened.

The gateway release fetcher and CLI updater verify the signed manifest with Sigstore, TUF trust roots, a transparency-log entry, trusted signing time and the exact GitHub release workflow identity at the requested tag. Reject unsigned or mismatched metadata before downloading or executing binaries. This replaces runtime checksum-only GitHub release trust. Superuser development publication and bootstrap installation retain their distinct trust paths. See [release verification](updates.md#publish-github-releases).

## D079: Embedded management console with separate browser authority

Memory-only sessions and restart sign-out are superseded by [D095](#d095-browser-sessions-survive-gateway-updates). Browser authority and credential separation remain unchanged.

Serve a management console from the gateway's existing HTTP listener and embed all static assets in the Go binary. Use native browser JavaScript and CSS without a frontend build or runtime service. Read cached gateway health instead of adding peer observation, task scheduling, or application execution to the gateway.

Add separately provisioned Argon2id browser passwords bound to existing immutable account IDs in SQLite schema version 5. Use `@operator` for the separately provisioned superuser browser login. Existing bearer keys, account roles, fleet isolation, and CLI/MCP behavior stay unchanged. There is no public signup or default password. This extends D016's account credentials without replacing its bearer authentication.

Browser cookies authorize only an explicit management-route allowlist. Attach a verified principal in server-owned request context and reuse fleet and administrative handlers rather than issuing or exposing synthetic API keys. Cookies cannot authenticate bearer endpoints or peer/client WebSockets. Operator machine access remains limited to the legacy fleet; global user and update administration retains superuser checks.

Keep sessions bounded and in memory, store only opaque-token digests, recheck account status and password revisions, and require idle/absolute expiry, same-origin mutations, and session-bound CSRF tokens. Gateway restart signs out browsers without losing passwords. Password resets revoke browser sessions without changing task ownership or CLI keys. Bound password work outside the gateway mutation lock and commit credential changes before acknowledging them. Require HTTPS except for direct loopback development; configured HTTPS origins may use a trusted loopback upstream, never arbitrary forwarded scheme headers. See [web console](console.md).

## D080: Explicit LAN membership and peer Wake-on-LAN

The manual LAN label and opt-in metadata collection are superseded by [D084](#d084-automatic-interface-reporting-and-verified-lan-relationships). Execution authority, bounded magic packets, and no-replay behavior remain unchanged.

Use a shared operator-assigned `labels.lan` identifier plus an exact private IPv4 subnet match to authorize LAN selection, rather than treating overlapping private ranges, a shared public IP, or a WebRTC route as evidence of one broadcast domain. Opt-in nodes advertise bounded IPv4/MAC interface metadata in signed registration bytes. Retain it in the existing fleet directory for offline wake requests and gateway restart recovery. It is fleet-visible metadata, not proof of firmware support, and may become stale while offline.

CLI and MCP share one sender-selection operation. The default chooses one compatible online node; explicit source selection can choose a worker or a local client that asserts its LAN identifier. Nodes execute `network.wake` under normal capability authority and admission, resolve targets in their own fleet directory, and recheck live local interfaces before sending. Never accept arbitrary caller-provided MAC/broadcast destinations or move packet emission into the gateway. This extends the provider model without granting ambient worker authority or adding a database schema.

Bound each request to one UDP port 9 magic packet per distinct target MAC/subnet. Do not automatically wake on execution failure, repeat sends, or fail over after uncertain transmission. Report sent packets, not successful power-on. Firmware, NIC settings, broadcast delivery, and service startup after wake remain external requirements requiring native hardware verification. See [Wake-on-LAN](wake.md).

## D081: Aggregate work, tunnel, and route counts in owner health

Extend [D022](#d022-owner-visible-machine-health-without-a-local-node)'s health report with counts of active tasks, exec sessions, transfers, and other operations; forward and reverse tunnels; peers by connection route; the lease expiry; and the latest activity start or completion time. The browser console has no peer API, so without these counts it could show only a total. The node derives them from the records the peer activity view already uses.

Reports still carry no operation names, owners, peers, arguments, or paths, and reach only the fleet's account key. Gateways validate the new counts as non-negative and ignore them when absent, so older nodes show totals only. Per [D056](#d056-signed-registration-bytes-and-tolerant-health-reports), older gateways ignore the new fields. Task control and per-operation detail remain on the peer API.

## D082: Durable management audit log

Record sign-ins, failed sign-ins, sign-outs, and management changes in a SQLite `audit_events` table (schema version 6) instead of relying on in-memory state or process logs. Each event is written in the same transaction as the change it describes, extending [D016](#d016-sqlite-backed-users-and-isolated-fleets)'s commit-before-acknowledgement rule, so the log cannot miss a committed change or describe one that rolled back. A sign-in is refused when its record cannot be written. Update publication and release checks persist outside SQLite, so their events follow the action and failures are logged.

Events are fleet-scoped like the data they describe: account keys and browser sessions read their own fleet, and the operator reads every fleet, including failed sign-ins for unknown usernames. Actors are the browser login or the bearer key's account and ID. Source addresses are the direct TCP peer, because forwarded headers remain untrusted. Retention keeps the newest 10,000 events per fleet within a 100,000-event gateway ceiling, so one fleet cannot evict another's history, and caps failed sign-ins at 2,000 so password guessing cannot displace management history. Owners may clear their fleet's history and the operator every fleet, but each clear leaves one `audit.clear` record per affected fleet in the same transaction, so removal is itself auditable. Events never hold secrets, installation links, or execution arguments. This supersedes the console's earlier lack of a durable audit trail noted in [D079](#d079-embedded-management-console-with-separate-browser-authority).

## D083: Reported execution context and hidden redeemed invitations

Nodes include `runAs` in signed registration: `system` when the process runs as root or a Windows service account (LocalSystem, LocalService, NetworkService), and `user` otherwise, including an elevated interactive account. The value describes the running process, so it stays accurate after a startup-mode migration, unlike invitation metadata. Gateways reject other values and, per [D056](#d056-signed-registration-bytes-and-tolerant-health-reports), older gateways ignore the field.

Clearing invitations deletes revoked and expired records but only hides redeemed ones, because a redeemed invitation holds its machine's credential. Machines are managed, disabled, and unregistered from their own records rather than by revoking the invitation in the console.

## D084: Automatic interface reporting and verified LAN relationships

Report eligible IPv4/MAC interfaces on every machine automatically, independently of labels, and detect interface changes separately from resource sampling. Manual opt-in prevented updated nodes from retaining the information needed after sleep. Matching private prefixes only select candidates; they do not prove a shared LAN.

Use fleet-scoped gateway challenges and signed proofs delivered exclusively by UDP directed broadcast to establish directional relationships. The receiving machine must obtain the other node's fresh signature from a broadcast before reporting it through its authenticated connection. The gateway verifies both identities, interface bindings, and freshness before committing gateway-owned `lanPeers` to the existing directory. Registration cannot assert relationships. This requires negotiated `lanDiscovery` support but no Protobuf-envelope or database schema change, and works when execution connections use only the gateway relay.

Bound discovery concurrency, message sizes, candidate counts, attempt rates, and retained edges. Refresh observations while both machines are online and retain them for seven days for offline wake requests. Match exact live sender interfaces and retained target interfaces at transmission. Stale evidence remains possible if hardware moves while retaining identical addresses and MACs; discovery is last-observed reachability, not proof of an offline host's present location. It grants no execution authority and changes no firewall, power, or NIC settings.

CLI/MCP automatically prefer a matching local registered NIC or choose an online verified worker. Remove manual LAN assertions and `--lan`; labels cannot override missing evidence. Both machines need one overlapping online period to learn a relationship, so discovery cannot recover an unreported sleeping machine. This supersedes the manual membership portion of D080 without adding automatic waking, wake retries, or gateway packet emission. See [Wake-on-LAN](wake.md).

## D085: Standalone-orchestrator wake eligibility from local neighbors

Extend D084's local sender selection to clients without an enrolled local node. Requiring a registered NIC excluded the standalone orchestrator even when its OS had already learned the target's exact LAN MAC and IP. Accept interface-scoped neighbor records that match the authenticated target metadata, or match an awake witness's exact interface in a verified target relationship. A shared private prefix alone remains insufficient.

Keep OS commands in platform-specific helpers, bound their output and lifetime, and permit at most three non-magic UDP cache-warming datagrams to awake verified witnesses. Never send those probes to a sleeping target. Neighbor records provide last-known link-layer evidence, not cryptographic assurance against stale entries or ARP spoofing. Do not persist derived local edges, create client fleet registrations, modify neighbor tables explicitly, or add ambient worker execution authority. Missing tools or evidence fail local selection; the default may choose a verified worker before any wake transmission. Once a magic packet is attempted, never retry or switch senders automatically. See [local sender behavior](wake.md#send-a-wake-request).

## D086: Gateway-relayed console wake

The browser console has no peer API or account-client key, so it cannot invoke `network.wake` with an instruction-bound grant. Add a narrow management-plane relay instead, like lifecycle policy and managed updates: the fleet owner asks the gateway, the gateway selects one eligible sender by the same verified-LAN rule as the CLI, and sends it a single `wake.request` over its authenticated connection. The gateway still never sends magic packets.

A node opts in by advertising `gatewayWake` in signed registration only when its allow rules let account clients call `network.wake`. The sender applies those rules again, plus work admission, directory resolution, and live interface checks, and replies once. There is no retry or fallback sender, and an unconfirmed request is reported as possibly sent. Each attempt is recorded in the audit log. This is a deliberate, scoped exception to [D045](#d045-account-client-authority-and-instruction-bound-worker-delegation)'s grant requirement for worker execution: it carries no arguments beyond a fleet-resolved target, cannot reach other fleets, and grants no other capability. See [console wake](wake.md#wake-from-the-console).

## D087: Hybrid installation with isolated execution contexts

Console row presentation is refined by [D089](#d089-hybrid-host-grouping-with-explicit-execution-contexts); independent execution and lifecycle boundaries below remain unchanged.

Unbounded partial-installation recovery is superseded by [D108](#d108-hybrid-completion-and-timeout-cleanup).

Extend [D043](#d043-explicit-installation-context-and-unix-system-startup) with a composite Hybrid invitation, not a process that changes its own privileges. Create two independent identity-bound invitations in one transaction. User execution retains the normal name; System execution requires the explicit `-system` target. Keep separate protected binaries, profiles, state, startup registrations, updates and retirement. Show both contexts independently in machine views, rather than hiding divergent availability or task stores behind a single online flag. Ordinary Windows System mode remains LocalService; the explicitly privileged Hybrid companion uses LocalSystem. Unix companions run as root.

Extend [D045](#d045-account-client-authority-and-instruction-bound-worker-delegation) with instruction-only destinations. All their execution RPCs require exact grants, including requests from account clients. A destination may issue a grant for the requesting account client's own live transport identity, never an unrelated client. Shared CLI/MCP code prepares those grants for explicitly named System operations. Synchronous and stream grants end on close; task grants retain the existing durable acceptance and idle-expiry rules. Workers gain no ambient permission, nested coordination remains disallowed, and providers are still not sandboxed.

Exclude instruction-only destinations from automatic selection. Never retry User work as System, substitute System after logout, or replay uncertain execution. Atomic invitation creation and pending-pair retraction avoid half-reserved names. Installation can recover a completed System half before starting User enrollment. The two enrolled contexts retain independent lifecycle controls; this avoids inventing a cross-node transaction for task acceptance, updates, or shutdown. See [Hybrid installation](installation.md#hybrid-user-and-system-contexts).

## D088: Gateway startup management with local-command recovery

Extend the management-plane model of D086 with one fixed startup-context instruction. Fleet owners may request System or Hybrid through the gateway, but cannot supply a command, URL, binary, or profile path. Nodes advertise support, recheck account allow permission, and reserve idle admission exclusively against work, leases, and updates. The gateway records intent before dispatch and never retries an unconfirmed instruction.

Windows User to System migration can run only when the installed node already has administrator rights. Prepare a checksum-verified detached helper for that node's own profile, acknowledge admission, then shut down; the helper waits for supervisor exit and validates identity again before installing LocalService startup. Other contexts and insufficient privilege require a local installer. Do not open remote UAC or sudo prompts. Admission is not success: observe the actual execution context after reconnect, and show local recovery commands for failure or uncertainty without replay. Generated Windows invitation commands run directly as `irm 'URL'|iex` in the caller's PowerShell, avoiding a nested interpreter.

## D089: Hybrid host grouping with explicit execution contexts

The rename action's selected-context-only behavior is superseded by [D151](#d151-automatic-atomic-hybrid-companion-renames). Other context-specific actions and execution boundaries are unchanged.

The offline-User metric display is superseded by [D091](#d091-system-telemetry-for-an-offline-hybrid-user); selection and execution boundaries remain unchanged.

Refine D087's console presentation to show one host row with selectable User and System chips. Derive pairing from redeemed, fleet-owned invitation metadata, not a name suffix or a node assertion. Count that host once in machine and availability counters while retaining real per-context work counts. Online means at least one context is online; selecting a chip chooses which context's metrics and lifecycle actions are shown.

Default to User even when only System is online. Keep explicit System targets and exact instruction grants for authorized work requiring elevation. Client and AI guidance may select that privileged alternative when the requested operation requires it, but must inspect failure and partial effects rather than automatically replay User commands. Presentation does not merge identities, storage, tasks, updates, or retirement, and selecting a chip changes no CLI/MCP routing.

## D090: Short one-time installation tickets

Generate installation URL tickets from six cryptographically random bytes encoded as eight unpadded base64url characters. This shortens copy-and-run commands without changing the target's separate 256-bit credential or Ed25519 identity. The ticket has 48 bits of entropy, less guessing resistance than a permanent credential, and remains a temporary bearer credential with the existing default 15-minute expiry, revocation, and single-identity redemption. Retain the existing lifetime override and signed response-recovery behavior.

Check both the ticket hash and any derived Hybrid companion hash against all retained gateway invitations under the mutation lock. Retry random collisions at most eight times, then fail without changing records. Never overwrite another fleet's invitation or a redeemed credential. Continue accepting existing 43-character tickets and keep Hybrid companion derivation unchanged. Persist hashes only. This refines D015's ticket generation without changing fleet ownership, download pinning, or node authentication. See [installation and recovery](installation.md#single-use-and-recovery).

## D091: System telemetry for an offline Hybrid User

Refine D089's row metrics without changing its default User selection. When the selected User context is offline and its paired System context is online, show System resources and connection/tunnel counts, and sort by those displayed values. Restore User telemetry automatically when it reconnects. An explicitly selected offline System context keeps unknown live metrics rather than borrowing User telemetry; both offline contexts also keep live metrics unknown.

Use only the gateway-supplied pairing and cached reports. This display fallback does not initiate sampling or peer probes, change the drawer or management-action target, merge per-context counters, or authorize System execution. Lifecycle actions and CLI/MCP work still target the explicitly selected identity. See [Hybrid rows and counters](console.md#hybrid-rows-and-counters).

## D092: Optional driver-reported GPU resources

The display policy and Windows sampling below are refined by [D093](#d093-classified-accelerators-and-loaded-dedicated-memory).

Extend cached system snapshots with bounded per-device GPU inventory and optional utilization and dedicated-memory readings. Use NVIDIA's installed CLI, Linux DRM sysfs, and Windows/macOS inventory tools without CGO, privileged daemons, new dependencies, or driver installation. Discover inventory once per collector and sample live readings on the existing resource schedule. Bound commands by context, output, and device count. Missing hardware, permissions, tools, and driver metrics mean absent readings, not zero utilization or shared RAM presented as VRAM.

Carry these reports through existing JSON APIs and owner-only health, validating new fields without changing peer execution authority or worker selection. Omit new nested fields from legacy reconstructed-signature registrations; exact-byte registration and unknown-field health decoding preserve nodes-first upgrades. Conditionally show GPU data in console tables/drawers and terminal hardware details, hiding offline usage and preserving Hybrid action targets during telemetry fallback. Sort console GPU utilization by the highest known device reading rather than averaging dissimilar GPUs. See [GPU support and limitations](dashboard.md#gpu-resources).

## D093: Classified accelerators and loaded dedicated memory

Restrict GPU/NPU displays to identified discrete GPUs and NPUs. Keep integrated, virtual, and unclassified inventory in JSON, but hide it from tables, drawers, and terminal Hardware details. Add optional validated `kind` metadata to the existing `gpus` entries rather than renaming that wire field. Prefer Windows DXCore hardware/integrated flags and NPU attributes; elsewhere classify only recognized models or NPU drivers. Never infer a discrete GPU from vendor or memory size alone. Unknown older reports remain hidden until the node and gateway are updated.

Make loaded dedicated memory the primary console bar and sort value, keeping compute activity separately labeled. Add 64-bit DXCore dedicated capacity and Windows GPU Adapter Memory gauges, joined by native LUID. Do not use truncated WMI AdapterRAM, process-scoped DXGI budgets, summed GPU-process memory, or display ordinals. Unsupported counters remain unknown, and shared RAM is not presented as dedicated memory. Retain bounded sampling, per-owner health, offline suppression, and Hybrid action/telemetry separation. See [GPU/NPU support](dashboard.md#gpu-resources).

## D094: MCP execution failures and bounded workflow reconciliation

Treat MCP `isError` results as provider failures while retaining the original result in durable tasks. A successful MCP protocol exchange can still report failed execution; treating it as task success incorrectly released dependent workflow steps. Synchronous callers receive `mcp_tool_failed` with unknown outcome and a bounded diagnostic. This refines D006's MCP adapter contract without retrying calls or discarding partial task output.

Bound D078's explicit reconciliation to eight concurrent child reads, six seconds per child and thirty seconds overall. Preserve plan order, include structured bounded query errors and retain healthy observations when other children time out. Caller cancellation joins the readers. Keep ordinary owner-authorized `tasks.get` queries for compatibility; this still transfers full child records from workers, though reports retain only summaries. No grants, task submissions or parent-state changes are introduced. See [recovery behavior](protocol.md#bounded-task-results-and-recovery).

## D095: Browser sessions survive gateway updates

Session lifetime policy is refined by [D099](#d099-longer-bounded-browser-sessions); persistence and revocation remain unchanged.

Persist bounded browser sessions in SQLite schema version 7 rather than signing every user out when an update restarts the gateway. Store only cookie digests with username, password revision, CSRF token, creation time, and last activity. Restore valid sessions without renewing idle or absolute expiry. Recheck account status, password revisions, and operator availability at startup and on authenticated requests. Keep the existing management-route allowlist, cookie security, and browser-to-bearer separation.

Commit sign-in and sign-out with their audit events before issuing or clearing cookies. Password changes delete that login's persisted sessions in the credential transaction. Checkpoint activity at most once per minute per session and flush pending activity on graceful shutdown instead of syncing every poll. After a crash, an idle deadline may be up to one minute earlier, never later. Existing memory-only sessions cannot cross the first upgrade. This supersedes D079's restart-sign-out choice without changing CLI credentials or execution authority. See [console sessions](console.md#credentials-sessions-and-limits).

## D096: Gateway-first updates with deferred offline participants

The global idle barrier and unconditional gateway-first order are superseded by [D114](#d114-independent-node-rollouts-and-relay-safe-gateway-restarts). Offline deferral and startup catch-up remain, but deferral cannot erase an unresolved commit intent.

Stage and verify executable assets before maintenance. Reserve live nodes idle, recheck membership, then switch the gateway's versioned child first. Only a gateway already running the selected build may commit node replacements. This makes coordinator changes available before new nodes start, while retaining durable runtime selection and reservations. Gateway restart still causes a short connection outage; this is not a hot relay handover or permission to replay accepted work. Browser sessions retain D095's restart persistence.

Persist unavailable installation participants as explicitly `deferred` after two minutes offline, matching the maintenance reservation lifetime. Their absence is never an applied acknowledgment. Complete the available fleet and permit later publication without waiting indefinitely for sleeping or unreachable machines. Keep deferred identities visible until fresh selected-software and released-maintenance reports confirm catch-up. Live connections always rejoin coordination, even before their pending marker clears. Persistence failure leaves the participant blocking. The selected manifest remains downloadable for offline nodes, which catch up directly to the current selection when they return.

Add authenticated `GET /v1/updates` exposing only the selected ID and manifest, not fleet or rollout identities. Nodes query it on startup, stage their verified platform asset, then use existing gateway prepare/commit authorization and idle admission. Failed or unsupported startup queries retain authenticated connection offers as recovery. Standalone CLI/MCP processes are still not managed nodes. This supersedes D014's nodes-first order and indefinite offline wait without weakening superuser publication, checksums, work admission, or task durability. See [managed updates](updates.md).

## D097: Exact GitHub release delivery with gateway fallback

Treat download location separately from the selected deployment. A managed node first checks the tag-pinned GitHub manifest and signature in the gateway-supplied `releaseRepo`. Use public executable bytes only when the signed manifest has the same version and exact platform asset as the authenticated gateway selection. Verify bytes and executable metadata as before. Missing, different, invalid, or unavailable public releases fall back to the gateway without changing versions or accepting different checksums. Bound release metadata/signature work to ten seconds and public binary download to one minute; parent cancellation stops both sources rather than starting fallback.

Keep every selected platform asset on the gateway, whether published by `make update` or fetched from a release. This permits private releases and public-network outages without giving nodes the gateway's GitHub token. Never send fleet credentials to GitHub, and refuse redirects for authenticated gateway downloads. Offer and startup-selection metadata carry only an optional repository name, never a caller-selected URL or credential. Old nodes ignore it, and absent metadata keeps gateway-only delivery. This extends D078's signed release trust and D096's startup catch-up without changing publication authority, update order, or idle admission. See [managed delivery](updates.md).

## D098: Sigstore trust metadata transport fallback

Superseded by [D153](#d153-pinned-ed25519-release-manifests), which removes Sigstore and its trust-metadata downloads.

The public Sigstore CDN and its Google Storage origin can reject a gateway's location with HTTP 403, preventing otherwise valid release verification. On that response, network failures, HTTP 408/429, or server errors, fetch the same metadata file from Sigstore's official GitHub-hosted TUF repository. Restrict this mapping to the canonical public CDN origin; do not accept arbitrary replacement URLs, roots, or verification bypasses.

Both paths feed the existing TUF updater using its embedded trust anchor. Root rotation, signature thresholds, expiry, and target hashes remain mandatory, followed by D078's exact release identity and transparency checks. Cryptographic or format failures never cause mirror retry. Preserve HTTP 404 as the normal end of available root versions, and preserve cancellation and response-size limits. If both transports fail, reject the update rather than accepting unsigned data or changing the selected deployment. This extends D078/D097's availability without changing their trust model. See [release verification](updates.md#publish-github-releases).

## D099: Longer bounded browser sessions

Replace the console's 30-minute idle and 12-hour absolute limits with 24 hours idle and 7 days total to reduce repeated sign-ins across workdays. Set login-cookie maximum age from the same absolute-lifetime constant used by server validation. Authenticated polling renews only idle activity; it cannot extend the seven-day limit.

Longer sessions increase the time a stolen cookie can be used. Retain immediate revocation on sign-out, password changes, and account disable, plus the existing HTTPS, HttpOnly, SameSite, origin, CSRF, and management-only authorization rules. D095's persistence keeps original timestamps across restart; older browser cookies retain their issued maximum age until a fresh login. No database migration or CLI credential change is needed. See [console sessions](console.md#credentials-sessions-and-limits).

## D100: Gateway egress fingerprints ignore IPv6 privacy rotation

Replace D067's raw address inventory and gateway-source strings with selected gateway egress identities. macOS can alternate temporary IPv6 source addresses on an unchanged interface/subnet; treating each choice as a network switch closed healthy VM sessions and cancelled client calls. Match each UDP route probe's source to its active interface and record its index/name plus assigned IPv6 subnet. Retain exact IPv4 addresses and subnets to detect DHCP changes. Sort and deduplicate the fingerprint, include target zones, and reject incomplete interface/source samples rather than resetting on a partial view.

Keep the two-second, packet-free sampling and existing recovery for changed selected interfaces, subnets, IPv4 addresses, or reachability. Ignore temporary IPv6 address rotation and unselected address/adapter changes. This deliberately narrows proactive reset detection to gateway paths; next-hop changes preserving the fingerprint, peer-only path changes, and missing initial gateway resolution rely on normal transport failure detection. There is no portable next-hop inspection, active connectivity test, new configuration, or application replay. TLS identity, fleet authorization, generation checks, accepted-task durability, and disconnected-carrier grace remain unchanged. See [connection lifecycle](architecture.md#connection-lifecycle) and [operational reference](protocol.md#node-configuration).

## D101: Bounded gateway admission and aggregate packet retention

Extend D029's 64-client default with positive operator-configurable global/account connection, node, client and handshake limits. Reserve authenticated-account slots before upgrade and queue allocation, include pending handshakes in those limits, and revalidate authority before persistence. Reject excess connections promptly with HTTP 503 rather than queuing indefinitely. Bound attempts introducing new identity records with an account token bucket; unchanged identity reconnects skip it. These in-memory limits reset on restart and do not delete offline registrations, immutable ownership history or accepted tasks.

Extend relay backpressure to full retained packet envelopes across signaling and relay, with destination, account and gateway byte ceilings. Include spare payload capacity and unknown protobuf bytes plus a fixed bookkeeping charge. Keep reservations until writes finish and drain them after joining the connection writer, rejecting enqueue through stale routing pointers after cleanup. Bulk data leaves one sixteenth of byte capacity for control; preserve the legal bulk receive window, session-local overflow, round-robin ordering and offline barriers. These budgets exclude socket/runtime/marshaling memory and are not RSS guarantees or byte-weighted tenant fairness. Expose only aggregate limits and counters through the existing superuser metrics route.

Keep the separate committed observation view, full SQLite durability, pinned peer TLS, authorization, application heartbeats and no-replay semantics. Removing an unused production channel and reusing Wake candidate sorting reduce bookkeeping without changing routing or LAN decisions. Proxy admission is independent: expose's new bypass HTTP reservation must be sized alongside Control, not bypassed by raising only one service's limit. See [limits and defaults](protocol.md#gateway-resource-admission) and [expose deployment](expose.md#limits-and-troubleshooting).

## D102: Native agent-desktop through a private pipe

The native-only command-surface restriction is superseded by [D127](#d127-full-agent-desktop-command-routing-through-control). The protected native input and privacy constraints remain unchanged.

The separately built C++ helper contract is superseded by [D103](#d103-isolated-go-desktop-helper-shipped-with-control). Its security and private-pipe constraints remain unchanged.

Add opt-in `desktop.run` backed by an operator-configured native C++ helper, not a native dependency in the Go executable. The helper loads an exact-version agent-desktop FFI library and checks ABI major and action layout before dispatch. Keep input narrow: status, app-scoped snapshots, headless qualified-ref click/clear, and headless value setting. Do not expose arbitrary commands, screenshots, clipboard, tracing, or physical-input fallback through this protected path.

Extend D041's worker-local secret store to native value setting. Resolve names only at execution and send values over a bounded anonymous stdin pipe, never argv, environment, task specifications, or an input spool file. Mask every native response using current configured values, including observations without a secret input, and suppress all raw native helper logs. Preserve sanitized command-failure envelopes in tracked task results while failing execution; uncertain effects are never replayed.

Keep native refmaps outside the filesystem API workspace and use a separate state namespace without inherited agent-desktop session settings. Share Control's per-user RPA lock and existing task, lease, delegation, cancellation, and maintenance contracts. This trades a separately built and trusted native helper for a CGO-free core and no Python runtime. Private refmaps and process memory can contain credentials; masking is not isolation from the worker account, target application, or unrestricted execution. See [native setup and limits](desktop.md).

## D103: Isolated Go desktop helper shipped with Control

The ABI-6-only check below is superseded by [D120](#d120-explicit-desktop-abi-6-and-7-support). The isolated helper and security constraints remain unchanged.

Replace the separately built C++ sidecar with `control __desktop LIBRARY`, selected by an absolute `desktop.library` configuration. Use pinned purego bindings on Windows, Linux, and macOS amd64/arm64 while preserving `CGO_ENABLED=0` builds. Route helper mode before ordinary profile, account, and service initialization. Load the library only in that subprocess, never in the serving node. This lets managed updates ship helper code without a separate compiler or Python runtime. Native library installation, desktop configuration, and OS permissions remain explicit and are not enabled by `make update`.

Keep D102's worker-side resolution, private stdin packet, masking, suppressed logs, private state, headless policy, and no-replay behavior. Validate the full packet before loading, check ABI major 6 and the 96-byte action layout, pin Go buffers during native calls, and copy only bounded native output before releasing it. Infrastructure failures and recovered Go panics return fixed diagnostics without input, paths, or panic values. Native crashes and hangs remain isolated and subject to worker cancellation. Retain external helper-command configurations for compatibility, but reject their combination with `library`. The native library and private refmaps remain trusted with plaintext credentials; process isolation is not a sandbox. See [native setup and limits](desktop.md).

## D104: Install desktop dependencies only when needed

Do not bundle or distribute upstream agent-desktop FFI archives with Control releases, installers, or managed updates. The same native version can serve multiple Control releases without repeated uploads or installation. Keep D103's isolated Go helper and explicit absolute `desktop.library` configuration.

When a GUI task needs missing desktop support, the skill and MCP instructions suggest installation and ask for approval. After approval, obtain the compatible OS/architecture FFI archive directly from the public upstream release, verify its checksum, keep companion files outside `workDir`, configure the User profile, and restart while idle. OS grants require separate approval. Ordinary work, enrollment, startup, and updates do not trigger dependency downloads. Preserve secret-reference input, masking, suppressed logs, and no-replay behavior. See [native setup](desktop.md) and [Control skill](../skills/control/SKILL.md#gui-work-only-when-needed).

## D105: Scenario-first client discovery

The local-only catalog and naming below are superseded by [D106](#d106-shared-and-machine-specific-runbooks-in-approved-stores). Instruction-first discovery and no-automatic-execution rules remain unchanged.

Extend D066's project-local scenarios with explicit CLI/MCP reads and automatic enrichment of successful client `node.describe` responses. Include small documents inline rather than relying on an AI agent to guess that a hidden directory exists or request a separate resource. Use the alias returned by the worker so stable-ID requests find the same current folder. Keep storage local to the invoking project, with no parent/home search, gateway synchronization, worker publication, or automatic execution.

Share bounded, rooted Markdown/JSON reading between CLI and MCP. Reject symlinks, special files and invalid text; report discovery errors rather than claiming no procedure exists. Bound document count, paths and inline bytes. Omit whole documents beyond the inline budget and require a complete named-file read instead of returning truncated action sequences. Remote capability discovery remains available with an explicit local error.

Teach agents to read matching scenarios before ad-hoc software discovery or questions already answered by saved instructions. Procedures identify applications, secret reference names, state branches, verification and recovery, but cannot confer authority, bypass current capability checks, approve installation/elevation, or justify replaying uncertain effects. Their contents are sent to the calling AI client, so they must never contain credential values. No execution grants or protocol changes are introduced. See [current runbooks](runbooks.md).

## D106: Shared and machine-specific runbooks in approved stores

Rename scenarios to runbooks and replace the project-only machine-folder contract with one shared reader for project `.control/runbooks/`, OS-native per-user global storage, and selected-worker user/common application-data stores. Keep shared action IDs and alias/stable-ID bindings separate. Stable-ID bindings survive alias changes. Use the approved paths documented in [runbooks](runbooks.md); do not reuse node configuration/credential locations or depend on an editor's own storage.

CLI/MCP discovery works without a node for local stores. A selected node adds authorized worker reads and matching bindings. Include small documents inline in client descriptions, but keep raw worker `node.describe` metadata-only. New `runbooks.list` and `runbooks.read` require account-client authority or an exact delegation plus existing method access, never ambient worker/discovery permission. These are read-only observations, not execution providers. Preserve normal System instruction grants and never choose elevation to read a common store.

Expose origin-specific availability/errors and complete exact-source reads, with no substitution after a failure. Keep conflicting copies visible and rank stable-ID, alias, then shared matches; within a scope prefer project, worker-user, worker-common, global. Do not concatenate or execute instructions. Bound source trees, document sizes, metadata, conflict references, and aggregate inline text. Invalid sources report no partial documents while healthy stores remain available. Worker instructions are untrusted content and never authority over the orchestrator. Runbooks contain secret references, not values.

Supersede D066/D105's naming, layout, and local-only discovery. Machine renames migrate only the invoking project's alias-bound folder and supported routing references, retaining prior transactional boundaries and no-replay recovery. Shared instructions, stable-ID folders, global stores, and worker stores are untouched. Old scenario commands/fields/roots are replaced; migration is explicit and must not overwrite existing documents. There is no gateway runbook synchronization, automatic execution, secret-value API, or arbitrary remote root selection.

## D107: Agent-readable runbook applicability metadata

Describe shared-action suitability in bounded Markdown YAML frontmatter or top-level JSON metadata, not OS/application/hardware folder categories. Return `title`, `description`, and agent-friendly `requires` guidance even when full instructions are omitted. Accept plain language, arbitrary key-value details, lists, and nested combinations without mandatory requirement keys or a fixed eligibility schema. Keep action IDs path-derived and individual-machine bindings separate.

Validate metadata text, JSON-compatible values and structural limits without a fixed requirement vocabulary. Unknown requirement fields remain visible, never dropped. Reject malformed values, YAML aliases, merge keys, non-string mapping keys and bounds violations; validate worker-supplied requirements during client enrichment too. Preserve numeric conditions across JSON hops. Source errors remain visible and do not hide healthy stores.

Agents interpret the guidance in context using instructions, fleet observations and authorized worker evidence. Do not invent artificial OS, GPU, or generic Control capability restrictions. Discovery neither runs checks nor installs dependencies, schedules work, or reserves resources. Metadata conveys no execution authority and does not change fleet, ownership, delegation, or explicit System-context boundaries. See [runbook applicability](runbooks.md#applicability-metadata).

## D108: Hybrid completion and timeout cleanup

Bound a Hybrid installation by its original invitation expiry, 15 minutes by default. Ticket redemption is not successful startup. Commit completion for both invitations with the second successful context registration. Before that commit, expired partial credentials cannot register or recover redemption. Already installed pairs acquire the marker on gateway upgrade from their two registered contexts. Once complete, later logout, disable, revocation or independent unregistration does not reopen installation or retire the other context.

At gateway startup and every 30 seconds, remove expired incomplete pairs in one SQLite transaction with directory removal, credential revocation, audit events and identity-retirement policy. Run the same cleanup before invitation creation and clearing. Preserve the pair's records until this transaction, including after revocation or manual list clearing, so cleanup cannot lose an installed half. On persistence failure restore live maps and publish nothing; retry later. Preserve immutable ownership and any superseding enrollment. Disconnect retired peers only after commit, and use their existing signed-policy retirement path for local startup/executable cleanup.

This supersedes D087's indefinite partial-install recovery, not its separate runtime and task lifecycle. Atomicity covers gateway enrollment state. Local OS installation is sequential; offline uninstall waits for gateway contact, and retained configuration, state and work files are not rolled back or deleted. A timed-out attempt needs a fresh invitation. See [Hybrid installation](installation.md#hybrid-user-and-system-contexts).

## D109: Machine-wide managed update download cache

Deduplicate automatic and development-rollout executable downloads across node profiles on one host. Use content-addressed blobs in a cross-account machine cache, with a filesystem lock held through lookup, network download, and atomic cache publication. Keep the lock file permanently and retain at most four binary blobs. Cancellation releases waiters without another source attempt. Unavailable cache access, publication, or a five-minute lock timeout falls back to the existing private download path.

The cache is untrusted transport storage, not a shared runtime or authority source. Open entries within an OS-rooted directory, reject symlinks and special files, and verify the bytes copied into private profile staging against the authenticated selected asset. Execute version validation only on that private copy. Never execute, hard-link, or load runtime selections from the shared cache; never put credentials there. A writable User cache entry cannot choose a System executable with another digest. Cache mutation, deletion, or hostile lock replacement can deny reuse, but must not bypass validation or redirect writes outside cache storage.

Preserve D087's isolated User/System identities, runtime ownership, admission, supervisors, and restarts, and D097's exact GitHub preference with authenticated gateway fallback. No gateway coordination, new execution grant, or protocol field is needed. Both contexts must run cache-capable updater code, so the first upgrade from older binaries may still download separately. See [paths and operational limits](updates.md#machine-wide-download-reuse).

## D110: Hybrid preflight, staging order, and rerun recovery

Windows User-phase elevation is superseded by [D113](#d113-limited-token-windows-hybrid-user-installation). Its preflight, pinned staging, and bounded recovery contracts remain unchanged.

The Hybrid installer used to install the root or LocalSystem companion first, so most User failures appeared only after a privileged service was running, and a User failure after redemption could not be rerun because its script returned HTTP 410. The installer now obtains the binary as the user and runs `enroll --preflight` before any privileged change. The System installer takes the binary from the staging folder through its existing checksum-verified local candidates. Gateways keep script, metadata, and binaries available, and retained, until an incomplete pair expires or completes. The same command is therefore safe to repeat until the deadline.

`POST /install/{ticket}/check` shares its rules with redemption, so preflight cannot drift from what the gateway enforces. Supplied keys prove possession under a separate signing purpose, which stops a ticket holder probing other identities and stops a check proof redeeming. The check reduces late failures but cannot remove them: conditions can change between preflight and redemption, and D108's timeout still bounds what remains.

Windows Hybrid stages binaries in the ACL-protected pair directory instead of `%TEMP%`, closing a hash-then-execute window against same-user limited processes. Ordinary Windows System and elevated User installations still stage in `%TEMP%`; extend this if their elevation is treated as the same boundary.

Windows privileged path checks use native security APIs in place of one PowerShell process per path. They preserve the previous rules and fail closed on access entries they cannot parse. Retired companions delete their managed binaries, since D087's pair-specific paths are never reused, while keeping identity, state, and work files. Linux `--linger` is opt-in because it changes the account's login behavior beyond Control.

## D111: Gateway-hinted machine state with a slower poll

Every node polled signed `POST /v1/node/state` every two seconds, which a Hybrid host paid twice. The gateway now sends a stateless `machine.state` hint after each persisted change and on registration. Nodes negotiate this through `/v1/auth` and a signed registration field, then poll every 30 seconds while connected, and every two seconds while disconnected or against an older gateway.

The hint carries no policy, so the signed query remains the only source of state and of acknowledgement, and machine admission semantics from D023 are unchanged. Losing a hint delays a change until the next poll rather than losing it. Retirement already closes the connection, and a drop now triggers an immediate query, so a retired node still learns its policy without waiting.

## D112: Lower recurring cost per node context

A Hybrid host runs two full nodes, which is the isolation boundary and stays. Their avoidable recurring work is reduced. Companions sample every 60 seconds by default, because the console shows their telemetry only while User is offline. Node cleanup hashes the running binary only when it would delete something, instead of every minute. D109's cache retention narrows to the requested binary and one other, since nodes fetch only the selected version and older entries help only a rollback. Updaters keep reporting status during long staging, including a wait for the other context's download.

Windows GPU memory sampling still starts PowerShell per sample in each context, now less often for the companion. Native counters would remove it on every Windows node. [D154](#d154-in-process-resource-sampling) replaces it with native counters.

## D113: Limited-token Windows Hybrid User installation

Direct linked-token process creation is superseded by [D118](#d118-validated-desktop-parent-for-windows-hybrid-user-launch). Protected staging, non-elevated User execution, and firewall ownership remain unchanged.

Keep Windows Hybrid's elevated companion installation, but run User preflight and enrollment in a separate process with the installing account's linked limited token. Validate that token's user and elevation, and select it explicitly at process creation. Fail closed on missing tokens or launch errors, including UAC-disabled accounts; never inherit the elevated token or choose another logged-on account. Dispatch the fixed helper before ordinary User profile/log initialization and recheck non-elevation in its limited child. An active ordinary System service needs a separate local uninstall before limited User installation.

Run the child from checksum-verified protected staging, granting its user read/execute only. Do not inherit that grant into System credentials, and copy candidate bytes without importing source ACLs. Check existing companion path ownership and ACLs before recovery executes its launcher. The protected elevated parent configures User firewall rules after enrollment rather than launching the writable User CLI through UAC. A post-registration firewall failure needs explicit firewall repair, not enrollment replay.

Replace install-self's executable version probe with read-only checksum and Go build-metadata inspection. An unrelated destination remains untouched, and matching bytes can be reused without launching them. This closes the elevated execution of an existing per-user installation while retaining isolated profiles, exact grants, identity ownership, single-use completion, and no automatic replay. This supersedes only D110's Windows User-phase privilege handling. See [Windows Hybrid installation](installation.md#hybrid-user-and-system-contexts).

## D114: Independent node rollouts and relay-safe gateway restarts

Replace D096's fleet-wide idle barrier with independently reserved node updates and at most two live commit intents. Busy, stale, downloading, and offline nodes do not prevent eligible peers from progressing. Persist each commit intent before dispatch; require fresh applied/resumed or explicit error/resumed confirmation to clear it. An unresolved intent prevents another selection and the gateway switch even after offline deferral. Superseding a merely pending deployment is allowed; legacy persisted installation records retain their conservative publication barrier. Preserve same-version skips, node-local admission, identities, supervisors, and task durability.

Gateway-first becomes a compatibility requirement. Read node gateway-epoch ranges from the exact verified executable and report them as staged software. Bind them to the selected manifest asset before allowing a node to update ahead of the coordinator. Current executables implement gateway epoch 1 and declare node support for legacy epoch 0 through epoch 1. Undeclared target builds wait for the gateway; undeclared running nodes cannot authorize a gateway epoch above 1. Nodes check the authenticated gateway epoch before registration. Compatibility declarations inherit the executable's existing authenticated checksum rather than changing manifest serialization, signatures, or IDs. A breaking epoch needs bridge nodes that support both sides.

Gateway restart safety belongs to transport admission, not node work counts. Track bounded relay lifetimes by authenticated endpoints, connection generations, and session ID, including standalone clients and pending opens. A negotiated, signed drain instruction stops new streams on cached outgoing relay sessions while existing streams finish. Only outgoing owners retire idle carriers, since an incoming side can appear idle before a remote open arrives. Legacy carriers remain blockers until naturally closed or disconnected. A 30-second attempt followed by 30 seconds of backoff never force-closes traffic.

Atomically fence relay and protected HTTP admission only after no live carriers, queued/in-progress packet writes, admitted mutations, or executable downloads remain. Recheck running-node compatibility and persist the gateway switch before invoking its supervisor. Failed switching reopens admission. Local node tasks and direct WebRTC sessions can continue, but new signaling, discovery, and workflow steps may fail during the brief outage. This is not relay handover and does not permit replay. Expose per-node pending reasons and separate gateway blockers while keeping global details superuser-only. See [managed updates](updates.md#idle-reservations-and-restart-behavior).

## D115: Worker-local secret age metadata

Extend D041's local secret records with optional UTC `createdAt` and `updatedAt`. Persist timestamps and values in the same locked, atomically replaced file so a successful mutation cannot publish one without the other. New names get both dates; each explicit successful set refreshes the update date, even when the supplied value is unchanged. Creation survives updates, while deletion and recreation start a new record.

Accept legacy string entries without rewriting on reads or deriving dates from file metadata. Their creation remains unknown after an update, which records only the now-known update time. Mutating another entry preserves all known and unknown dates. The first successful mutation writes structured records; older binaries cannot read that format, so upgrade the node and management CLI before using it.

Keep plain `secrets list` names-only and add opt-in `--metadata` for names and known timestamps, omitting unknown dates. Use a separate value-free metadata type rather than serializing stored records. No secret management RPC, MCP tool, value-read command, rotation policy, or gateway synchronization is added. Dates describe local writes, not credential issuance or application-side rotation. See [metadata and compatibility](secrets.md#enter-credentials-outside-chat).

## D116: Encrypted owner-managed worker secrets

Extend D041's local-only administration with explicit remote CLI and browser management, not credential-bearing execution tasks. Reuse fleet-owner authentication and console origin/CSRF checks through three narrow routes: metadata/key listing, encrypted set, and delete. Common keys and delegated workers gain no authority. Nodes independently require account `secrets.manage` permission, with disabled and maintenance admission enforced. An explicit System target manages that profile only, without a general execution grant or privilege fallback.

Encrypt values in the originating CLI or browser to a P-256 key generated by the worker process. Use standard ECDH, HKDF-SHA-256 and AES-256-GCM, binding destination, name, request ID and expiry in AAD. Signed registration carries the public key; legacy registrations omit it. Require HTTPS except for direct loopback development. The gateway relays ciphertext without decrypting or persisting it, and audits machine/operation/outcome without secret names or values. Browser sessions need no general peer authority or copied client keys. The gateway remains trusted for key discovery, authorization and browser code; this does not protect against an actively compromised gateway.

Bound message shapes, in-flight requests, handler concurrency, expiry and replay retention. Restart invalidates pending envelopes by changing the worker key. There is no offline queue, automatic retry, provider, secret-bearing task, management peer RPC, MCP tool or value-read response. Unknown delivery needs metadata inspection before another explicit action. Preserve D115's timestamps and legacy handling. This supersedes only D041's local-only management restriction; unencrypted worker storage and unrestricted-process trust limits remain. See [remote secrets](secrets.md#manage-a-worker-from-the-orchestrator).

## D117: Strip console comments during Go bundling

Keep JSDoc and explanatory comments in editable console JavaScript, but omit them from binaries produced by `cmd/control-bundle`. Use esbuild's Go API to remove comments and excess whitespace, then supply temporary copies through Go's build overlay for native and cross-platform builds. Preserve top-level names and classic-script scope without module bundling, identifier mangling, source maps, or runtime minification.

This extends D079's embedded console while keeping the existing Go-only build command. It avoids checked-in generated assets, stale copies, source rewrites, and a Node.js build dependency. Direct Go development builds still embed original source. See [console bundling](console.md).

## D118: Validated desktop parent for Windows Hybrid User launch

Windows can return an identification-only impersonation token for `TokenLinkedToken`. Checking its user and elevation does not make it suitable for `CreateProcessAsUser`, which rejects it with `ERROR_BAD_IMPERSONATION_LEVEL`. Do not try to raise that token's impersonation level or substitute the elevated installer's token.

Launch the fixed protected helper with `PROC_THREAD_ATTRIBUTE_PARENT_PROCESS`, through Go's `SysProcAttr.ParentProcess`, using the desktop shell's primary token. Before launch, verify that the installer is elevated and the shell has a non-elevated primary token for the same account and Windows session. Keep the opened process handle through command completion. Go supplies an explicit inherited-handle list. Forward output through pipes because console handles cannot be duplicated into the shell process; use NUL input for the non-interactive helper and bound output-pipe cleanup. Preserve structured arguments, a cleaned explicit environment, protected working directory, exit status, and context cancellation.

Missing shells, other-account elevation, elevated shells, and query/launch errors stop installation. There is no shell-command execution, arbitrary process search, or elevated fallback. The child still rechecks non-elevation before profile access. This supersedes only D113's linked-token launch mechanism and requires a desktop session under the installing account. Invitations pin executables, so recovery from an older installer needs newly published binaries and a new invitation. See [Windows Hybrid setup](installation.md#hybrid-user-and-system-contexts).

## D119: Protected Hybrid parent removes legacy User login tasks

Older elevated Windows User installers can leave a matching login task whose security descriptor denies deletion to the limited token now required by D113/D118. Replacement enrollment uninstalls old startup before replacing the profile, so it can fail at `Unregister-ScheduledTask` after the System companion has already registered. Do not solve this by elevating the User child or granting broader task permissions.

Before actual limited enrollment, let the already elevated, verified Hybrid staging executable remove only the existing Control task bound to the exact root-folder name, profile description, and installing account SID. Perform this after protected-path and same-user desktop-parent validation. Use the existing fixed ownership-checked removal script through the OS PowerShell and built-in modules. Never execute the task action or a per-user installed binary, alter its ACL, select another account, or retry an uncertain mutation. Missing tasks are harmless; ownership conflicts and scheduler failures stop the phase before enrollment. Preflight skips task removal entirely, and cancellation is checked before preparation and launch.

This extends D113/D118's division of privileges with one startup-metadata operation. User profile loading, enrollment, task creation and execution remain limited, and firewall setup remains with the protected parent after enrollment. A subsequent User failure can leave its old startup absent until recovery, as with the existing replacement uninstall; there is no OS-wide rollback or extension of the incomplete-pair deadline. Existing pinned installers need a newly published binary and invitation. See [Windows Hybrid recovery](installation.md#hybrid-user-and-system-contexts).

## D120: Explicit desktop ABI 6 and 7 support

Accept agent-desktop ABI 7, shipped in v1.1.0 including native Windows ARM64 assets, while retaining installed ABI 6 libraries. The status, snapshot, qualified-ref action signatures, action kinds, headless policy, and 96-byte action layout used by Control are unchanged. ABI 7's breaking low-level exact-identity APIs are not bound or exposed by this adapter.

Read `ad_abi_version` and accept only major 6 or 7. Call `ad_init` with that exact major and check `ad_action_size` before creating an adapter or dispatching a request. Do not infer compatibility from the release number, accept unknown future majors, or retry initialization against another major. Callback tests exercise every supported command under both majors and reject unknown versions before initialization. Existing private-pipe credential handling, masking, suppressed logs, bounds, cancellation, and no-replay rules remain unchanged. This supersedes only D103's ABI-6-only restriction. See [native setup and compatibility](desktop.md#enable-on-a-worker).

## D121: Resumable local-to-worker bulk uploads

Add `file-upload-v1` on the existing peer bulk lane, shared by CLI and MCP through `internal/client`. A fresh CLI process per base64 `files.write` chunk creates needless handshakes, permanent identity bindings, and disk syncs, and can exhaust D101's identity-rate bucket. Keep that safeguard unchanged; transfer bytes through one bounded stream per attempt instead. Retain artifact delivery for worker-to-worker transfers and artifact downloads for retained outputs.

Bind each upload ID to the stable authenticated owner, immutable worker identity, destination, size, and SHA-256. Stage on the destination filesystem with independent bounded staging quotas, checksummed metadata, and synced contiguous-prefix checkpoints. Resume is explicit and verifies both prefixes before sending more bytes. A connection failure never retries the stream or invents a replacement ID. Account routing is required; no local API streaming proxy, ambient worker authority, automatic System fallback, or gateway execution endpoint is added. Exact System grants expire/revoke with each attempt and are not restored from checkpoints.

Require a complete disk rehash and explicit finish before atomic create-only hard-link publication. Existing destinations always conflict. Persist publication intent and retain staging identity until a completion receipt is durable, so restart/lost-response reconciliation never guesses from content equality or automatically recreates a destination. Unsupported filesystems fail closed. Keep terminal receipts and failed-cleanup intents within bounded retention; unknown history does not imply that work never happened. Uploaded workspace files are ordinary files, not automatically retained artifacts or installed software.

Use rooted filesystem operations and exclude reserved staging/metadata from ordinary file RPCs and artifact export, including existing symlink aliases. These restrictions do not sandbox unrestricted node-user programs. Preserve existing transport windows and account/gateway packet budgets. Unix publication syncs directories; Windows provides atomic visibility and process-restart recovery without a directory-entry power-loss guarantee. See [uploads](uploads.md), [streaming](streaming.md), and [verification](testing.md#upload-verification).

## D122: Separate execution and operation capacity

Desktop routing and visibility are refined by [D123](#d123-visible-pool-queues-and-independent-desktop-admission). Execution/I/O separation and the safety rules below remain unchanged.

All provider invocations previously shared `maxTasks` slots. A handful of long-running commands could therefore block file reads, HTTP requests and GUI batches even though those activities were independent. Keep commands, AI CLIs, custom providers and MCP tools on that execution pool, and admit known built-in file, HTTP, Wake-on-LAN and desktop capabilities through a separate `maxOperations` pool, eight by default and configurable from 1 to 256. Use identical routing for synchronous calls and durable tasks; do not infer resource use from arbitrary command arguments or MCP tool names.

Keep coordination outside both pools to avoid local-child deadlocks. Preserve per-user GUI locking, total accepted-task bounds, cancellation, durable acceptance, authorization, machine-wide leases and idle-only maintenance. This provides bounded concurrency, not CPU/memory isolation or safety for conflicting external state. Expose advisory execution/operation limits and usage in peer descriptions, and teach CLI/MCP callers that busy workers can still accept independent work. No scheduling or execution moves to the gateway. See [worker concurrency](protocol.md#worker-concurrency).

## D123: Visible pool queues and independent desktop admission

RPA participation is removed by [D126](#d126-retire-the-rpa-adapter-in-favor-of-agent-desktop). Native desktop keeps the same slot and shared lock.

Task slot lifetimes and transfer-stage visibility are refined by [D124](#d124-bounded-task-staging-execution-and-publication). Desktop separation and per-user locking remain unchanged.

Desktop invocations previously acquired general operation slots before waiting for the shared per-user desktop lock. Enough lock waiters could block independent file or HTTP work. Give the built-in desktop/RPA adapters one separate admission slot per node, without a new configurable concurrency setting, and retain the existing shared per-user file lock across profiles. Unconfigured optional adapter names used by custom providers remain execution work. Coordination still holds no provider slot.

Share admission accounting between synchronous calls and durable tasks. Report limits, in-use slots and current local queue counts in descriptions and authorized peer activity snapshots, preserving them through CLI/MCP aggregation. A desktop invocation admitted locally but waiting on another profile's file lock counts as in use, not locally queued. Keep each pool's counters consistent under a short mutex, never held through provider execution or waiting. Return independent observations and preserve absent legacy queue/desktop readings as unknown.

Retain task/transport bounds, cancellation lifetimes, owner checks, instruction-bound grants, durable acceptance, machine-wide lease exclusion and maintenance protection for queued/admitted work. These reports are advisory, not scheduling reservations or proof of CPU usage. Gateway health, automatic placement and frontend tables remain unchanged. See [worker concurrency](protocol.md#worker-concurrency).

## D124: Bounded task staging, execution and publication

Publication admission for inline results is refined by [D130](#d130-inline-task-results-bypass-artifact-publication-admission). Separate transfer pools and provider-result capture remain unchanged.

Tasks previously held their provider slot through artifact input fetches and output/result publication. Transfer waits could therefore exhaust execution capacity while no provider was running. Add task-only input staging and output publication pools, each defaulting to four slots and configurable from 1 to 256 through `maxTaskStaging` and `maxTaskPublications`. Stage inputs before provider admission, capture returned results as independent JSON before releasing provider admission, and publish outputs/results afterward. Capturing under provider admission preserves result ownership for providers that serialize access to reusable storage. Input-free tasks bypass staging; tasks without results or declared outputs need no publication admission. Provider-internal I/O remains execution work. Coordination never holds a transfer-stage slot while awaiting its children.

Keep each accepted task in its existing owned goroutine and lifetime context. Durable identity/specification, original owner, delegation activity, busy lease exclusion, timeout, cancellation and maintenance protection span all stage waits and work. Queued tasks may be staging inputs; running means provider admission has occurred, not necessarily that the provider is still running during publication. Failed/cancelled publication never replays a provider, and shutdown joins all stages before releasing node state. Cancellation finalizes without waiting for unrelated publication capacity or creating deferred artifacts, so unpublished results can be discarded.

Expose optional `inputStaging` and `outputPublication` pool reports alongside provider capacity, preserving unknown legacy readings. Retain the 256 accepted unfinished-task bound across the pipeline, including tasks waiting with provider results. These limits bound concurrent stages and task counts, not arbitrary provider memory or workspace bytes. Preserve artifact budgets, immutable copies, result-artifact contracts, shared desktop locking, authorization and transport behavior. See [worker concurrency](protocol.md#worker-concurrency).

## D125: Bounded capacity-aware workflow ready ordering

D060's plan-ordered launch loop could fill every workflow slot with children queued at one busy destination, leaving a later ready step's idle destination unused. For parallel workflows, inspect up to 32 unstarted ready steps per refill and query up to eight distinct destinations concurrently through existing discovery-authorized descriptions. Bound the entire probe phase to one second and join every reader. Observe local counters directly without expanding coordinated execution authority. No gateway scheduling, new grant, protocol field, background poller or reservation is added.

Prefer free provider/input-staging headroom, then unknown readings, then busy pools, with stable plan-order ties. Conservatively subtract this workflow's in-flight children for their full lifetime, including new batch selections, because snapshots may predate task admission. This can double-count already observed children but avoids immediately refilling the same pool from stale headroom. Desktop capability names check both desktop and execution headroom because descriptions cannot distinguish built-in adapters from custom providers using those names. Failed/denied/legacy readings stay unknown. Do not infer arbitrary provider resource use or select another worker. When all candidates are busy, keep submitting in stable order rather than polling for capacity. The bounded window does not promise visibility of every idle destination or fleet-wide fairness.

Keep sequential workflows unchanged, and preserve validated dependencies, artifact provenance, fixed task IDs/targets/specifications, destination-owned grants, total parallel bounds and child lifetimes. Ordering applies only before launch; accepted tasks never move or resubmit. Buffer bounded completion outcomes during probes and observe them before launching further work. An observed failure stops new submissions while already started siblings finish. Publication, leases and resource telemetry remain outside the ordering heuristic; normal destination checks still enforce authority and admission. See [workflow ready ordering](protocol.md#workflow-ready-ordering).

## D126: Retire the RPA adapter in favor of agent-desktop

The requirement to use ordinary `exec.run` for full upstream functionality is superseded by [D127](#d127-full-agent-desktop-command-routing-through-control). Legacy RPA removal and protected native input remain unchanged.

Remove built-in `rpa.run`, MCP `control_rpa`, the RPA action schema, configuration field, and supplied Python helper rather than maintaining two desktop automation stacks. Keep opt-in native `desktop.run` for masked observations and worker-local secret-reference headless input, with the upstream agent-desktop CLI available separately through ordinary `exec.run`.

Reject any legacy top-level `rpa` key during configuration decoding, including `null`, with explicit removal and migration instructions. Do not silently disable access, translate selectors, rewrite retained tasks, or replay unfinished work under another capability. Native desktop has no action batches, screenshot-artifact import, secret-reference keystroke typing, or physical input. Broader CLI commands do not gain native masking, secret resolution, desktop admission or locking. Unsupported protected input still requires manual login or approved setup, never shell credential retrieval.

Preserve the one-slot native desktop pool, queue observations, task/lease/maintenance behavior and cross-profile per-user file lock. Keep the historical `control/rpa.lock` filename so profiles running different Control versions cannot bypass serialization. Rename internal lock bookkeeping without moving or deleting that lock. This supersedes D040, removes the RPA portion of D041/D123, and retains D102/D103's native security contract. See [migration and feature differences](desktop.md#migrate-from-rpa).

## D127: Full agent-desktop command routing through Control

Support the full installed upstream CLI through `desktop.run`, configured with an absolute `desktop.cli` executable path. Forward structured arguments and optional non-secret stdin without a shell or Control-maintained command whitelist. A narrow native binding cannot provide upstream command parity, exact app/window discovery, system surfaces, drill-down or future commands without ongoing duplication. CLI routing preserves upstream behavior and platform limitations while making all installed features operable from Control's CLI, generic calls, MCP desktop tool and durable tasks. A library-only profile continues to provide native commands; discovery must not advertise an unconfigured backend.

Retain D102/D103's private-pipe native secret input, and allow `cli` alongside `library`. Both configured backends share desktop admission, the per-user lock, profile-private default state, authorization, leases, maintenance protection and task reconciliation. Strip inherited upstream home/session/tracing variables, set the private home explicitly, and do not mix explicit-session or other-profile refs with native sessionless refs. CLI calls retain caller/task deadlines and process-tree cancellation, not a native two-minute limit. This is a one-shot command interface; persistent upstream MCP serving belongs in the existing MCP integration.

Bound argv/stdin and captured stdout/stderr. Never stream raw CLI logs to tasks. Redact complete textual envelopes and diagnostics before publication, preserve upstream failures and partial results, and fail on nonzero exit even with a contradictory successful envelope. Upstream batch envelopes can report success despite failed entries; fail Control execution for failed/unstarted batch entries while retaining the original envelope so dependencies cannot mistake partial completion for success. Do not resolve worker secrets into CLI arguments or stdin. CLI inputs remain task-visible; protected credentials still require native secret-reference input. Binary screenshot/export bytes are not redacted and use declared task outputs and existing artifact publication. Upstream traces and state files can retain private data. Full functionality is not a sandbox, permission to use the clipboard, capture credentials, request OS grants or synthesize physical input, nor permission to replay uncertain effects. Installation and dependency verification remain explicit. This supersedes only D102's narrow surface and D126's ordinary-execution-only routing for full CLI commands, not their credential, isolation, CGO-free build or no-replay constraints. See [full setup and contract](desktop.md#full-upstream-command-surface).

## D128: agent-browser CLI and loopback dashboard forwarding

Add opt-in `browser.run` with an absolute configured agent-browser CLI and optional trusted upstream config. Require stable 0.41.2 or newer, checking the binary before each command so older CLIs cannot silently ignore namespace/configuration settings. Forward the installed command surface rather than duplicating browser actions in Go. Use structured bounded argv/non-secret stdin, normal execution admission, an owner-specific file lock, private default home/runtime/namespace, explicit configuration instead of workspace discovery, captured/redacted text and no raw task logging. Preserve upstream JSON, exit status and failed batch outcomes. Do not substitute Control secrets into CLI input or imply that live frames and upstream activity are redacted.

Reuse agent-browser's bundled dashboard through a loopback-only client-owned TCP forward for remote human viewing/input. Keep dashboard startup explicit and separate from opening a view, preventing a view request from silently starting services or replaying login actions. Pin standalone destinations by immutable identity and preserve existing `tcp.open` authority, leases, expiry, revocation and stream cleanup. Do not introduce gateway browser endpoints, a custom streaming protocol, a public port, or another frontend build dependency. The dashboard controls all sessions in its namespace; this first integration does not promise single-session sharing or enforced human/AI takeover locks. Worker dashboard ports must be distinct for concurrent owners.

Upstream owns detached browser/dashboard processes; a completed command or closed forward is not browser shutdown. Require explicit session close/dashboard stop and document idle expiry, restart survival and managed-update limits. Dependencies remain optional, separately installed after approval, and outside Control release bundles. Default state separation is not an execution sandbox; explicit upstream flags and unrestricted providers retain OS-user authority. See [browser setup and lifecycle](browser.md).

## D129: Opt-in compact and named client discovery

Keep plain CLI `machines` and MCP `control_nodes` full JSON compatible. Add optional compact projection and exact case-sensitive name/stable-ID filtering through one shared client implementation. Filtered results remain arrays, full records retain unknown fields, and unknown targets fail without fallback. Compact records retain identity, policy, presence and Hybrid pairing while omitting repeated schemas and resource/network details.

Project the existing fleet-scoped directory response locally rather than requiring a new gateway protocol or execution-worker connection. This reduces tool output, not gateway wire bytes. Discovery does not refresh resources or confer execution authority, and includes offline, disabled and System contexts without scheduling them. Reuse description resources and runbook catalogs within the current sequence, preserving explicit refresh and exact-source read safeguards. See [discovery reference](protocol.md#gateway-observation).

## D130: Inline task results bypass artifact-publication admission

Reserve output-publication capacity only for successful declared-output imports or JSON results above the existing 1 MiB inline limit. Small results require no artifact I/O and must not wait behind unrelated transfers. Keep independent JSON capture under provider admission, large-result artifact bounds, durable task completion, ownership, leases and maintenance protection unchanged.

Cancellation while awaiting output publication retains captured inline diagnostics but does not import outputs or bypass admission for a large result. Large unpublished results may still be discarded, and no provider is replayed. This refines D124's admission condition and cancellation-result behavior. See [worker concurrency](protocol.md#worker-concurrency).

## D131: Lease-sensitive upload and clipboard admission

Reject new `files.upload.open`, `clipboard.open` and `clipboard.paste` streams while the serving node has an unexpired execution lease or an expired lease still held by unfinished leased tasks. Apply the rule to every caller, including the lease owner and local API. Uploads and clipboard operations can publish workspace files or interact with shared desktop state outside provider admission. Artifact operations and TCP access retain D010's existing exemptions; relaying bytes alone does not reserve execution capacity.

After authorization and maintenance admission, wait cancellably for pending lease persistence, then check lease state and reserve an active invocation under the same node mutex used by lease acquisition. A stream admitted first makes lease acquisition fail as busy. A lease committed first rejects the stream. Failed lease persistence leaves the previous state authoritative. Never hold the mutex during clipboard, disk or network I/O. Release the reservation once, after the operation's owned I/O and cleanup finish, including verification, publication and completion replies.

Do not preempt in-flight operations to grant a lease. Existing cancellation, authority expiry, deadlines, shutdown and uncertain-effect reconciliation remain unchanged. Inactive upload partials do not reserve execution capacity, but resume attempts require fresh admission. Status and owner-authorized abort remain available during a lease. All upload-open requests pass the gate, even retained-receipt requests; use status to reconcile during a lease.

Keep stream framing and request schemas unchanged, with no streaming `leaseId` or owner bypass. Return `node_leased` on a lease rejection without authorizing automatic retry. Upload before acquiring the lease and use clipboard operations before acquiring or after releasing it. Older workers do not enforce this restriction; upgrade every relevant worker before relying on it. Leases are node/profile-wide, not host-wide locks across Hybrid contexts, humans or unrelated OS processes. See [lease policy](protocol.md#shared-pool-leases), [uploads](uploads.md) and [clipboard permissions](clipboard.md#permissions).

## D132: Single-process services and temporary update handoffs

Its retain-the-legacy-wrapper migration policy is superseded by [D133](#d133-retire-legacy-windows-supervisors-and-observe-migration). The single-process and temporary-helper contract remains unchanged.

Run gateway and node services in their entry-point process rather than keeping a Control supervisor and child alive. Preserve existing update authority, executable verification, idle reservations, gateway relay drain, persisted selection, state locks and no-replay rules. There is still a brief service interruption; memory and live connections are not migrated.

After graceful cleanup, Unix services revalidate and persist the selection and use `exec`, retaining the PID, including container PID 1, execution account, arguments, environment, working directory and standard streams. Release the old runtime lock immediately before exec; the new image reacquires it before opening state. A competing startup can win the lock but cannot overlap service ownership. Exec/startup failures are explicit and retain manual recovery rather than retrying application effects or automatically rolling back state.

Windows uses a temporary updater launched from a verified versioned source. Before shutdown it opens the original process handle and checks the exact profile's restart registration and permissions. It requires explicit commit after clean shutdown and waits for process exit and the runtime lock before rechecking request/current selection, persisting the switch and replacing the stopped stable runtime. Start SCM, Task Scheduler or a process once, wait for a unique selected-checksum readiness marker after startup checks/listener binding, then exit. Preparation and readiness waits are bounded. EOF without commit and explicit stops do not authorize restart. Failure to escape an enclosing process job rejects preparation before service shutdown.

Install SCM and User tasks against the stable runtime directly. Grant each restricted SCM service SID start/query rights only to its own service, in addition to its existing retirement rights. User tasks directly run a GUI-derived copy with only the PE Subsystem field changed. Full-image comparison proves that derivation before software identity reports the original release checksum; ordinary executable inspection remains byte-accurate. Keep original verified sources private and versioned, including the initial selection, independently of CLI self-updates. Firewall paths and installation ownership stay unchanged.

This supersedes the resident supervisor in D014/D021, the persistent User wrapper in D033/D063, and D046's supervisor-owned publication, not their admission, account or firewall boundaries. Existing `__managed` children retain the old request/EOF behavior until a current installed entry point and startup registration are explicitly refreshed. This requires one local restart, or a current container image, rather than silently detaching from an older service manager. See [update handoff and migration](updates.md#single-process-runtime-and-update-handoff).

## D133: Retire legacy Windows supervisors and observe migration

Do not acknowledge a new runtime as fully updated while a reported legacy supervisor remains. Add optional software `runtime` observation, distinguishing `supervised` from `single`, and require migration before current-version acknowledgement. Exact byte validation remains independent of this runtime observation. Omit it from reconstructed legacy registration signatures. Software predating the observation retains compatibility, not a false assertion about its process topology.

User handoffs use a separate triggerless, time-bounded Limited task under the same interactive user. A current-user-only local named pipe rejects remote clients and authenticates a random token before carrying preparation and explicit commit. Remove the temporary definition before committing; its running instance exits after runtime readiness. Do not depend on Task Scheduler permitting process-job breakaway. SCM and standalone helpers retain their enclosing job containment.

At Windows legacy-child startup, before opening work admission, bind the remembered installation/identity, selected executable, parent process handle/image and exact startup definition. Preflight existing OS authority. Exit without creating the old supervisor's restart request, allowing that parent to finish naturally. The temporary helper waits for parent exit and takes installation/runtime locks, rechecks identity and selection, publishes the stable copy, changes only the owned startup action and starts once. The migration record is separate from the old child-restart request. A known User logon task preserves principal, triggers, settings and ACL; an unchanged-definition write tests modification authority without executing or changing its action, and registration triggers are rejected.

Automatically migrate User tasks and already-authorized SCM contexts such as LocalSystem companions. Do not self-elevate or grant LocalService `SERVICE_CHANGE_CONFIG`, which could change the service account to LocalSystem. Older restricted SCM registrations report an actionable privileged refresh requirement while remaining available, not rollout completion. Foreign or unsupported registrations are never repurposed. Old Unix/container parents still need their own entry point refreshed; a child cannot exec its parent or replace an old PID 1 without container shutdown. D137 supersedes the manual-refresh requirement for [supported launchd/systemd jobs](#d137-bound-startup-validation-memory-and-migrate-owned-legacy-service-jobs). This supersedes only D132's legacy-wrapper retention policy. See [migration](updates.md#migrate-an-existing-supervisor).

## D134: Separate software compatibility from runtime migration

Use the running selected release to decide whether the gateway can coordinate node updates without separate staged compatibility metadata. Use both the selected release and runtime topology to decide whether installation is complete. A reported legacy supervisor must not make already-selected gateway code appear unavailable for protocol coordination. Combining those checks leaves supervised nodes occupying both commit slots while they wait for a supervised gateway that itself waits for those commits.

Keep D133's refusal to acknowledge retained supervisors, strict executable validation, idle reservations, and durable commit-intent reconciliation. Allow migration preflight to finish or report an explicit unpaused error through the existing commit protocol, so blocked migrations do not starve other nodes. Do not infer success or discard uncertain intents from a version match alone. This clarifies D133's current-version observation and supersedes its use as a compatibility gate. See [migration](updates.md#migrate-an-existing-supervisor).

## D135: Settle software switches separately from startup blockers

Its timestamped dirty-version suffix is superseded by [D143](#d143-keep-development-version-labels-free-of-build-timestamps). The startup-blocker behavior is unchanged.

A fresh report of the exact selected executable version and checksum, released maintenance, a non-restarting state, and `runtime: "supervised"` proves the software switch settled while explicitly denying startup-migration completion. Clear only that software commit intent and stop re-offering/recommitting the same bytes. Never infer this from a version match alone, stale status, paused maintenance, an in-progress restart, or another deployment. Keep unresolved intent and gateway-switch publication barriers unchanged.

Show `startup-refresh-required` per node. When all available software switches and the gateway have settled but these startup blockers remain, persist deployment phase `blocked`, not `complete`, and retain recovery binaries. This allows other nodes and later superuser-published fixes to proceed without restarting blocked profiles or falsely claiming their supervisors disappeared. It refines D133/D134's migration acknowledgement and avoids retrying permanent startup limitations as binary updates. Dirty `make update` builds receive a timestamped version shared by the native CLI and bundle, so fixes do not silently reuse pinned dirty bytes.

## D136: Preserve Limited User task maintenance authority

After registering an owned Windows User login task, preserve its ACL and ensure the installing SID has file-generic read/write/execute access on that task. An elevated creator otherwise leaves Administrators as owner with only a read grant for the Limited principal, preventing the unchanged-definition preflight and startup migration despite a correctly bound installation. Do not grant ownership, ACL-change rights, another principal, a broader task-folder ACL, an elevated execution token, or SCM change-config authority.

Already-installed read-only tasks require an explicitly authorized privileged repair bound to that exact profile, SID, Limited principal, action, and logon trigger. Preserve other permissions and startup definitions and do not start or stop the profile as part of that repair. A subsequent ordinary managed update can perform the existing committed migration. This clarifies the existing-authority requirement in D133 without allowing a Limited worker to elevate itself.

## D137: Bound startup validation memory and migrate owned legacy service jobs

Its blanket systemd drop-in refusal is superseded by [D138](#d138-validate-inert-vendor-systemd-drop-ins-during-legacy-migration). The remaining identity, authority and restart constraints are unchanged.

Verify a Windows GUI runtime by streaming the complete source and derived images with fixed buffers, hashing original bytes before normalizing the two Subsystem bytes. Preserve full-image equality, PE validation, source software identity and actual-file checksum inspection. Whole-image `io.ReadAll` in the persistent User process caused a large startup allocation spike on every User profile, independently of task history. Allocation benchmarks and the native GUI-startup fixture guard this boundary; forcing garbage collection or trimming the working set would hide rather than fix it.

A standard macOS launchd or Linux systemd job can migrate without a persistent helper or manual service cycle. Before work admission, bind the remembered profile/identity, selected checksum, exact owned definition, loaded job and supervisor PID/start time/account/image. Systemd also requires matching cgroup membership, no drop-ins and no pending daemon reload. Under installation, binary-registry and CLI-update locks, atomically refresh only its startup executable with the verified selection, preserving mode/ownership/extended attributes and retaining original bytes. In particular, retain the original SELinux label rather than inheriting a non-executable staging label. Return a deliberate failure so the old supervisor propagates it and the existing restart-on-failure job reopens the same startup path. Cancellation, foreign/custom jobs, stale bindings and missing authority never authorize restart. Do not modify registration, change accounts, delete state or equate replacement with readiness. Require fresh `single` observation to finish the deployment.

This supersedes D133's blanket manual-refresh requirement only for supported launchd/systemd jobs. Containers and unsupported legacy startup retain explicit refresh/cycle recovery. CLI self-update remains independent of ordinary managed updates; this one-time migration refreshes the remembered startup executable because an old parent cannot exec itself on a child's request.

## D138: Validate inert vendor systemd drop-ins during legacy migration

Do not reject an otherwise standard owned systemd job solely because its distribution supplies the service-wide `TimeoutStopFailureMode=abort` override. Fedora supplies it for both User and System managers. Keep exact unit, process, account, restart policy, cgroup and no-pending-reload checks. Accept at most eight bounded, root-owned, non-shared-write regular files under the matching `/usr/lib/systemd/{user,system}/service.d` directory, with protected directories and no symlinks. Parse only comments, `[Service]` and that exact timeout-abort setting; reject execution hooks, other sections/directives, account/environment changes and custom paths.

Bind the loaded path set to content checksums and revalidate it before atomically replacing startup. Never delete, rewrite or reload an override to make migration pass. This narrows D137's drop-in restriction without treating arbitrary root-owned configuration as a supported startup contract or weakening SELinux metadata preservation. Tests cover the real vendor shape, unsafe directives, ownership and write permissions, symlinks, size bounds and unmatched loaded paths. Native opt-in checks inspect the distribution files read-only; final migration still requires observed single-process readiness.

## D139: Portable case-folded task ID exclusion

Keep public task IDs case-sensitive, but reject a different ASCII-case spelling while its folded ID belongs to a pending acceptance, retained task or tombstone. Exact-ID owner/specification checks remain unchanged. Metadata, logs and workspaces already use the public ID in their paths, so accepting `Build` and `build` independently can overwrite recovery records and mix owners' logs on case-insensitive filesystems. Apply the exclusion on every OS rather than relying on host-specific filesystem detection.

Maintain the folded index under the same node mutex as task acceptance. Failed acceptances release it; pruning transfers protection to the tombstone; successful compaction releases forgotten IDs only when no task or acceptance still holds them. Startup rejects conflicting retained spellings before rewriting interrupted tasks or compacting the journal, preserving evidence for explicit offline recovery. Existing conflicting histories on case-sensitive storage also require recovery. Do not lowercase public IDs, merge owners or specifications, migrate workspaces automatically, discard outcomes or replay tasks. This refines D007/D058/D075 without changing durable acceptance, retention periods or the on-disk layout. See [task IDs](protocol.md#task-specification).

## D140: Lifetime-aware grant cleanup and submission receipts

Release client-prepared temporary authority after synchronous-only calls, including error and cancellation. Preserve every grant potentially backing an accepted durable parent, including single-use instructions, and task grants backing synchronous workflows' children and outputs. A provider's completion does not end resumable output access. Each cleanup batch shares a five-second deadline independent of caller cancellation; failures are diagnostic, never permission to replay work.

Concurrent or repeated submissions keep independent fresh grant sets. Generate an acceptance receipt inside each task submission decision and return it only in that RPC's envelope, bound to a fresh request ID. An acknowledged duplicate or pre-adoption rejection proves that attempt's grants were not adopted, allowing their cleanup without touching the winner. Forwarded task submission preserves the child's decision. Never infer non-adoption from timestamps, a preflight read, `activeTasks == 0`, an arbitrary error, or a missing reply. Lost and legacy receipts retain possibly adopted authority until existing expiry or explicit owner reconciliation. Receipts are not persisted, and no automatic submission retry or shared deterministic grant ID is introduced.

Centralize instruction, task and artifact-access admission/removal under the destination's grant mutex. Keep the 4,096 global count limit and add a configurable 1,024-per-owner default. Reclaim expired entries before checking capacity without evicting live records. Listing and revocation remain available when saturated. This refines D045/D087's automatic client grant lifetime and count bounds without changing exact instructions, owner/subject binding, durable task lifetime, artifact deadlines, or restart recovery. See [delegation](delegation.md) and [receipt contract](protocol.md#instruction-bound-delegation).

## D141: Recover observation and publication without replaying effects

Keep failed tombstone appends separate from adopted cleanup intent. An append error may have persisted some or all bytes, so retain the affected task records and block further appends until a complete journal rewrite, including directory sync, succeeds. Preserve strict interior-corruption rejection. Protect task-output artifacts under their import lock until terminal references become visible; failed terminal persistence does not promise recoverable results.

Let a later explicit client request retry failed backend initialization, coalescing concurrent setup without replaying an operation or changing a selected backend's authority. The initiating caller owns cancellation; independently cancelled waiters do not. Shutdown cancels and joins setup. Completed forwards leave active capacity automatically, with only bounded completed-ID acknowledgments retained.

Workflow status reads may retry transient local transport failures under the existing workflow deadline. A permanent observation error retains the accepted child identity and stops new scheduling without inventing a terminal child outcome or cancelling healthy work. Only workflow cancellation, timeout or revocation triggers cancellation cleanup. No task start, grant or provider effect is automatically replayed.

Bind console responses to the originating session and form/query generation. One-time credential issuance blocks ordinary dismissal while pending, but session expiry still removes the form and prevents stale credential display. Audit polling resets to a continuous newest page and valid cursor when it cannot merge without a gap. These choices refine D007/D075 and the existing client and console lifecycle contracts without adding a task state or changing wire schemas.

## D142: Invocation-owned one-shot command descendants

Terminate ordinary descendants of `exec.run`, `agent.run` and configured subprocess providers before releasing invocation admission, on completion as well as cancellation. These are one-shot commands, not daemon launchers. Persistent services belong to an OS service manager or an explicit persistent adapter. Browser/desktop adapters and configured MCP sessions retain their separate documented lifetimes.

Re-execute a temporary helper from the running binary. Its private status channel reports the requested program's exit code independently of helper termination. A private gate prevents requested execution until ownership is established and detects parent exit. Unix keeps the helper as a live process-group leader until the parent kills the group before reaping it, preventing process-group ID reuse. Windows assigns the gated helper to a parent-owned kill-on-close Job Object before opening the gate. Nested job assignment failure rejects the invocation; there is no uncontained fallback, breakaway flag or elevated launch.

In Control the helper entry point runs before profile and account initialization; the private dispatch also supports test executables. Gate/status handles do not pass to the requested program. Output-copy goroutines are bounded and joined; incomplete output remains a failure. The helper exists only for its invocation, not as a resident node supervisor or updater.

This refines D006's subprocess lifetime rather than introducing a sandbox. Unix programs can escape the group with `setsid`; externally managed processes have independent lifetimes. Arbitrary programs retain the node account's OS authority. Preserve exit status, cancellation, no-replay rules and CGO-free builds. Native Linux, macOS and Windows tests must exercise descendant cleanup, parent exit and Windows enclosing-job compatibility.

## D143: Keep development version labels free of build timestamps

Preserve Git-derived and explicit version labels unchanged in `make update`, including `-dirty`. Build time remains separate metadata rather than a long suffix in CLI, dashboard and machine versions. This supersedes only D135's automatic dirty-version timestamp and retains D017's source-based labels and D047's same-version update boundary. Commit changed source or select a new explicit `VERSION` for another development rollout.

## D144: Conditional directory reads and bounded relay reuse

Give each committed fleet directory an opaque ETag. Compare serialized node digests when publishing directory changes, including nested metadata, while health/update-status-only changes update telemetry without rehashing nodes or invalidating the validator. Authenticate conditional requests normally and return 304 before copying or encoding unchanged records. Direct responses are private and require revalidation, varying by Authorization and Cookie; console responses retain their stricter no-store policy. Keep only per-node digests and per-fleet validators, not encoded bodies or per-client gateway caches. This extends D073's committed observation view without weakening fleet isolation or turning validators into authority.

Only the node's LAN watcher retains a conditional discovery snapshot, projected to the fields LAN discovery uses after validating every record's fleet. It revalidates every ten seconds, clears its snapshot when the fleet or directory generation changes, and recomputes candidates after 304 so proof expiry is unchanged. General directory reads, lookup/membership checks and execution authority do not consume this snapshot. Old gateways may continue returning full arrays without validators. [D164](#d164-receiver-scoped-lan-discovery-view) supersedes the full-directory polling path for compatible gateways.

Use comparable relay-session keys and FIFO rings that grow only to the existing 768-packet session bound. Keep at most one drained ring of eight slots or fewer per destination, with no packet references or identity key retained in that spare. Release larger drained rings. Preserve D077/D101's packet/byte admission, fairness, offline barriers and write-lifetime accounting. Ordinary WebSocket scratch buffers still reuse up to 128 KiB; readers detach larger storage on return, and the gateway writer releases oversized marshal storage after synchronous write completion, including failure. These changes reduce allocation churn without promising lower idle RSS or different bandwidth guarantees.

## D145: Worker-local command secret injection

Extend `exec.run` with `envSecrets`, mapping up to 32 portable environment variable names to local secret names, and `stdinSecret`, supplying one secret's exact bytes instead of literal stdin. LDAP/RPC programs and cloud SDKs can consume stored credentials without returning their values to an orchestrator or requiring another interactive password entry. Validate references before launch and resolve from one snapshot at execution, after queueing. Never mutate persisted task arguments, delegation instructions or idempotency comparisons. No argv substitution, temporary credential files or value-read API is added.

Keep resolution in the worker provider and reuse command process ownership, cancellation, admission, leases, task ownership and authorization. Environment injection overrides inherited values only for that invocation. Reject literal/reference overlaps and case-folded duplicate variable names on every OS. Programs and their descendants receive plaintext through the selected process channel; this is ordinary application credential use, not isolation from that program or the OS account.

Capture secret-bearing stdout/stderr completely within the existing 2 MiB per-stream bound, without raw task logging. Mask all snapshot values and common encodings before returning results, errors or completion-delayed logs. Decode JSON strings before masking when the stream is JSON. Suppress both streams and report failure for truncated or non-UTF-8 output rather than leaking a boundary fragment. Such failure cannot undo effects or authorize replay. Commands without references retain their existing live raw logs. Files, artifacts, arbitrary transformations and application-managed logs remain outside text masking.

This supersedes only D041's GUI/HTTP-only resolution restriction. Secret administration remains a separate owner-management operation; command use confers no management or peer authority. Callers must discover the added schema fields and upgrade older workers before use, since old receivers ignore unknown fields. See [command secrets](secrets.md#authenticate-commands-and-sdks-by-reference).

## D146: Explicit worker proxy for isolated machines

Let a child that cannot reach the gateway install and run through one explicitly selected CONNECT proxy on a same-fleet worker. The proxy is separately managed software; Control does not install it, open firewall rules, switch routes, chain proxies, or replay work. Scripts and binaries stay on the gateway and stream through the proxy without a per-invitation proxy cache. The child keeps its canonical HTTPS gateway URL, identity and credentials, and verifies gateway TLS end to end.

Workers advertise explicit `proxyEndpoints` in their signed registration, never every interface, and report bounded CONNECT, TLS and `/healthz` checks in the existing owner health stream on their own interval. Advertisement is separate from readiness; observations older than three minutes are stale and never selectable. The gateway does not probe endpoints: it cannot prove child reachability, and probing would make it an arbitrary URL fetcher. Readiness therefore proves proxy-to-gateway access only.

Invitation creation takes a stable worker ID and one advertised endpoint, never an arbitrary URL, and validates fleet membership, presence and fresh readiness under the gateway lock before writing state. A `{url, worker}` snapshot persists in the existing transactional invitation JSON, so renames and address changes never rewrite invitations or installed profiles. Clients require the gateway's `gatewayProxy` feature and an echoed selection, and never retry an uncertain creation.

Generated scripts pass the endpoint explicitly to every download and enrollment stage, using curl `--proxy` with an empty `--noproxy` and PowerShell `-Proxy`, rather than relying on environment inheritance through sudo, UAC or limited tokens. After checksum verification, the script requires the binary's declared `__installer-features` capability, not a version guess. Enrollment requires `--gateway-proxy`, and proxied redemption proofs sign the endpoint, so older binaries fail before consuming a ticket instead of enrolling directly. Direct invitations keep their scripts and signature bytes.

The route persists in pending enrollment and commits to the profile as `gatewayProxy` before service startup; retries reuse it, and re-enrollment preserves it unless explicitly replaced. At runtime every gateway request uses a fixed-proxy transport without fallback or global default changes, and the network monitor watches the proxy route. Proxied nodes skip direct GitHub downloads. A proxy outage is a gateway outage: direct WebRTC sessions survive, interrupted streams fail without replay. [D147](#d147-gateway-owned-lifecycle-for-managed-proxy-listeners) adds an optional managed listener, and [D149](#d149-coordinated-proxy-first-managed-rollouts) adds its coordinated update drain. Failover and proxy chains remain separate work. See [install through a worker proxy](gateway-proxy.md).

## D147: Gateway-owned lifecycle for managed proxy listeners

The dependency-triggered activation and final-dependency shutdown portions are superseded by [D148](#d148-owner-managed-explicit-proxy-settings). The dependency records, retirement acknowledgement, fixed CONNECT target, and maintenance bounds remain.

Its indefinite update deferral for idle connected children is superseded by [D149](#d149-coordinated-proxy-first-managed-rollouts). The zero-open-CONNECT maintenance fence and refusal to cancel admitted traffic remain.

Offer an opt-in managed endpoint as an alternative to external proxy software. A worker runs Control's own CONNECT listener on an explicit private/VPN IP, separate from the local API, admitting only configured private source CIDRs and only `CONNECT` to the canonical gateway authority. It pipes bytes, so gateway TLS stays end to end. It is not a general proxy, and Control still never starts, stops, or configures external proxy software or other processes. D146's external endpoints are unchanged.

The gateway, not the worker or child, owns the decision to run the listener because only it sees every dependency durably and fleet-scoped. Pending dependencies are computed from unrevoked, unredeemed, unexpired managed invitations, so revocation and expiry release them without mutation. Enrolled children cannot rely on invitation records, which history clearing hides, Hybrid cleanup deletes, and re-enrollment revokes. Each redemption through a managed endpoint therefore writes a record keyed by the child's immutable identity in the same transaction. Offline children count; disconnection is never evidence of retirement.

Unregistration revokes credentials, but the record remains until the child proves it durably applied retirement: a signed, non-inspect `/v1/node/state` read naming the exact retirement revision, sent after the node persists that state. The existing first read cannot serve, since it carries the previous revision and its response may be lost. The proof is bound to the signer's own identity, so a child cannot release another child or fleet. Nodes retry the acknowledgement within a small bound and repeat it on every later start. A child that never returns would hold the listener indefinitely, so the fleet owner can explicitly release an unregistered child's record.

Selecting a managed endpoint before any child exists must work while the listener is off. The worker reports `standby` after verifying the direct gateway path, and creating the invitation activates the listener; no per-ticket binaries or scripts are prepared. The response reports `pending` until the worker's own check through the running listener is `ready`, and clients only observe, never retry creation. Workers read desired listeners through a separate signed read-only endpoint on the existing policy hint and poll. They persist the decision so restarts restore children's access before the gateway is reachable, and keep current listeners on read failure.

Deactivation closes only the listening socket; admitted tunnels end with their peers or node shutdown. No idle-byte timeout applies. Admitted connections, including pending handshakes, block node updates and startup changes through an atomic fence alongside the work gate. A proxy worker therefore postpones its own updates while children stay connected. That cost was accepted rather than force-closing children's gateway sessions. A route mesh, proxy chains, failover, and authenticated proxies remain out of scope. See [managed listener](gateway-proxy.md#managed-listener).

## D148: Owner-managed explicit proxy settings

Make gateway proxy hosting a remote machine setting. The fleet owner selects a live worker interface/address, port, and explicit private source CIDRs through fixed GET/PUT management routes. Reuse the bounded gateway-only management channel used for startup and secret administration. The worker discovers native interfaces, validates address assignment, binds and persists the listener, and confirms application. No shell, execution grant, credential read, elevation, firewall modification, or node restart is involved.

Enablement is worker-local durable desired state, independent of dependency count. Enabled listeners run with zero invitations or children, including existing managed profile endpoints. Only explicit disable stops them. The first managed profile endpoint becomes owner-editable; external and other endpoints retain their configuration. Disabled endpoints are not advertised as selectable.

Gateway dependencies remain a safety condition for disable/reconfiguration, checked under the invitation transaction lock. A durable gateway-owned uncertainty fence blocks new managed invitations while a mutation is outstanding, including after gateway restart. Worker mutations refuse active CONNECT work and maintenance reservations. Bind or persistence failure retains the old listener. A lost reply is uncertain; a later serialized inspection reconciles actual settings without replaying the mutation. Metadata updates and health checks publish the new endpoint without disrupting peer sessions.

This supersedes D147's automatic activation and shutdown lifecycle. It trades automatic resource reclamation for an explicit setting the owner can inspect and control. Interface declarations and successful application do not prove child reachability; readiness still comes from the worker's bounded CONNECT/TLS health check. See [managed listener](gateway-proxy.md#managed-listener).

## D149: Coordinated proxy-first managed rollouts

Treat idle children's permanent gateway connections as drainable connectivity, not an indefinite reason to retain an obsolete proxy binary. The gateway groups children from same-fleet enrollment dependencies and signed configured routes. It waits for fresh idle reports and persisted destination-owned reservations, then fences only their new relay carriers and asks outgoing owners to drain existing carriers. Children with negotiated support park their own gateway HTTP/WebSocket traffic only after response bodies, relay links and gateway setup have drained. No proxy socket is force-closed and no application operation is replayed.

Keep the proxy's existing actual zero-CONNECT fence, fresh reservation and durable commit intent. Update it first, then let children reconnect, release their holds after fresh proxy confirmation, and require acknowledged resumption before child software updates. These reservations are not software commits, do not consume commit slots, and cannot authorize a child commit. One group coordinates at a time with at most 256 children; unrelated nodes continue independently. A 45-second attempt, two-minute reservations and at-most-30-second route parks bound coordinator loss and failure. The coordinator may reconstruct reservations only from fresh child acknowledgements; an uncertain proxy commit still requires the existing reconciliation.

This supersedes only D147's accepted indefinite update starvation. Existing proxy executables can participate through their original socket fence, but the gateway and children need the new coordination support. Legacy children update independently first to gain it. Offline/unreserved children and unknown CONNECT clients remain conservative blockers. External proxy software, startup changes, routing failover, TLS verification, ownership and execution delegation are unchanged. See [managed proxy updates](gateway-proxy.md#updates).

## D150: Browser-local custom machine order

Store optional console machine order in browser local storage, scoped by gateway origin, authenticated fleet ID and role. Persist node IDs and gateway-issued Hybrid IDs rather than names or selected context IDs. This keeps positions stable through renames and User/System chip changes without changing directory records, gateway APIs, or worker selection.

Use the existing platform icon as a pointer/touch drag handle, saving immediately on drop, with Alt+Up/Down for keyboard reordering. Keep Overview and Machines consistent without an ordering dialog. Freeze row rendering during capture, show the insertion point, and auto-scroll at the list edges. Reorder only visible slots so hidden machines retain their positions; reconcile identities with the latest snapshot before saving. New machines follow saved entries in name order; absent entries are ignored. Column sorts are temporary and do not erase the preference. Invalid storage falls back to name order, and failed writes leave the existing order unchanged. Preferences survive sign-out but do not sync across browsers. See [custom machine order](console.md#custom-machine-order).

## D151: Automatic atomic Hybrid companion renames

Renaming a Hybrid User node renames its registered same-fleet System companion to `NEW_NAME-system` automatically through every client. Pair by gateway-owned Hybrid identity and roles, never by alias suffix. Repair existing alias mismatches rather than requiring the old System alias to match the User alias. Missing companions do not block a single-node rename; ambiguous pairing is rejected. Direct System renames remain independent.

Validate both destination names, including client identities and invitation reservations, before changing either. Commit both directory records, routing overrides, audit events and explicitly forced pending-invitation retractions in one SQLite transaction. Roll back all in-memory changes on failure and notify both nodes only after commit. Retain signed installation proofs, admission revisions, credentials and established sessions.

The console's Rename action edits the host's base name from either context chip and submits one User rename, not two sequential mutations. This supersedes only D087/D089's independent rename behavior. Other lifecycle actions, execution targeting, privileges and task stores remain separate. Project-local runbook migration remains limited to the node explicitly selected by the CLI or terminal dashboard. See [rename protocol](protocol.md#gateway-observation) and [Hybrid console actions](console.md#hybrid-rows-and-counters).

## D152: One release signature bundle with a short filename

The Sigstore format and filename are superseded by [D153](#d153-pinned-ed25519-release-manifests). Publishing only one manifest signature and retaining unsigned installer checksums remain unchanged.

Publish only `control-manifest.sigstore` as the JSON Sigstore bundle for `control-manifest.json`. Stop publishing a separate checksum signature because Control's consumers authenticate binary hashes through the signed manifest. Keep the unsigned `checksums.txt` for bootstrap installers and manual checksum checks. This refines D078's release asset contract without changing its signature, identity, transparency-log, or trust-root verification.

Use the new filename consistently in release publication, CLI self-updates, gateway release ingestion, and managed nodes' GitHub downloads. Given the small installed base, use a clean rename rather than retaining a legacy alias. Migrate existing installations through a one-time reinstall or development push. See [release publication](updates.md#publish-github-releases).

## D153: Pinned Ed25519 release manifests

Replace Sigstore with standard-library Ed25519 verification and source-controlled public keys scoped by exact repository and key ID. Publish a bounded versioned `control-manifest.sig` over the exact manifest bytes and NUL-delimited purpose/repository/tag/key-ID context. Reject malformed envelopes, unknown pins, changed bytes, wrong context, and missing signatures before public executable download or execution. Keep binary hashes, executable validation, superuser development publication, authenticated gateway fallback, and idle rollout admission unchanged.

The small installed base permits a clean cutover without a legacy verifier. This removes Sigstore's binary/dependency cost and online trust-root availability requirement, but gives up workflow-identity proofs, transparency records, and trusted signing time. A dedicated long-lived private key needs protected CI custody, encrypted offline backup, staged overlapping pins for planned rotation, and trusted reinstall/development-push recovery after loss or compromise. Old binaries cannot be forced to revoke a stolen key. Complete rollback/freeze protection and bootstrap authentication remain separate work.

Build unsigned assets and the signer without secrets. Use a separate protected-environment publication job, expose the PKCS#8 secret only to a stdin-signing step, verify against compiled pins before writing and publication, and never ship signing tooling or private keys as release assets. Keep the initial keyring empty rather than generating a private key or trusting a fixture during implementation. Publication fails until an operator provisions a reviewed public pin and CI secret. This supersedes D078's Sigstore choice, D098, and D152's signature format/name. See [release signing and recovery](updates.md#release-signing-keys).

## D154: In-process resource sampling

Periodic resource samples must not start processes or enumerate every host process. Measured on a Windows host with a discrete GPU, each PowerShell/WMI GPU-memory query cost 250-440 ms of CPU, four times a minute in each Hybrid context. That was about ten times the node's own idle CPU. Windows nodes now read `\GPU Adapter Memory(*)\Dedicated Usage` through PDH, keeping one wildcard query open per process. A reused query costs microseconds and returns the same adapter-wide values, joined by LUID as before. It works for User and LocalSystem contexts. PowerShell remains a fallback for a sample where PDH fails, and the query reopens on the next sample.

Platform and kernel versions are read once per collector, and only uptime is read each sample. `host.Info` also enumerated all processes and, on macOS, started `sw_vers` every sample; Control did not report the extra fields. A changed OS version is shown after the next node restart, which an OS upgrade normally requires.

A Hybrid System profile without a saved `metricsIntervalSeconds` now uses D112's 60-second default when loaded. Previously, only newly converted profiles got 60 seconds, so earlier companions kept sampling every 15 seconds. An explicit value, including 15, is preserved. `nvidia-smi` remains a bounded per-sample command where installed.

WebRTC peers disable pion's multicast DNS. Its default query-only mode bound UDP 5353 on all IPv4 and IPv6 interfaces and joined the mDNS group for every ICE agent, only to resolve `.local` host candidates. Control peers never gather those, and no browser peer connects to a node. Disabling it removes one socket pair and LAN multicast processing per connection. A node that receives a `.local` candidate from an unexpected peer ignores it and uses its other candidates or the relay. On Windows, those wildcard sockets also raised firewall prompts for executables without Control's rules. Unanswered prompts left "Query User" Block rules, which a real test run on a worker reproduced; after this change it bound only loopback and created no rules.

## D155: Read terminal payloads on demand and append cleanup outcomes

Use the existing durable task file for full terminal reads. Keep active work, failed-persistence diagnostics, ownership, artifact references and duplicate detection in memory. This trades a local file read on completed-task inspection for memory proportional to history metadata rather than result bytes. A reader admitted before pruning may finish; later readers see the tombstone.

Write cleanup completion using the existing checksummed tombstone format instead of replacing the journal on every batch. Bound superseded entries with periodic compaction and retain the dirty-journal recovery barrier. This refines D058 and D075; see [limits and recovery](protocol.md#bounded-task-results-and-recovery).

## D156: Narrow workflow traffic without changing authority

Forward grant instructions to the coordinator named as their subject. A leaf RPC needs the selected ID because the destination already holds and checks the exact instruction. Prepare grants concurrently only after graph validation; keep plan order and collect acknowledged grants for cleanup on failure. Capacity and artifact metadata queries use optional filters on existing methods so access rules and older peers remain compatible. See [workflow ordering](protocol.md#workflow-ready-ordering).

## D157: Check proxy workload before reserving children

[D149](#d149-coordinated-proxy-first-managed-rollouts) allows parking idle children to drain their gateway CONNECT sockets. Parking cannot drain an application tunnel on the proxy. Start a group only when the proxy's current connection reports fresh aggregate health with no active operations or lease. Release pre-commit holds if this changes. This reuses health from existing proxies without collecting operation details or introducing a version-specific command. Health only controls when to try the drain; the proxy's own socket fence and durable commit still govern restart. See [proxy updates](gateway-proxy.md#updates).

## D158: Short-lived client setup and lossy-path throughput

Each CLI invocation is a new account-client process. Measured against Windows, Linux and VPS workers, one `control exec` spent 150–250 ms downloading the full directory (about 270 KB for 32 machines) only to resolve a name, then established the interactive lane for its blocking wait after submission returned. Clients now persist name-to-identity hints and read one scoped `/v1/peers/{id}` record, accepting it only when the gateway's current record carries the requested name. Rename and retirement fall back to the directory. A waiting submission prepares its interactive lane concurrently with `tasks.start`; preparation sends no application bytes, so cancellation cannot affect acceptance or replay work. A persistent MCP session remains the faster path for repeated calls because it reuses established carriers.

The gateway gzip-encodes directory and status responses when the caller accepts gzip. These responses hold fleet metadata rather than credentials and are the largest periodic reads: console tabs poll status every five seconds and name lookups read the directory. BestSpeed reduced the measured directory from 274 KB to 33 KB for about 0.2 ms of encoding time on an M4 Pro. Compressed directories use a weak validator.

Senders set a 64 KiB SCTP minimum congestion window, a 64 KiB fast-retransmit window and four-MTU congestion-avoidance steps. Pion's default collapses the window to one MTU after a retransmission timeout. On a lossy 170 ms direct path this held uploads near 1 Mbps and made several transfers fail with multiplexer write timeouts. With the floor, the same path sustained 3–3.7 Mbps without failures. Clean LAN and 140 ms VPS paths did not regress. The floor bounds recovery time; loss still reduces the window and retransmission backoff remains. ICE route selection still considers reachability rather than measured quality, so a lossy direct path can remain slower than the relay.

## D159: Data-channel-only WebRTC, prompt stale-registration recovery, and text results

Control's WebRTC connections carry only data channels, but `webrtc.NewAPI` registers default media codecs and RTP/RTCP interceptors whenever no registry is supplied. Their NACK generator ticks every 100 ms and the sender/receiver report loops every second on every peer connection, which dominated idle wakeups on connected workers. A live Linux worker with two connected clients averaged about 147 voluntary context switches per second and 1% of a core, against 17 per second for its companion without carriers. Peers now build the API with an empty media engine and interceptor registry. In a loopback carrier with three lanes, measured on the target machines, the idle cost fell from 89 to 9 extra context switches per second and from 0.64% to 0.09% of a core on Linux, from 82 to 10 and 0.25% to 0.05% on an arm64 macOS VM, and from 0.34% to 0.05% CPU on Windows. Data-channel SDP and wire behavior are unchanged, so mixed versions interoperate.

After a network change, the first gateway authentication request could reuse a pooled HTTP connection bound to the abandoned path and wait for its full 15-second timeout. The gateway, which never received the old socket's close, then also refused the reconnect with `identity already connected` until its heartbeat expired, up to another 15 seconds. Worker logs showed both. Endpoints now drop idle pooled gateway connections when the gateway socket is lost or the network changes. A proof-verified duplicate registration is still refused, preserving [D067](#d067-network-switch-recovery-and-end-to-end-gateway-liveness)'s guard against replacing a live registration, but the gateway challenges the existing connection immediately with a three-second deadline. A live duplicate answers and stays registered. Reconnecting through an abandoned path in a loopback test took 15.6–16.5 seconds before and 2.0–3.6 seconds after on Windows, Linux and macOS workers. Probes are triggered only by authenticated proofs for the same identity, and an outstanding challenge is only shortened, never duplicated.

`http.request` and `files.read` returned base64 even for text, which agents cannot read without decoding and which is a third larger. Both accept an optional `encoding: "text"` that returns valid UTF-8 directly, falling back to base64 otherwise. Text file chunks never split a character. Base64 remains the default, so existing callers and older workers, which ignore the field and report `encoding: "base64"`, keep working. MCP text results also restore the `<`, `>` and `&` characters that Go's JSON encoder writes as six-character escapes; commands and markup use them often, and a typical shell task record shrank by 17%. The decoded JSON value is unchanged.

## D160: Compact agent-facing discovery, task and artifact results

Measured on the live fleet, one `control_describe` was about 11.8 KB, of which about 2 KB was per-connection session and transport detail that grows with every connected client and is also in `diagnostics.get`, and 1.2 KB was runbook interpretation guidance repeated with an empty catalog. Client descriptions now omit `connections`, `sessions` and `transport` unless `view: "full"` is requested, and an empty catalog carries a one-sentence notice. A worker's `name` previously came from its installation profile, so a renamed machine described itself as `ubuntu-msa2` while the fleet routed it as `ubuntu`. Descriptions now report the gateway-owned name from the transport's cached identity lookup and add `configuredName` when it differs. Local readiness checks compare the configured name.

A User context cannot inspect a common runbook store inside a root-only System state directory, so every description reported a permission error. Such a store is now `restricted` rather than an error: its contents are unknown, an exact read still fails, and no permissions change.

`artifacts.list` returned every full reference; one Windows worker produced 88 KB for 288 artifacts, more than an MCP result allows, with each entry repeating the node ID and an ID equal to its SHA-256. A `limit` or `cursor` now selects a newest-first page of summaries with total count and bytes. Without them the legacy array is unchanged, and clients page older workers' arrays locally with the same cursor rules. Artifacts never expired and that worker held 1.26 GB since early October. Opt-in `artifactRetentionHours` lets the existing retention pass delete old artifacts that no retained task, reader or grant uses. The default still keeps artifacts until removal, so upgrades delete nothing.

Task acceptance, waits and cancellation repeated the caller's specification, including inline scripts, on every response. CLI and MCP results now omit `spec` unless `--spec` or `includeSpec` asks for it; the worker protocol is unchanged. Compact machine listings omit `lastSeen` for online machines, where the gateway value is the connection time and suggested inactivity.

## D161: WebRTC socket buffers and data-channel send window

Between two one-vCPU Linux servers 141 ms apart, Control's connection test reached 12–17 Mbps while plain TCP on the same path reached 112–115 Mbps. The CPU was not the limit: each receiving host dropped 29–234 datagrams per transfer in its 208 KiB default UDP receive buffer, and every drop collapsed the SCTP congestion window, which recovers slowly at that RTT. A standalone Pion data channel with Control's SCTP settings reproduced the baseline. Larger socket buffers alone removed the drops and reached 31–42 Mbps; the remaining limit was the 1 MiB data-channel send cap, about one cap per round trip. Buffers of 2–8 MiB with a 4 MiB cap reached 54–83 Mbps without drops. A 4 MiB cap with default buffers was slower, because it lost more packets.

Pion's standard network is wrapped through `SettingEngine.SetNet` so every host, server-reflexive and relay UDP socket requests 4 MiB send and receive buffers. Linux tries `SO_RCVBUFFORCE`/`SO_SNDBUFFORCE` first, so root System nodes are not capped by `net.core.rmem_max`; unprivileged requests are clamped silently, and macOS halves rejected requests. The wrapper keeps Pion's per-agent interface freshness with a one-second shared snapshot, so later connections still see network changes. The smallest granted receive size appears in transport diagnostics, and a process logs once when the OS grants less than half the request. Kernels allocate only queued datagrams, so idle sockets do not use the larger limit. The data-channel send cap rises to the existing 4 MiB SCTP receive window, refilling at 1 MiB. This raises worst-case sender queueing per busy channel from 1 MiB to 4 MiB; yamux windows and SCTP receive bounds are unchanged. Throughput past about 80 Mbps on one vCPU becomes CPU-bound at the measured 42–72 ms of CPU per MiB, so further gains need profiling rather than larger windows.

## D162: Route-driven network interface sampling

The gateway egress monitor enumerated every interface and its addresses every two seconds. On a Windows worker one sample cost 2.2 ms of process CPU and 6.5 ms of wall time, about 0.11% of a core per node process and twice that on Hybrid machines, a large share of their idle CPU. Route resolution is cheap; the inventory only maps a route's source address to its interface identity. The sampler now resolves routes on every poll, but reuses the interface inventory while every route keeps the same source. It re-reads interfaces at once when a source changes, retries with a fresh inventory when the cached one cannot place a source, and refreshes after 30 seconds. A changed source still alters the fingerprint immediately. Changes that keep the same source address, such as a different subnet on that address, are noticed within 30 seconds instead of two. Failed enumerations remain failed samples, not network switches, and fingerprint semantics from [D100](#d100-gateway-egress-fingerprints-ignore-ipv6-privacy-rotation) are unchanged.

## D163: Heartbeat-confirmed node telemetry

Each idle node sent three five-second gateway streams: health, unchanged update status, and the heartbeat echo. On a 19-connection fleet this was about 300 bytes per second of TLS per node through expose, with the gateway and expose using 0.92% and 0.68% of one vCPU. Health repeats its resource sample until the next 15-second collection, and unchanged update status existed only to stay within the gateway's eight-second freshness window.

Gateways now advertise `telemetryLiveness`. When a node echoes the outstanding application challenge, the gateway refreshes the receipt time of health and update status received on that same connection within 45 seconds. The echo proves the connection is alive, and nodes send changes in order on that socket as soon as they observe them, so those reports remain current. Wrong nonces, superseded connections and older reports are not confirmed, so a stalled updater still goes stale within 45 seconds rather than eight. Nodes negotiating the feature with `gatewayHeartbeat` send health only when it changes, on each new gateway connection, or every 30 seconds, and send unchanged update status every 30 seconds. Changes keep their existing detection cadence: five seconds for health and two for update status. Older gateways and nodes keep five-second reports, preserving nodes-first managed rollouts. Rollout freshness checks, health expiry and status windows are unchanged; only what refreshes them changed. This refines [D022](#d022-owner-visible-machine-health-without-a-local-node) and [D067](#d067-network-switch-recovery-and-end-to-end-gateway-liveness)'s application heartbeat. Expected idle traffic falls by roughly 60% of bytes and two of three messages per node; the resulting gateway and proxy CPU reduction was not measured.

## D164: Receiver-scoped LAN discovery view

Re-verifying one LAN pair used to invalidate all workers' conditional `/v1/nodes` polls because the full directory includes every proof timestamp. Give discovery its own compact representation with online same-fleet discovery nodes and only the receiver's proof list. Build its validator from a shared topology revision and the receiver's proof revision, both maintained by committed observation publication. Return 304 without assembling a response when neither changes. A refresh for another receiver does not invalidate the view, but still invalidates the full directory.

Bind each read to the worker's live signed registration as well as its bearer key, since workers may share that key. Use the existing registration signature to derive a session credential without adding a handshake round trip, persisted state, or execution authority. Negotiate through `/v1/auth`. Keep the full-directory fallback for older gateways and leave ten-second polling and proof freshness checks intact, including after 304.

## D165: Persisted release version floor

GitHub publication dates are unsigned. Someone who can edit releases but does not hold a signing key could republish an older signed release and the gateway would accept it as newer. Persist the newest semantic version ever selected from GitHub in `deployment.json`, carry it across development pushes, and select a `vMAJOR.MINOR.PATCH` release only when it is above that floor. Development pushes remain the explicit superuser path to an older bundle. Non-semantic tags cannot be ordered and keep the earlier publication-date check. This blocks rollback, not withholding of newer releases.

## D166: Batched disconnect last-seen persistence

Every disconnect used to commit the node's offline state and last-seen time with a full fsync while holding the gateway mutation lock. A network blip that dropped many nodes serialized one commit per node, delaying their reconnects. Startup already resets `online`, so only last-seen history needs durability. Mark disconnected nodes dirty and persist them in one transaction every 30 seconds and during graceful close, after connection handlers finish. A disconnect that starts an update restart grace still commits immediately. A crash can lose up to 30 seconds of last-seen history; presence published to readers is unchanged.

## D167: On-demand MCP desktop guidance

Every MCP session paid about 7.6 KB of desktop-only instructions, roughly a third of the server instructions, even for command or file work. Follow the browser guidance pattern: keep a short unconditional summary with routing and credential safeguards, and return the full workflow guidance through a local `control_guide` tool with `topic: "desktop"`. The skill bundle already loads GUI details on demand. Tests check the same safeguards across the summary and the guide, cap the summary at 130 words, and bound total instruction and tool-definition bytes so new guidance moves behind on-demand topics instead of growing every session.

## D168: Hold the gateway restart for node downloads

The 15-second acknowledgement settle is superseded by [D169](#d169-gateway-first-rollout-order); the hold while nodes report `downloading` and the node download retry remain.

The gateway restarted as soon as no executable download was in flight, which is usually within a second of publication, before nodes had started downloading. Downloads that began during the restart got 502 from the gateway's reverse proxy, entered a 30-second error backoff, and released the machine cache lock, so a Hybrid companion often downloaded separately. Machines also appeared offline during the restart while their update had not visibly started. The gateway now waits up to 15 seconds for online nodes that need the selected software to acknowledge it and while they report `downloading`, bounded at five minutes, before draining for its restart. Node gateway downloads retry transient proxy and connection failures for up to 30 seconds within one attempt. Restart order for node commits and gateway epochs is unchanged.

## D169: Gateway-first rollout order

D168 kept the gateway up until nodes had downloaded, but by then nodes held commit slots one after another, and the gateway restarts only with no node commit in flight. It therefore switched last, after every node, the opposite of what operators expect. A gateway restart during node updates also interrupts them. Node offers now wait until the gateway has staged and switched to the selected software, so nodes download from and commit under the new gateway and its restart never overlaps their downloads or commits. Two cases keep nodes first: online nodes that cannot connect to the staged gateway epoch, and a gateway that cannot replace itself. Relayed traffic that outlasts a 30-second gateway drain attempt releases nodes at once, preserving the rule that a persistent relayed tunnel postpones only the gateway. The hold is otherwise bounded at two minutes after the coordinator selects the deployment, so a failing gateway stage cannot block node updates indefinitely. Released nodes update independently as before. Proxy children still update after their proxy ([proxy rollout coordination](gateway-proxy.md#updates)). A child parked for its proxy's restart now shows `update` with `waiting-proxy`, with the same one-minute display grace as a restarting node, instead of `offline`.

## D170: Batched LAN proof persistence

Each re-verified LAN pair committed its receiver's directory record with a full fsync under the gateway mutation lock, about k(k-1) commits every ten minutes for k LAN peers. Proofs are refreshed observations, not acknowledged changes: nothing waits on their durability, and later discovery cycles report them again. Publish an accepted proof to the directory and LAN view at once, and persist it with the batched last-seen flush of [D166](#d166-batched-disconnect-last-seen-persistence), within 30 seconds and during graceful close. A crash can lose at most that interval of evidence. The full directory still includes proof timestamps, so its validator still changes on re-verification; the receiver-scoped view of [D164](#d164-receiver-scoped-lan-discovery-view) is unchanged.

## D171: In-memory queued-to-running transition

Starting a provider durably rewrote the task record to record `running`. Acceptance is already durable when the queued record is written, and restart recovery marks every non-terminal record `interrupted` whether it was queued or running, so that fsync bought nothing. The transition is now published in memory only, and the next durable update, normally the terminal record, writes it. Acceptance durability, idempotent submission, and the rule that recovery never re-runs an uncertain task are unchanged.

## D172: Snapshot reads for peer lookup and credential usernames

Peer authentication during session setup and `GET /v1/auth` waited behind fsync commits on the gateway mutation lock, although both only read state that is already published. `GET /v1/peers/{id}` now reads machines from the published directory observation, falling back to the lock only for connected client identities, and the credential index published after each committed credential change carries the account username and installation record key. This extends the published-snapshot approach of [D070](#d070-bounded-recent-activity-and-pinned-binary-verification-outside-shared-locks) to those two read paths; console sessions and invitation listing still read under the lock.

## D173: Stall-bounded, resumable binary downloads

A two-minute whole-request client timeout meant a roughly 27 MB executable could never stage below about 1.8 Mbit/s, and each retry restarted from byte zero. Update and installer downloads now end only on stalls: no response headers within 30 seconds or no received bytes for 30 seconds. Gateway downloads keep received bytes across their bounded retries and resume with `Range`; progress renews the retry window. Verification is unchanged: the complete file must match the selected size and SHA-256 before publication, and a mismatch after a resume refetches once from offset zero rather than publishing or retrying indefinitely. Partial bytes are private temporaries beside the destination and never survive the attempt that wrote them.

## D174: Rooted file mutation primitives

The filesystem API offered only `files.list`, `files.read` and `files.write`, so agents created, removed, renamed and copied workspace files with `mkdir`, `rm`, `mv` or `cp` through `exec.run`. That is unconfined, specific to one OS shell, and uses execution capacity. Add `files.stat`, `files.mkdir`, `files.remove`, `files.move` and `files.copy` under the same `workDir` `os.Root`, private upload paths, operation pool, authorization, lease, maintenance and delegation checks as the existing methods, and let `files.write` take `encoding: "text"`.

Each mutation acts once and never retries. Nothing replaces an existing entry unless `overwrite` is set, and then only a regular file replaces a regular file. Directories, links and special files are never replaced, which matches `os.Root.Rename`. Removal and moves act on the named entry, refusing a link or special file there, so neither can reach outside the root. No-overwrite file moves and copies publish by hard link, as uploads do, so a concurrently created destination is never replaced. Copies are synced to a private temporary file and published only when complete. File systems without hard links therefore need `overwrite`. Errors distinguish `destination_exists`, where nothing changed, from `partial_effect`, where a step succeeded before a later one failed. `files.copy` and `files.stat` hashing are limited to 1 GiB and check cancellation between 1 MiB chunks. Larger files belong in tasks or artifacts. A directory move refuses trees that contain upload staging or metadata, the task workspace root, or more than 10,000 entries.

Recursive removal is not offered. It cannot be atomic, a failure leaves an unknown subset deleted, and a tree can hold running task workspaces or upload staging. Callers remove entries individually, or accept that risk explicitly through `exec.run`.

## D175: Bounded head and tail output retention

Command results kept only the first 2 MiB of each stream, and task logs kept only the first 10 MiB, so a long job that failed at the end lost the diagnostics that mattered. `exec.run` and `agent.run` captures now keep complete output up to 2 MiB. Longer output keeps the first 1 MiB and the last 1 MiB minus 64 bytes, joined by a `\n... [control: N bytes omitted] ...\n` marker, within the same 2 MiB per stream. `truncated` keeps its meaning. New `stdoutOmitted` and `stderrOmitted` fields give exact counts, so callers need not parse the marker. Neither cut splits a UTF-8 sequence; partial sequences count as omitted.

The task log file and its byte-offset, polling and `follow` semantics are unchanged. Output beyond the 10 MiB head goes to a 1 MiB ring, allocated only after the head fills. While the task runs, `tasks.logs` snapshots read the ring. Before execution ends and the task can become terminal, the ring is sealed and atomically written to `dataDir/tasks/ID.tail` with its head size and absolute start offset. `tasks.logs` with `tail: true` reads it by absolute output offset, starting at the tail when the requested offset is earlier. Head reads that reach the end of the head report `omitted`, `tailOffset` and `size`. Mid-log head reads and streams do no extra work. A tail read without a tail is an ordinary head read, the same response an older worker gives for the ignored field, so new clients degrade without error. Followers never include the tail, because splicing a gap into an offset stream would break older readers. The CLI exposes `--tail`; MCP `control_task_logs` accepts `tail`.

The tail never weakens [D145](#d145-worker-local-command-secret-injection). Secret-bearing commands still suppress truncated output entirely, zero the omitted counts and write nothing to logs until complete masked output exists. That output is at most 4 MiB, so it cannot reach a log tail. Desktop, browser and configured provider adapters keep head-only capture and still fail on overflow. The log tail only holds bytes a provider already wrote to its task log.

Retention removes the tail with the record and log, under the same pending-cleanup intent of [D075](#d075-bounded-task-data-and-durable-cleanup). A crash before execution ends leaves no tail. The interrupted record then reports no omission, never a partial or stale tail. A tail that fails validation fails explicit tail reads but never head reads. Memory is bounded at 1 MiB per running task whose output exceeded its head. The bounds are constants, not configuration.

## D176: Advisory GPU selection constraints

D092 and D093 sampled GPU/NPU inventory but kept it out of worker selection, so accelerator work could only be placed through hand-maintained labels. Extend D078's `nodes.select` constraints with `gpuKind`, case-insensitive `gpuVendor`, `minGpuMemoryBytes` for dedicated capacity, and `minGpuFreeMemoryBytes` for dedicated capacity minus use in the same sample. Offer both memory forms because capacity is stable inventory while free memory is a live reading, meaningful only inside the existing 20-second health and resource freshness window that every constraint already requires. Validate kinds against the reported set and reject negative or fractional sizes by field name.

One reported device must satisfy every GPU constraint. Do not sum dedicated memory across devices, since one workload cannot generally treat several devices' memory as a single pool. Unclassified kinds, unreported vendors, missing capacity and missing use are unknown and never satisfy a constraint. Older workers or gateways without GPU reports therefore yield no GPU match rather than an assumed one. Without GPU constraints GPUs do not affect preference, so ordinary work is not steered toward accelerator machines. With them, the least-loaded matching device's compute utilization and dedicated-memory use join the existing score, and unknown readings count as fully loaded so they never outrank a known reading.

Every route reaches the same transport selector and the gateway's owner-visible health, which already carries `system.gpus`. Credentials without summary access receive an explicit freshness error, as for other resource constraints. GPU-constrained results add `gpuConstraints: true`; account clients reject answers without it, because a local node API or explicitly targeted selector running older software ignores unknown fields. Selection remains advisory: no device reservation, lease, or admission change is introduced, and destination-owned GPU reservations remain future work. See [`nodes.select`](protocol.md#node-operations).

## D177: Pre-planned workflow cleanup steps

After any workflow step failed, no further step started, so a plan had no way to delete a temporary directory, release a licence or stop a helper service once something went wrong. A step may now carry `"when": "always"`; the default `"when": "success"` keeps the existing rule. Validation rejects any other value, and dependency cycles through cleanup steps are rejected like any other cycle.

A cleanup step is part of the validated plan, not a replay or a retry. Its target, task ID, specification and grant are fixed before the workflow starts, and the client prepares its grant up front with every other step. The coordinator submits it at most once, and nothing is resubmitted after an uncertain outcome.

A cleanup step becomes ready once every step in its `needs` and artifact `inputs` has settled: it succeeded, failed, was definitely never submitted, or will never start because normal scheduling stopped. A submission whose request was never sent counts as never submitted. Any other submission error, or an unrecoverable status-read error, leaves that child unresolved. Cleanup steps that depend on an unresolved child, directly or through steps that never started, do not start and are reported as not started. Starting cleanup beside work that may still be running could destroy the state that work is using. An artifact input whose upstream step did not succeed cannot be staged, so that cleanup step is not submitted and is reported as not started with that reason. Plan validation cannot know which steps will succeed, so this is checked at run time. A not-started cleanup step counts as settled and can release later cleanup steps.

Cancellation and timeout of the workflow task withdraw its authority to run. No further step starts, including ready cleanup steps, and pending cleanup steps are reported as not started. A workflow cannot itself run under a delegated grant, because `access.grant` rejects `workflow.run`. Revocation therefore acts on individual children at their destinations. A revoked child is cancelled, and its status read may fail, which leaves it unresolved. A cleanup step whose own grant was revoked is refused by its destination and never runs. A child that ends `cancelled`, including after its own task timeout, is a settled outcome, because the coordinator cannot tell why a child was cancelled and child timeouts are the most common case that needs cleanup. Cancel the workflow task, or also revoke the cleanup steps' grants, when no cleanup should run.

A success step that needs a cleanup step requires it to succeed, and never starts after any failure, including a cleanup failure. Cleanup steps count against `maxParallel`, use the same capacity-aware ordering, lookahead and probe bounds, and respect the 100-step limit. In-flight children still finish normally and are never cancelled to make room. The result lists every started child, including cleanup steps. A failed step fails the workflow, including a cleanup step that fails after every other step succeeded. The error reports the first failure of a success step, or the workflow's own cancellation or timeout, before cleanup failures and not-started cleanup steps, so cleanup never masks the original failure. Results keep their existing shape, and no task state is added.

Reconciliation repeats each step's `when`. It marks a step `notStarted` only when the terminal parent's retained result has no child for that step and the destination replies that no such task exists. Uncertain submissions are also absent from the parent result, so either piece of evidence alone leaves an ordinary query error.

Older coordinators ignore the unknown field and would run a cleanup step as an ordinary step. Coordinators now advertise `when` with an `always` value in the `workflow.run` step schema. The client refuses a plan containing cleanup steps before preparing any grant or creating any task when the coordinator's directory record lacks that schema. Through a local API, the client reads the coordinator's `capabilities.list` first. Plans without cleanup steps are unchanged. This refines the failure rule of [D060](#d060-bounded-parallel-workflows-and-load-preferring-selection) and [D125](#d125-bounded-capacity-aware-workflow-ready-ordering), and the unresolved-observation rule of [D141](#d141-recover-observation-and-publication-without-replaying-effects), without changing authority, grant preparation or ready ordering. See [cleanup steps](protocol.md#cleanup-steps).

## D178: Streamed HTTP downloads into artifacts

`http.request` returns the body inline, capped at 2 MiB, under a fixed five-minute client timeout. Fetching an installer, model or dataset onto a worker therefore needed `exec.run` with `curl` or PowerShell, which differs by OS, has no size, budget or checksum contract and requires general execution permission. Add a built-in `http.download` capability that streams one `GET` response body into an immutable content-addressed artifact held by the invocation's task owner and returns `status`, selected masked headers and the artifact reference, never the body.

Artifacts are the only destination. They are already the documented mechanism for large data: SHA-256 identity, owner holds, budget and free-space accounting, retention of abandoned staging, and direct delivery to other workers. The body streams through the existing artifact import staging file (`.import-*`), so the same `maxArtifactBytes` budget, `artifactReserveBytes` check, crash cleanup at startup and age-based reclamation apply, and any failure removes the staging file before returning. A workspace `path` destination was left out: placing a file in a workspace is already a task input, and a second write path would need its own no-overwrite and confinement rules. Only `GET` is accepted; other methods remain `http.request` work, so a download never repeats a non-idempotent request.

Bounds are explicit. `maxBytes` defaults to 1 GiB and may be raised to 1 TiB. A `Content-Length` above `maxBytes`, above the remaining artifact budget or within the free-space reserve is refused before the body is read; streamed bytes are checked again, so a missing or false length is still bounded. A supplied `sha256` is verified at end of stream, and a mismatch fails the import before publication. Only `200 OK` is published. Response compression is neither requested nor decoded, so the stored bytes are the entity as sent and match a publisher's digest.

The invocation or task deadline bounds total duration; there is no fixed whole-request timeout. As in [D173](#d173-stall-bounded-resumable-binary-downloads), stalls end a download: no response headers, or no body bytes, for `stallSeconds` (default 30, 1..3600). The timer stops at end of stream so slow publication is not mistaken for a stalled server. There is no automatic retry and no `Range` resume in this version; a failed download is simply repeated by the caller, which never silently replays it. Resume would need validators such as `ETag` with hash verification and is left for later.

Secret headers reuse `http.request` semantics: at most 32 `headerSecrets`, no literal/secret overlap, worker-local resolution, masked errors and returned headers, and no redirects when references are present. Without references at most 10 redirects are followed, each limited to http/https. The downloaded bytes are content, not text output, and are not masked.

`http.download` uses the `maxOperations` pool, like `http.request`, for both synchronous calls and tracked tasks, so routing stays identical across both paths and the stall bound limits how long a hung server can hold a slot. The artifact is written while bytes arrive, inside the provider invocation, so it does not use task output-publication admission. Authorization, delegation, leases, maintenance admission and activity tracking need no new rules: the capability is an ordinary built-in method. A long download through a synchronous call depends on that connection staying up, so tracked tasks with an explicit ID and sufficient `timeoutSeconds` are recommended. See [`http.download`](protocol.md#node-operations).

## D179: Process-owned SOCKS5 forwards

The 128-socket limit and immediate overflow close below are superseded by [D189](#d189-bounded-socks5-capacity-on-shared-peer-sessions). The forwarding and authorization rules still apply.

A forward binds one local listener to one fixed `HOST:PORT`, so reaching several hosts on a worker's internal network needed one tunnel per destination. `control tunnel NODE --socks [--listen ADDRESS]` and MCP `control_forward_start` with `socks: true` add a SOCKS5 mode to the existing process-owned forward. Each accepted socket negotiates RFC 1928 "no authentication" and one `CONNECT` with an IPv4, IPv6 or domain-name address, then opens an ordinary `tcp.open` stream to that address. The worker therefore authorizes, admits, dials and records each socket exactly as for a fixed forward, with its real destination in activity. Domain names are passed unresolved so the worker's DNS and network apply. `BIND` and `UDP ASSOCIATE` are refused with reply `0x07`; dial failures map to SOCKS reply codes and `lastError`, and a failed stream is never reopened. Username/password authentication is not offered: the listener is local and credentials would add secret handling without isolating anything a loopback listener does not already isolate.

The handshake parses untrusted local bytes with fixed-size reads, at most 255 bytes per field, and one 10-second deadline. A socket occupies one of the forward's 128 slots from accept, including the handshake, and forwards still count against the 32-listener limit. Stop and shutdown cancel pending dials, close handshaking and connected sockets, and join their goroutines like other forwards. Fixed forwards may bind any explicitly chosen listener, but a non-loopback SOCKS listener is an unauthenticated open proxy to everything the worker can reach. It therefore requires an explicit `allowNonLoopback` (CLI `--allow-non-loopback`) opt-in, and the bound address is checked again after listening. Reverse SOCKS is refused.

Instruction-only destinations, such as Hybrid System companions ([D087](#d087-hybrid-installation-with-isolated-execution-contexts)), are refused before binding and again for every socket. Their grants bind the exact `tcp.open` address. The shared client could prepare one exact grant per socket, but the address would then come from any local process that reaches the listener rather than from an instruction the user named, which would turn an explicit System operation into an ambient one. SOCKS sockets use a peer opener that never prepares grants, and an exact-grant requirement is unchanged. Use a fixed forward to `NAME-system` for one explicit address.

Retained `control tunnel start` definitions ([D051](#d051-host-owned-persistent-tunnel-definitions)) do not support SOCKS mode. Their state file requires a destination `HOST:PORT`, and an older service that loaded a definition without one would reject its entire retained state rather than skip one entry; the service also refuses SOCKS definitions explicitly. This extends D030/D042's process-owned forwarding without changing `tcp.open`, duplex negotiation, gateway routing or worker authorization. See [SOCKS proxies](tunnels.md#socks-proxies) and [protocol](protocol.md#orchestrator-sessions-and-forwards).

## D180: Plan-time workflow target selection and fan-out

A workflow step named one fixed `target`. Running a step on every machine with a label, or on any machine that fits, meant writing the plan by hand after `nodes.select` or `nodes.list`. A step `target` may now be `{"select": SELECTOR}` or `{"each": SELECTOR}`, where `SELECTOR` holds the `nodes.select` fields, including GPU constraints. `select` resolves to one machine, chosen as `nodes.select` would choose it. `each` expands the step into one step per eligible machine.

Resolution belongs to the shared client and happens once, before validation, the cleanup capability check and grant preparation. The CLI, MCP, account routing and the local API therefore behave identically. Coordinators and destinations only ever receive concrete targets, so instruction-bound grants ([D045](#d045-account-client-authority-and-instruction-bound-worker-delegation)), parallel scheduling ([D060](#d060-bounded-parallel-workflows-and-load-preferring-selection)), capacity-aware ordering ([D125](#d125-bounded-capacity-aware-workflow-ready-ordering)) and workflow traffic ([D156](#d156-narrow-workflow-traffic-without-changing-authority)) are unchanged; they simply see a larger plan. A coordinator resolving selectors itself would move placement authority to a worker identity, so plan validation rejects a step that still carries a selector and no step starts.

The selector is encoded in place of the string `target` rather than as separate `select` and `each` fields. Older clients and coordinators decode `target` as a string. With separate fields they would ignore the selector and see an empty target, which a coordinator reached through its local API runs on itself. An object where a string is expected fails decoding instead, so every older peer fails the plan closed. The cost is a custom JSON encoding of workflow steps in the model.

Fan-out reuses selection rather than duplicating it. `nodes.select` gains `all: true`, which returns every eligible machine as an array under exactly the filters of single selection: online, enabled, policy-acknowledged, not instruction-only, no known lease or update reservation, and fresh resources when constrained. Instruction-only Hybrid System companions ([D087](#d087-hybrid-installation-with-isolated-execution-contexts)) are never selected; a plan names them explicitly. An older selector ignores `all` and answers one object, which the client cannot decode as an array, so a fan-out never silently shrinks to one machine. GPU-constrained matches must each confirm the constraints, as in [D176](#d176-advisory-gpu-selection-constraints). Selection remains advisory, as in [D078](#d078-resource-placement-explicit-reconciliation-and-release-signatures): nothing is reserved and destination admission still decides. Workflow selectors reject unknown fields, because a misspelled constraint would otherwise widen a fan-out to every machine; plain `nodes.select` keeps its tolerant decoding.

Expansion is deterministic from the plan, the parent task ID and the matched machines. Copies are named `STEP@MACHINE` in name-then-ID order, falling back to the machine ID for missing or duplicate names. A copy of a step with an explicit task ID gets a hash of that ID and the machine ID. Every other step without an ID gets the hash of the parent ID and its step name, the derivation grant preparation already used, so the printed and submitted plans agree. A `needs` entry naming a fanned-out step expands to all copies, including for cleanup steps ([D177](#d177-pre-planned-workflow-cleanup-steps)); a fanned-out cleanup step waits for every upstream copy rather than only its own machine's, which is conservative and needs no pairing rule. Artifact inputs from a fanned-out step are rejected because no single source is obviously right, and pairing copies by machine would silently depend on two match sets being equal. The 100-step limit applies after expansion.

A retry must not pick different machines. For a task submission the client first reads the coordinator's `tasks.get` for the task ID. If the task exists, its retained `workflow.run` arguments are the resolved plan and are resubmitted unchanged, which the coordinator acknowledges as `existing` ([D140](#d140-lifetime-aware-grant-cleanup-and-submission-receipts)). Only the coordinator's own not-found reply permits fresh resolution; any other read failure stops submission, and a pruned ID must be replaced. If an earlier uncertain attempt is accepted after the retry checked, a differently resolved retry is refused as a different specification and its fresh grants are released. `control task start` prints every resolved step with its machine and child ID before sending, as it already prints the task ID, and MCP `control_task_start` returns the resolved specification instead of omitting it. Reconciliation reads the retained resolved plan unchanged.

Resolution only reads: the plan is checked first, then the existing task and the directory. Any failure, including an empty match set or an oversized expansion, is reported with outcome `not_sent` before any grant or task exists. Direct synchronous `workflow.run` calls are resolved too, with fresh child IDs, but are not reconcilable; fan-out should be submitted as a task with an explicit ID. This extends the small workflow executor of [D010](#d010-destination-owned-leases-and-a-small-workflow-executor) without adding MCP tools, parameters or instructions. See [target selection and fan-out](protocol.md#target-selection-and-fan-out).

## D181: Advisory task submission checks

Agents often submit a task that is rejected, or accepted and then fails at once, for a reason the node could have reported first: an unknown capability, arguments outside the advertised schema, an ID reused with a different specification or differing only in case ([D139](#d139-portable-case-folded-task-id-exclusion)), a missing permission, a lease, or a disabled or updating machine. `tasks.check` takes the specification `tasks.start` would receive and reports each condition separately, without accepting it. CLI `control task start NODE --check` prints the report and exits nonzero on a failing check; MCP reaches it through `control_call`, so no tool or instruction text is added.

The check follows the order of `tasks.start` admission but only reads state. It reserves no task ID or acceptance slot, writes nothing, enters no maintenance or provider admission, records no activity, takes no lease, starts no provider or MCP server, creates no workspace, verifies no artifact grant and contacts no artifact source, workflow child or `peers.call` target. It is observational for maintenance, so it answers on disabled and updating machines rather than being refused by the condition it reports. A matching retained task or tombstone passes the `id` check and skips the rest, because `tasks.start` returns it before capability and admission checks. Arguments are validated with the JSON Schema library already used for desktop and browser contracts, without remote references; acceptance itself still does not validate arguments, so that check predicts a provider failure, not a rejection. Workflow checks validate the plan graph and explicit child IDs on the coordinator only: fanning out to children would need grants or would be an oracle at destinations the client has not yet authorized, and children can change before submission anyway. The specification normalization is repeated rather than extracted from `startTaskContext`, keeping that acceptance path untouched; a test compares their digests.

Every answer carries `advisory: true` and a note. State can change between check and submission, so a check is neither a reservation nor evidence about a later submission. In particular, a passing check never turns a lost or failed submission into `not_sent`; uncertain submissions are still reconciled by task ID ([D007](#d007-durable-acceptance-with-conservative-restart-recovery), [D140](#d140-lifetime-aware-grant-cleanup-and-submission-receipts)).

The check needs exactly the authority of the submission it describes. `allow` matching treats `tasks.check` as `tasks.start`, so a separate rule grants nothing and current nodes name `tasks.start` when refusing. The capability permission is the `authorization` check; when it fails, every check other than the discovery-visible capability name is skipped, so the answer is no oracle for callers who could not submit. Instruction-only destinations ([D087](#d087-hybrid-installation-with-isolated-execution-contexts)) were not refused: the shared client already obtains an exact single-use grant for any synchronous instruction to them, and a `tasks.check` grant binds the method and exact specification, is revoked when the reply returns and requires the capability permission at issuance, as a task grant does. It cannot authorize `tasks.start`, and a `tasks.start` grant cannot authorize a check, so a check never uses or renews the grant meant for the real submission. Coordination capabilities are refused at issuance there, as their submissions are.

Older nodes reject the unknown method, or deny it by name under restrictive `allow` rules. Current nodes produce neither message for this method, so the client reports both as `task_check_unsupported`, "preflight not supported by this node". No capability flag or negotiation is added; a failed check sends no work and needs no fallback. This extends [D045](#d045-account-client-authority-and-instruction-bound-worker-delegation) and [D057](#d057-blocking-task-waits-and-acceptance-outside-the-node-lock) without changing task acceptance, grants, leases or the peer envelope. See [task checks](protocol.md#task-checks).

## D182: Owner task tags and cancel-by-tag

A task specification had no grouping, so listing or cancelling a batch of 50 related tasks took 50 calls. `TaskSpec` gains optional `tags`: at most 8, each 1..64 ASCII letters, digits, `.`, `_` or `-` starting with a letter or digit, compared case-sensitively. Acceptance validates them, then removes duplicates and sorts them before the record and digest are written. Tags are metadata. They grant no authority, select nothing alone, and change no execution, lease or admission rule.

Tags are part of the specification digest of [D007](#d007-durable-acceptance-with-conservative-restart-recovery). Normalization makes the digest order-insensitive, while adding, removing or changing a tag is a conflicting specification and is rejected, so a duplicate submission never retags a task. An empty list normalizes to an omitted field, so an untagged specification encodes byte-for-byte as before. Retained records and [D058](#d058-terminal-task-retention-with-tombstones) tombstones written by older nodes keep matching, and no stored digest is recomputed or migrated. Tombstones keep only the digest; tags are removed with the record, so a pruned task is no longer listed by tag but still reconciles.

`tasks.page` and `tasks.list` accept a `tag` filter. It is applied during the existing scan of retained records, at most 8 comparisons per record, so listing stays O(retained × 8) with the same `limit + 1` working set and cursors. No tag index is kept. Its benefit would only matter at retention sizes where the scan already dominates, and it would be another structure to keep consistent through pruning, compaction and restart. Retained metadata keeps tags in memory, at most about 0.5 KiB per record. [D075](#d075-bounded-task-data-and-durable-cleanup) summaries, legacy listings, full records and [D160](#d160-compact-agent-facing-discovery-task-and-artifact-results) compact CLI/MCP task results, which keep a top-level `tags` copy when `spec` is omitted, all carry them.

`tasks.cancelTagged` with a `tag` signals cancellation of the caller's own unfinished tasks carrying it on that node, exactly as `tasks.cancel` does for each. Ownership is exact, including for the local API's node identity, whose broader inspection view does not extend to bulk cancellation. The node acts on the set it observes under its task lock. A task accepted concurrently, including one whose acceptance record is still being written, is left for a later call rather than chased. The result lists every matched unfinished task with outcome `cancelRequested` and the state observed then, or `failed` with an error, plus an `alreadyTerminal` count. Terminal tasks are counted rather than listed, because retention can hold 10,000 records while unfinished tasks are bounded by the node's 256-task queue. `cancelRequested` is named for what happened: cancellation is asynchronous, and a task already finishing may still end `succeeded` or `failed`. The call deletes and replays nothing and can be repeated. Like `tasks.cancel`, it bypasses maintenance admission and is tracked as an operation. Access rules must name it, since a `tasks.cancel` rule does not match it.

Task grants are bound to one explicit task ID ([D139](#d139-portable-case-folded-task-id-exclusion) keeps that ID unique). A tag-scoped grant would let a worker holding it cancel tasks the grant never named. `access.grant` therefore refuses `tasks.cancelTagged`, `authorize` rejects it under any grant, and the handler checks again. Only account clients calling directly and the local API can cancel by tag, and instruction-only destinations such as Hybrid System companions support cancellation by ID only. Workflow children do not inherit the workflow task's tags. Children live on other nodes and are cancelled per node, and inheritance would make a step's digest depend on its parent. A step may carry its own tags, validated with the plan. Cancelling the workflow task already cancels its in-flight children.

Older nodes ignore the unknown `tags` field and `tag` filter. They would accept a task untagged, which a later cancel-by-tag silently misses, or list every task as matching. Nodes advertise `taskTags` with protocol `task-tags-v1` and the bounds in `node.describe`. Before sending a tagged specification or a tag filter, the shared client reads `node.describe` from every destination involved, including `peers.call` targets and workflow step targets, and refuses with outcome `not_sent` before preparing grants or submitting anything. Requests without tags make no extra call. An older node rejects `tasks.cancelTagged` as an unknown method, which is explicit, so that call is not pre-checked. The CLI adds `control task list NODE --tag TAG` and `control task cancel NODE --tag TAG`. MCP reaches both methods through `control_call` without new tools. See [task tags](protocol.md#task-tags).

## D183: Per-invocation command resource accounting

[D013](#d013-sample-host-resources-separately-from-activity-polling) samples host resources but explicitly does not account for tasks, so nothing recorded what one command cost. Sizing repeated work, or choosing a machine for it, meant guessing or wrapping the program in `time`-style tooling that differs by OS. `exec.run`, `agent.run` and configured subprocess providers now measure the invocation-owned process tree of [D142](#d142-invocation-owned-one-shot-command-descendants) with the mechanisms that ownership already provides, without CGO.

Results carry a compact `usage` object: `wallMs`, `userCpuMs`, `systemCpuMs`, and one platform-named memory field. Every field is an optional pointer; an absent field is unknown and a present `0` is a measured value, as elsewhere in Control. A tracked task stores the same object as `usage` on its terminal record, because a configured provider's result is the provider's own JSON and cannot carry it. Task summaries, gateway health reports and the dashboard do not include it, so pages and directory traffic do not grow. The object is a few dozen bytes and is never mutated after publication.

Unix and Windows measure different things, and the field names say which. On Linux and macOS the ownership helper waits for the requested program and reports that program's `wait4` rusage over its private status channel. The parent's own wait of the helper is not used, because its `ru_maxrss` would be floored by the helper's resident set. The rusage covers the program and descendants reaped through an unbroken chain of waits. Orphans reparented elsewhere, and descendants still running when the program exits and then killed by group cleanup, are not counted; making the helper a subreaper would change process semantics for accounting's sake. `ru_maxrss` is converted from kilobytes on Linux and other Unix systems and used as bytes on macOS. It is the peak of the largest single process, not a sum over the tree, so it is published as `maxRssBytes` rather than as a tree peak. On Windows the parent queries the invocation's Job Object after cleanup and the helper's reaping, before closing the handle. `JobObjectBasicAccountingInformation` covers every process that ever ran in the job, orphans included; the helper's own process times are subtracted. `PeakJobMemoryUsed` is the peak committed memory of all job processes together and cannot separate the helper's few megabytes, so it is published as `peakJobMemoryBytes` and documented as including them. `wallMs` runs from opening the gate to receiving the exit report.

Measurement must never change execution. The helper's report rides on the existing status message, and the Windows queries are two non-blocking calls on a handle the parent already owns. Neither delays completion, keeps a process or handle alive, or changes exit-status or error reporting. Any failure leaves fields unknown, and a program that never started reports only `wallMs` on Unix. A different helper binary after an update simply omits the report. Older workers omit `usage`, which clients already treat as unknown. Secret-bearing `exec.run` reports the same numbers, which are not secret, while [D145](#d145-worker-local-command-secret-injection)'s all-or-nothing output suppression is unchanged.

This is accounting only. Usage imposes no limits, changes no admission, placement or `nodes.select` scoring, and is not aggregated anywhere. CPU or memory limits, for example through Job Object limits or cgroups, are separate future work with their own failure and enforcement semantics. See [command resource accounting](protocol.md#command-resource-accounting).

## D184: Online gateway database backup

Backing up the gateway meant stopping it and copying its state directory. Add `control gateway backup create FILE [--data DIR]`, run on the gateway host as the service account, which snapshots the SQLite database while the gateway serves, and `control gateway backup verify FILE`, which checks a backup read-only before an offline restore. There is no online restore.

The backup runs in the operator's process, not the gateway's. The documented systemd unit confines the gateway's writable paths to its state directory, so a gateway-side snapshot could not reach an operator-chosen destination outside it. A loopback reverse proxy makes any "local-only" HTTP check meaningless, and a remote superuser-triggered file write would be new network authority. Filesystem access to the state directory is already what an offline copy requires, so it stays the authority: no API route, no superuser key, and credential hashes never leave the host through Control. The command appears only in superuser help. `gateway.lock` only excludes a second serving gateway and is unchanged. SQLite's own WAL locking coordinates the extra connection, and on Unix the command refuses to run unless its effective user owns `gateway.db`, because another account's connection could leave `-wal`/`-shm` files the gateway cannot open. A separate `gateway-backup.lock` allows one backup at a time per state directory.

Consistency comes from SQLite, not file I/O. A separate connection runs `VACUUM INTO`; its WAL read transaction sees exactly the committed transactions, so the copy holds a prefix of acknowledged mutations ([D016](#d016-sqlite-backed-users-and-isolated-fleets)), without taking the gateway mutation mutex or blocking its writers ([D055](#d055-incremental-gateway-commits-and-separate-hot-paths), [D073](#d073-committed-observation-views-and-safe-performance-boundaries)). The WAL can grow during the snapshot because checkpoints cannot pass it. The temporary output is mode `0600`, synced because SQLite does not sync `VACUUM INTO` output, and checked with `integrity_check`, `foreign_key_check` and the schema version. It is then published create-only by hard link on Unix or a non-replacing write-through move on Windows. Failure or cancellation removes it. A schema newer than the executable is refused, as at startup.

Each published backup gets a `gateway.backup` event in the operator's history ([D082](#d082-durable-management-audit-log)), written by the command through its own short transaction before publication, so a published backup always has a record; a failed publication can leave a record without a file. The event holds the checksum, schema, size and host account, never the path or contents. Retention catches up on the gateway's next pass. The record is accountability, not access control, since anyone who can read the state directory can copy it.

Only the database is included. Release blobs are large and can be fetched or pushed again. `updates/deployment.json`, with rollout progress and the [D165](#d165-persisted-release-version-floor) version floor, cannot be captured consistently with the database while the gateway runs, and restoring stale rollout state could reselect older software or lower the floor. Runtime selections belong to the installed host, and the superuser key and TLS material live outside the state directory. A restore therefore keeps the host's current release storage, or on a new host starts without a selected deployment, and its floor restarts from the next selection. Everything committed after the backup is lost: later revocations are undone, later enrollments must enroll again, and later console sign-outs and password changes revert. The offline stop-and-copy method remains available when rollout state must be preserved exactly. See [back up and restore](gateway.md#back-up-and-restore).

## D185: Fleet-owner-managed machine labels

Labels existed only in each node's configuration file and its signed registration, so changing one meant editing a file on that machine and restarting its node. Labels are discovery and selection metadata that grant no authority, so the fleet owner may now set them centrally. The gateway stores an owner overlay per machine identity in a new `machine_labels` table (schema version 9) and applies it to directory copies. `g.nodes`, the persisted registration and the node's signed bytes keep exactly what the node sent; the gateway never re-signs or rewrites them.

Before relying on "labels grant no authority", every use was checked. Labels are read by `nodes.select` and workflow `select`/`each` resolution ([D180](#d180-plan-time-workflow-target-selection-and-fan-out)), by display and search, and by legacy client-migration heuristics over node-reported registrations. Access rules, delegation grants, artifact grants, runbook applicability, Wake-on-LAN evidence ([D084](#d084-automatic-interface-reporting-and-verified-lan-relationships)), proxy routing and update rollout do not read them. The only trust-adjacent use is a node's own `labels.lan` configuration deciding whether it reports interfaces to the first Wake-on-LAN gateways; it reads local configuration, not the directory, so an overlay cannot change it. Owner labels do decide where fanned-out work runs. That is intended, because only the fleet owner, who already holds execution authority, can set them.

The effective set is the node-reported labels without owner-removed keys, then owner values; the owner always wins. Removal is an explicit entry rather than an empty value, because selection treats an absent label as empty and an empty value would be indistinguishable. A removal also hides a key the node only reports after a later restart, so an owner decision cannot be undone by a node configuration change. `reset` discards the owner's entry and restores the node's value. Directory records carry the effective `labels` plus gateway-owned `ownerLabels`, `removedLabels` and the node's `nodeLabels` only when an overlay exists, so existing readers keep working and owner views can show each label's origin. Registrations cannot assert these fields; the gateway discards them.

`PATCH /v1/fleet/nodes/{id}` accepts `labels` with `set`, `remove` and `reset`, exactly one of `disabled`, `name` or `labels` per request. It mirrors rename authorization: account key, owner console session, or superuser on the legacy fleet. Keys are 1..63 letters, digits, dots, hyphens or underscores that start and end with a letter or digit, so the CLI's `KEY-` is unambiguous. Values are 1..128 characters from a conservative set without spaces, commas or `=`. A machine holds at most 32 owner entries including removals. Node-reported labels are not validated at registration, so these bounds are stricter than the node path; node keys outside the syntax can only change in node configuration. The overlay and its `machine.labels` audit event commit in one transaction ([D082](#d082-durable-management-audit-log)), the effective record is published only after commit ([D073](#d073-committed-observation-views-and-safe-performance-boundaries)), and connected fleet peers receive `directory.changed` as after a rename ([D043a](#d043a-owner-selected-machine-routing-aliases)). No admission revision, node acknowledgement or node upgrade is involved.

Older gateways reject a labels-only PATCH with HTTP 400 because neither `disabled` nor `name` is present, so they cannot silently acknowledge it. The client still checks `machineLabels` in `/v1/auth` and refuses before sending, giving a clean upgrade message and protecting against a gateway that might ignore the field. Older gateways refuse the version 9 database. Unregistration, forgetting and Hybrid timeout retirement delete the overlay in the same transaction as the registration; same-identity replacement enrollment keeps it, because the identity is the same machine. Hybrid User and System contexts are separate registrations with separate overlays. Unlike rename ([D151](#d151-automatic-atomic-hybrid-companion-renames)), no atomic pairing is needed: labels carry no naming or uniqueness relationship between the contexts, and instruction-only System contexts are never selected by labels anyway.

Selection, the dashboard and the console read the effective labels from the gateway directory on account routing and through a local node's API, so they need no separate code. The CLI adds `control machines label`, and the console edits the overlay from the machine drawer. This extends [D023](#d023-durable-owner-controlled-machine-lifecycle)'s owner controls and [D016](#d016-sqlite-backed-users-and-isolated-fleets)'s persistence without changing identity ownership or execution authority. See [owner labels](protocol.md#owner-labels).

## D186: Low-disk guard for task admission

Free-space reserves protected only artifact imports and uploads ([D076](#d076-artifact-budgets-and-reference-protection), [D121](#d121-resumable-local-to-worker-bulk-uploads)). A worker with a nearly full disk still accepted and started tasks, which then failed midway in arbitrary ways: a half-written output, a failed log append, or a failed terminal write that left only an in-memory outcome. New work is now refused at acceptance while free space on the filesystem holding `workDir/tasks` (or `workDir` before the first task) or `dataDir` is below `taskReserveBytes`. Zero selects 256 MiB, the same default as the other reserves, so an upgraded healthy machine behaves as before, and a machine below it would already refuse artifact imports. `-1` disables the guard for small dedicated volumes whose operators know their tasks fit. Other negative values are rejected. The field is signed, unlike the two existing reserves, because those have no way to switch the check off.

Refusing was chosen over holding the task in a waiting `phase`. A held task would keep its grant, lease exclusion and maintenance admission alive and occupy the 256-task bound while waiting for an event that may never come. It would contradict bounded queues, and its record would itself need disk space. The refusal uses code `insufficient_disk_space` and a new outcome, `not_accepted`: the destination received the request and refused it before accepting a task or starting any effect. `not_sent` would misstate delivery, and `unknown` would make callers reconcile something that cannot exist. The stream-bound receipt stays `rejected`, so the client still releases the attempt's fresh grants. The message names the filesystem and tells the caller to free space or choose another machine. No client retries or re-places work, and the CLI and MCP report `task ID was not accepted` instead of asking for reconciliation. Older clients show the same worker message, and older coordinators treat the unfamiliar outcome as uncertain, which is the safe direction.

The check runs after duplicate-ID reconciliation and before the durable write, outside the node lock. A resubmitted accepted or pruned ID therefore still returns its record or tombstone, and a refusal leaves no record, reserved ID, log or workspace. Only admission is guarded. Stage transitions, terminal writes, cancellation, observation, retention, tombstones and cleanup intents of accepted tasks never consult it, because they are how space is reclaimed. Synchronous `files.write` and `files.copy` are guarded too, since their purpose is adding bytes to the workspace. `files.remove`, `files.move`, `files.mkdir`, `artifacts.delete`, upload abort and synchronous commands stay available as recovery routes. Commands are not classified by arguments, and a cleanup command is as likely as a large write. Artifact imports, `http.download` and uploads keep their own reserves.

Acceptance is a hot path ([D057](#d057-blocking-task-waits-and-acceptance-outside-the-node-lock), [D171](#d171-in-memory-queued-to-running-transition)), so one cached reading serves every admission for at most two seconds, with at most one probe in flight. Admission waits at most one second for a probe. A failed probe (unsupported filesystem, permission error) or one that has not finished is unknown and never refuses work. Its error is reported, without local paths, in `diagnostics.get` under `taskDisk` with the reserve, the latest reading, the state and whether admission is refused. A probe that hangs in the system call cannot be cancelled; it leaves at most one goroutine blocked rather than stalling acceptance. Free space is what the node's account can use: `f_bavail` through gopsutil on Unix, and the caller's quota-aware value from `GetDiskFreeSpaceEx` on Windows in a platform file, because gopsutil reports the volume total there.

Health reports already sample free space for both directories, which `nodes.select` `minDiskBytes` and the dashboard use, so no health field or dashboard state is added. Those samples are up to one metrics interval old and remain advisory; the destination's own check decides admission.

A workflow child refused this way was provably never accepted. The coordinator classifies `not_accepted` like `not_sent`, as a settled failed step, so cleanup steps whose needs it blocks can start, as in [D177](#d177-pre-planned-workflow-cleanup-steps). Cleanup aimed at the same low-disk worker is refused too and reported as a failure. Reconciliation finds no child in the parent result and no task at the destination, and reports `notStarted`. The guard is a threshold, not a reservation. Accepted tasks and other processes can still fill the disk, and terminal-write failures keep their existing in-memory reporting. This extends [D007](#d007-durable-acceptance-with-conservative-restart-recovery)'s acceptance rule without changing durability, idempotency or restart recovery. See [low-disk admission](protocol.md#low-disk-admission).

## D187: Directory transfers over existing upload and artifact mechanisms

Uploads and `artifacts.export` took one regular file ([D121](#d121-resumable-local-to-worker-bulk-uploads), [D008](#d008-immutable-artifacts-and-recipient-initiated-transfers)). Moving a project to a worker, or collecting a result directory, meant archiving by hand with `exec.run tar`, which differs by operating system and has no recovery. Two bounded additions reuse the existing contracts instead of adding a directory stream.

`control upload start NODE LOCAL_DIR REMOTE_DIR --recursive` is client-side orchestration. `--recursive` is required, so a mistyped path cannot send a tree. The client first scans the whole tree without following links and validates every relative path with the receiver's own rules, now exported as `upload.ValidatePath`. It refuses symbolic links, special files, Windows reparse points, rejected names, and names that differ only by Unicode simple case folding, since Windows and default macOS destinations are case-insensitive. A refusal lists the count and the first ten paths and sends nothing. `--skip-unsupported` is the explicit opt-in to skip them, but never a case collision, because choosing one of two names would be arbitrary. Normalization-equivalent names, which only macOS conflates, are not detected; their second upload fails with `destination_exists`. The limits are 512 files, 512 directories, each file within the worker's advertised `maxUploadOwnerBytes`, and the tree within `maxUploadStagingBytes`, all checked before anything is created. 512 files is half the worker's 1,024 retained records per owner, because each published file keeps a receipt for the retention period. A tree that needs more should be split or archived.

The remote root must not exist. `files.mkdir` without `parents` ([D174](#d174-rooted-file-mutation-primitives)) creates it exclusively, so a new transfer never merges into an existing directory. The local recovery record is written only after that succeeds. A lost response therefore leaves at most an empty root and no record, and starting again fails visibly rather than adopting a directory it cannot prove it created. Leaf directories are then created with `parents`, which preserves empty directories, before any file. Each file is one unchanged `file-upload-v1` upload with its own permission check and, on instruction-only destinations, its own exact single-use grant. No broader grant exists, and the worker needs no change beyond `files.mkdir`. Its ID is `d` plus 40 hex digits of SHA-256 over the transfer ID and relative path, so a resume finds each file's existing record. Two files upload at a time, matching the worker's two active uploads per owner. The first failure stops new files while in-flight files finish, and every goroutine ends with the call.

There is no atomic whole-tree publication. Each file appears atomically, but other programs can see a partial tree, and nothing is ever deleted on failure, cancellation or abort. Making partial state visible and resumable was preferred to staging the whole tree and renaming it. A rename would need a new receiver protocol, would charge the whole tree to staging at once, and on Windows cannot be made both atomic and no-replace for directories. The client's private recovery record, beside the single-file records, binds the worker identity, root, skip setting and sorted names and sizes. Resume rescans and requires the same names and sizes. Each file's content is bound by its own record when it starts, as before. Files this client saw published are not contacted again, and started files resume from their durable checkpoint. A file whose earlier open the worker refused before reserving anything, for example because of a lease ([D131](#d131-lease-sensitive-upload-and-clipboard-admission)), reports `upload_not_found` on resume. That is a definite refusal, not an uncertain effect, and reopening it as new is still create-only, so the client does so once. Status and abort accept the transfer ID and return one compact summary: counts by state and at most ten failures. Abort abandons only unpublished partials.

MCP `control_upload` refuses a directory `localPath` and points to the CLI. Its definitions have no room for an explicit recursive opt-in, and accepting directories implicitly would reintroduce the typo risk. Status and abort return the directory summary through the unchanged tools.

`artifacts.export` with a directory path now produces one immutable `tar-v1` artifact. It is a PAX-format tar written with the Go standard library: entries sorted by relative path, directories before their contents, normalized ownership, fixed `0755`/`0644` modes, the Unix epoch as modification time, and no absolute or parent paths. Identical trees are therefore identical content-addressed artifacts on every operating system. Symbolic links, special files, non-UTF-8 names, private upload staging, the filesystem root, more than 10,000 entries and more than 1 TiB of file data are refused. The archive streams through the existing import staging and artifact byte budget, and a file that changes while archived fails the export without publishing ([D076](#d076-artifact-budgets-and-reference-protection)). `node.describe.artifactExport` advertises the format, because older workers reject a directory path. No request field selects archiving, since an older worker would ignore it and silently export a file. Receivers never extract automatically: extraction is where path traversal bugs live, and a consumer can extract with a tracked task that checks entry names. Declared task outputs remain regular files. See [directory uploads](uploads.md#directory-uploads) and [directory export](uploads.md#directory-export).

## D188: Worker-enforced secret use policies

Any authorized `exec.run`, `desktop.run`, `http.request` or `http.download` invocation could resolve any worker-local secret, so a mistaken or prompt-injected caller could inject a credential into the wrong command or send it to the wrong URL. Add an optional per-secret use policy, stored with the secret on the worker and checked when a reference resolves. This is a narrow form of the approved-destination broker [D041](#d041-worker-local-secret-references-for-credential-entry) named as missing. It does not narrow provider authority.

A policy lists permitted uses: `http` for `headerSecrets`, `exec` for `envSecrets`/`stdinSecret`, and `desktop` for native `setValue`. An `http` policy also requires 1..16 host patterns. A pattern is an exact host, a leading `*.` wildcard that matches one or more labels in front of a suffix of at least two labels, or an exact IPv4 or bracketed IPv6 literal, each with an optional port. Without a port, a pattern matches the scheme's default port only. Only HTTPS is permitted unless `plainHTTP` opts in. Matching uses the parsed URL the HTTP client connects to. Userinfo, zoned and IPv4-mapped addresses, and numeric final labels are refused. Host names are ASCII and compared in lowercase with one trailing dot removed. Unicode names are refused rather than converted, because matching an IDNA mapping other than the client's could admit an unnamed host, and adding `golang.org/x/text` for IDNA was not justified. Redirects stay unfollowed for secret-bearing requests, so the checked URL is the only destination. `exec` and `desktop` are allow/deny only. A command name is chosen by the caller the policy constrains, and an unrestricted process can spawn anything under it, so an allowed-command list would be false assurance.

Secrets without a policy keep their behavior. A policy is deny-by-default. One shared `Resolver.Resolve` checks all references of an invocation before any value is returned, so one refusal fails the invocation with nothing launched, set or sent. Masking still reads every value from the same snapshot. Errors name the secret and the refused use or "HTTP destination", never values, hosts or other uses, which callers without management access cannot list. A source-scanning test keeps the set of production call sites that obtain unchecked values (for redaction and startup validation) closed.

The policy is stored verbatim in the secret's `values.json` record, under the existing lock and atomic replacement, bounded to 2 KiB and strictly validated in canonical form on load. A present but invalid policy, including `null`, refuses every use of that secret and is preserved by unrelated writes. Stores holding a policy carry `"#format": 2`. Older binaries reject it as an invalid name and refuse the store instead of silently ignoring policies, so a downgraded node fails to start until policies are cleared. Policy-free stores keep their previous bytes. Value-only sets keep the stored policy. A local `control secrets policy` command changes the policy without the value or its entry timestamps.

Remote writes gain envelope version 2. Its plaintext carries canonical policy JSON or `null`, a NUL and the value. `control-secrets-v2` replaces the v1 label in both HKDF info and AAD, so relabelling an envelope makes it fail to open, and a worker that predates policies cannot decrypt one as a bare value. Workers advertise `secretPolicy` in signed registration. Clients refuse to read a value for, seal or send a policy write unless the gateway reports that advertisement, and gateways refuse version 2 to workers without it. A false advertisement can only make the write fail. Version 1 writes, including the console's, keep the stored policy, so a value edit cannot silently drop a restriction. Workers include policy metadata only in listings whose request asks for it, because older gateways disconnect on unknown reply fields. Those listings are bounded at 512 KiB. Remote policy changes always carry the value. Envelopes have no sender authentication, and the gateway can create its own, so a value-free remote change would let a compromised gateway lift the restriction on a credential it cannot read. The gateway's audit records are unchanged and contain no names or policies.

This stops accidental and prompt-injected misuse through structured secret references. It is not a sandbox. Any `exec.run` on the worker, custom providers and same-account processes can read or rewrite the store. A permitted command can forward the value. HTTP host matching trusts DNS, which matters when plain HTTP is allowed. As in [D116](#d116-encrypted-owner-managed-worker-secrets), the gateway stays trusted for key discovery, so one that substitutes keys can read and alter both value and policy. Displayed remote policies are not authenticated, and encrypted writes within their lifetime are not ordered. The console does not display or edit policies. See [use policies](secrets.md#restrict-where-a-secret-can-be-used).

## D189: Bounded SOCKS5 capacity on shared peer sessions

A browser can keep over 128 SOCKS connections open even when few are actively transferring. Closing socket 129 before the SOCKS greeting broke requests. Raising only the listener limit would move the problem to the peer stream limits and leave a `CONNECT` waiting indefinitely for a slot.

The process-owned SOCKS listener defaults to 256 connections and accepts 1–384. The upgraded interactive session carries up to 448 streams, leaving at least 64 slots beyond one maximum-size forward. The outgoing interactive pool and separate incoming interactive handler pool each have 512 slots per process. Incoming legacy, control and bulk traffic keeps its separate 512-handler pool; fixed forwards keep their local socket limit. Interactive endpoints negotiate `control-interactive-streams-v1` through pinned TLS ALPN over either existing transport. If either end is older, the session stays at 128 slots. This needs no gateway negotiation or additional peer link, and does not change SOCKS, TCP duplex, or authorization.

A listener accepts at most 32 overflow waiters with short handshakes and gives a valid `CONNECT` up to 500 ms to acquire a slot. The peer caller also waits up to 500 ms for `tcp.open` stream slots. Negotiated receivers bound their own overflow queue and report `stream_capacity` without invoking the operation. At a full queue, a bounded reply path rejects rather than creating more waiters. The proxy uses SOCKS general failure (`0x01`) and increments `capacityRejections`. Shutdown closes tracked waiters and existing connections; busy or lost streams are never replayed. No idle eviction is introduced: byte-idle tunnels can carry valid long-lived sessions, and the proxy cannot distinguish an idle HTTP/2 pool entry from an idle application buried in TLS. See [streaming limits](streaming.md#limits-and-cleanup) and [SOCKS proxies](tunnels.md#socks-proxies).

## D190: Loopback HTTPS proxy with HTTP/2 CONNECT

Offer a second application-configured proxy without replacing SOCKS5 or introducing a system route. The client hosts a loopback-only TLS/HTTP2 listener. Apps can share a TLS connection to the listener across independent `CONNECT` or plain HTTP requests. Each request still uses one ordinary authorized `tcp.open` to the named worker, so DNS, destination sockets, accounting, and instruction-only restrictions are unchanged. Website TLS passes through untouched. Plain HTTP is forwarded as HTTP/1.1 from the worker to the origin without following redirects or keeping idle origin sockets. There is no worker proxy service, gateway change, or HTTP/1.1 fallback on the proxy listener.

Generate a new leaf and private key in memory for each listener. Expose its public key hash for per-launch pinning, not a shared CA or changes to system trust. Loopback is not process isolation. Bound TLS connections, valid `CONNECT` slots, waiters and response writes; close and join when the process-owned listener stops. See [HTTPS proxies](tunnels.md#https-proxies-over-http2).

## D191: Bounded gateway HTTP pools for proxied nodes

The managed gateway proxy does not fan out to website destinations, but its 256 admitted connections are shared by all dependents, installers and probes. A dependent's pooled HTTPS requests previously had no active connection limit. Cap ordinary proxied gateway HTTP connections at eight per process, proxy, and gateway authority. Use a separate non-pooling transport for peer registration, with one concurrent dial per authority for authentication followed by the gateway WebSocket upgrade. Relay traffic keeps using that socket. Both transports use the same TLS verification, proxy-only routing and maintenance fence. Direct gateway clients retain their previous transport.

Keep the managed listener cap and close-on-overflow behavior. Raising the cap would not constrain an individual dependent, and a limit per source IP would make multiple nodes behind one address compete for the same quota. Report current admissions, the process-lifetime peak, the cap, and capacity refusals through existing worker diagnostics without reporting addresses or counting source-policy or maintenance refusals. Older dependents and external clients do not acquire a client-side bound until updated, and a large group can still exhaust the shared cap. See [managed gateway proxy](gateway-proxy.md#managed-listener).

## D192: Opt-in interactive shell streams

Use a dedicated `shell.open` stream on the existing interactive lane rather than treating a terminal as a durable command task or asking operators to run an SSH server. The worker opts in with `shellEnabled` and applies the named method permission separately. Reachability, audience, and worker OS privileges stay unchanged. An instruction-only destination can issue a single-use grant to the requesting account client for that exact session, but cannot delegate the session to a worker. No local HTTP or MCP adapter exposes the stream.

The connection owns the terminal. Input, resize, output, and exit status have bounded binary records. The worker reserves an execution slot and excludes leases and updates for the session's lifetime. Disconnect, expiry, or revocation tears down the pseudoterminal instead of retaining it for reattachment. Control records operational metadata but does not record a transcript. This keeps session ownership and cleanup explicit without adding gateway state or changing the TCP tunnel protocol. It supersedes [D030](#d030-concurrent-client-connections-with-stable-task-owners) only on PTY support. See [interactive shells](protocol.md#interactive-shells).

Windows owns descendants through a Job Object. On Linux the worker keeps the exited shell unreaped while targeting verified session members through pidfds, avoiding reuse of a PID or session ID. macOS and Linux without pidfds close the PTY and rely on kernel hangup rather than signalling unrelated processes by a stale numeric PID. This is not job containment; an escaped Unix process can survive, and cancellation and the exit drain deadline close the terminal even if another process still holds its slave.

## D193: Start User shells in the worker account's home directory

An interactive User shell starts in the home directory reported by the worker's OS environment. Use the same OS account classification as worker registration, rather than the caller's home directory, the machine name, or an instruction-only flag. A Hybrid System profile, a System account, or an unknown OS account classification retains `workDir`. An unavailable User home fails the open instead of silently choosing another directory. This does not change task workspaces, filesystem roots, execution authority, or the shell wire protocol. See [interactive shells](protocol.md#interactive-shells).

## D194: Opt-in local Vault for CLI and MCP authority

An enabled frontend contacts a pinned loopback broker running under a different OS identity. The broker keeps the fleet credential and binds approval to one live connection. CLI and MCP use the same boundary. A code identifies a request; an operator must separately prove approval. No account key passes to the approved process. The approval covers all nodes subject to destination permissions, and makes no change to the gateway or worker protocol.

Tunnel management crosses this boundary; traffic through a listener does not. Retained definitions and their restoration authority stay in the broker account. Grant expiry does not change tunnel TTLs or cancel accepted worker tasks. See [Control Vault](vault.md).

## D195: Gateway and fleet locks restrict worker admission

The owner selector in this decision is superseded by [D198](#d198-select-access-by-key).

The operator may require an access allowance across all fleets; a fleet owner may require one independently. A lock is a restriction on existing execution authority. Each allowance matches the authenticated orchestrator key, its stable owner identity, or both, and specifies all workers or particular immutable worker IDs. One allowance must cover the workers involved in the instruction. A delegated worker still needs its destination-issued instruction grant. Common and installation credentials do not become execution keys.

The console manages policy with a browser session and CSRF protection. Changing local Vault settings cannot change fleet policy. Workers check admission on established peer sessions as well as new ones, because rejecting gateway signaling alone cannot stop new commands over a warm WebRTC connection. The gateway decides access from identities and timestamps without receiving command payloads. See [lock worker access](console.md#lock-worker-access).

## D196: Execution-only orchestrator keys

A fleet previously had one execution credential, its account key, which also administers the fleet. Owners can now issue any number of `orchestrator` keys through `POST /v1/fleet/keys`. The gateway gives their account-client sessions `executionAuthority`. It refuses them for machine enrollment and every fleet, audit, console-password, and gateway administration route. Common and installation keys stay discovery-only and are never promoted. Owners cannot issue `user` or `superuser` keys this way. Older gateways ignore the role and would issue a common key, so the CLI requires the `orchestratorKeys` advertisement before sending. It verifies the issued role and withholds a mismatched secret instead of retrying.

The standalone client owner identity stays scoped by gateway URL and fleet, not by key. One host keeps a stable owner selector across key rotation, account and orchestrator logins, and Vault migration. Tasks belong to that proven owner, and the same OS account is not a sandbox between keys. Admission distinguishes keys only by the gateway-attested key ID of each client session. CLI login, tunnels, Wake-on-LAN, and Vault provisioning and runtime accept the role; fleet administration commands do not. Worker `allow` rules and fleet locks still restrict it like any account client. See [fleet key roles](protocol.md#fleet-key-roles).

## D197: Label saved client profiles independently of execution authority

Superseded by [D198](#d198-select-access-by-key).

Access lock refers to stable owner identities as client profiles: multiple profiles may run on one computer. The fleet owner can label a profile after matching `control session`'s `ownerId`. The gateway records connection and disconnection times; legacy records have no historical timestamp. Labels and presence help people choose an identity. They neither replace the signed owner nor change an allowance. See [Lock worker access](console.md#lock-worker-access).

## D198: Select access by key

This replaces D195's owner selector and D197's profile picker. Each allowance names one key, its workers and an optional expiry. Orchestrators sharing a key share its access; separate keys can have different allowances. Internal owner identities still bind tasks and instruction grants. Upgrades remove host-restricted allowances instead of broadening them.

Owners can issue `orchestrator-enroll` for enrollment, discovery and account-client execution without fleet administration. Existing `common` and `orchestrator` keys keep their original permissions. The console defaults to the combined choice; the default for existing CLI and API calls stays `common`. See [fleet key roles](protocol.md#fleet-key-roles).

## D199: Select key permissions individually and recover through the console

The console shows granted permissions instead of role presets. Discovery is inherent to fleet membership; enrollment and execution are independent. The [permission-list endpoint](protocol.md#fleet-key-roles) rejects unsupported permissions and preserves existing role-based clients.

Revocation stops key authentication. A fleet owner can restore the original credential through the password-authenticated console or permanently remove its revoked record. Only the console can reactivate keys because restoring the same ID also restores any still-valid key allowances. For compromise, the owner can leave the old key revoked and obtain a fresh account key even after removing the old record. Only one account key can be active per fleet. A replacement never inherits allowances by name. Renaming changes only the label.

## D200: Separate fleet management from account recovery

Named keys may grant fleet management independently of enrollment and execution. This extends D199's permission list. A manager can grant at most its own permissions. The account key and password-authenticated recovery remain distinct from named keys, even when the permissions displayed for two keys match. See [fleet key roles](protocol.md#fleet-key-roles).
