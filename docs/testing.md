# Testing with Docker Compose

[Contributor guide](../AGENTS.md) · [Architecture](architecture.md) · [Engineering principles](principles.md) · [Design decisions](decisions.md)

Use Docker Compose for repeatable integration testing. Docker supplies Go, FFmpeg, and the node runtime, so the host only needs Docker Engine or Docker Desktop with Compose v2 or newer. The Make targets also need Make and a POSIX shell.

## Run the tests

Secret-panel browser checks also require successful creation/replacement and create/edit Cancel to return to the list. Cancel clears entry controls without writes or changes to stored values, including after an uncertain write. A simulated list-refresh failure after a successful write offers only a metadata reload and never repeats the write.

Remote secret fixtures cover encrypted CLI envelopes, tampering and recipient/name/expiry binding, process-key rotation, replay rejection and bounded retention, destination allow/disabled/maintenance checks, fleet isolation and common-key denial, metadata-only responses, sanitized errors, CSRF, current-connection replies, no redirect/retry, and absence of plaintext in gateway persistence or task files. Browser smoke tests exercise real Web Crypto encryption decrypted by the Go worker, add/replace/delete, preserved stored creation dates, full-height right-side panels, accessible icon actions, updated-only timestamp display, long names and dates without horizontal scrolling at mobile/desktop widths, masked single-line textarea-backed editors with fixed height and no resize handles, add/edit focus, absence of HTML inputs and login forms, disabled autocomplete, invalid-name/multiline rejection, Enter suppression, cancelled/uncertain writes, cleared inputs and no browser-local value storage. Native Safari Passwords and third-party password-manager suggestions need manual browser verification; DOM checks do not prove those browser-owned prompts are disabled. No real credentials or user fleet are used.

MCP/skill instruction tests require brief on-demand GUI setup guidance, approval before installing the latest compatible upstream FFI release, SHA-256 verification against that release's checksums, ABI major 6 and 96-byte action-layout validation, rejection of incompatible libraries before credential use, private companion-file placement, and an idle User-profile restart. They do not require a specific upstream release number. The Compose update test builds with no internet access and checks that the bundle contains only its Control binary and metadata, not desktop dependencies.

Wake-on-LAN core tests use synthetic interfaces, fake packet writers, and loopback sockets, never broadcasts on the host LAN. They verify magic packets, exact interface/relationship matching, private-address and MAC validation, deduplication, cancellation, partial failure without replay, CLI/MCP contracts, automatic candidate selection, signed proof freshness and source binding, gateway-owned relationship persistence, transaction rollback, and fleet isolation. Peer tests over WebRTC and relay check capability authorization and disabled-sender admission using fake sends. Compose verifies real signed UDP broadcast discovery and all directed relationships among three isolated containers, including with relay-only Control connections and without LAN labels. Actual NIC wake support needs native hardware smoke tests. See [Wake-on-LAN](wake.md).

Rolling-upgrade fixtures emulate the original Wake-on-LAN gateway's rejection of unlabeled interface metadata. Synthetic private NICs ensure the check runs even on public-address-only CI hosts. They verify that new nodes reconnect to older gateways without reporting automatic metadata, preserve explicit legacy-label reporting, omit metadata with legacy registration encoding, and automatically enable collection when the gateway advertises discovery. No discovery broadcast is sent by these fixtures.

Standalone-orchestrator tests parse synthetic macOS, Windows, and Linux neighbor tables and verify exact IP/MAC/interface binding, failed/incomplete entry rejection, direct target evidence without enrollment, witness identity/interface matching, expired relationships, output bounds, and rejection of subnet-only guesses. These tests do not send packets or modify the host's neighbor table. Native neighbor-tool availability and hardware wake-up still require platform smoke tests.

Gateway tests also cover the embedded console's separate browser passwords, session/CSRF boundaries, fleet management, expiry, revocation, HTTPS/origin policy, bounded password work, transactional failure, and database upgrades. The optional [browser smoke test](console.md#verification) checks real desktop/mobile rendering and management flows against an isolated temporary gateway and node. Node.js and Playwright are test-only dependencies, not deployment requirements.

Frontend edits also require the local `make typecheck-console` check after `npm --prefix tests/console ci`. This runs TypeScript 7 strict, no-emit JSDoc checking over every shipped browser JavaScript file. There is no separate type-coverage tool or frontend typing CI job. API declarations must follow Go response/request contracts; contextual callback/local inference counts as typed, but `any` and suppression comments are not permitted. See [frontend typing](../AGENTS.md#frontend-typing).

Bundle-builder unit tests verify JavaScript comment/JSDoc removal, preservation of URLs, regular expressions, template-literal text and top-level names, unchanged source files, and rejection of missing or invalid source. Use `make build` before browser smoke tests to exercise the comment-stripped embedded copy rather than a direct Go development build. See [console bundling](console.md).

Console restart tests retain account and operator cookies and CSRF tokens across repeated gateway restarts, preserve original session deadlines and recent activity, and reject signed-out, replaced, password-reset, disabled, and expired sessions after restart. They verify schema version 6 migration, digest-only cookie storage, and failed sign-in/sign-out writes without false success or leaked sessions.

GPU tests use synthetic NVIDIA CSV, Windows/macOS inventory JSON, and Linux DRM device files. They cover unavailable readings versus valid zeros, count/text/output bounds, command cancellation, isolated cache copies, health validation and fleet visibility, legacy registration encoding, offline usage suppression, and console sorting/Hybrid fallback with multiple GPUs. Compose runs the Linux file fixtures without exposing host GPUs. These tests do not verify real Windows/macOS driver telemetry, NVIDIA installations, or GPU passthrough; those need hardware smoke tests. See [GPU resources](dashboard.md#gpu-resources).

Accelerator tests also cover discrete/integrated/virtual/NPU classification, unknown-device suppression, LUID-matched adapter-wide memory counters above 4 GiB, linked physical adapters, valid idle zero readings, overflow rejection, and synthetic Linux NPU driver links. Browser fixtures verify that loaded memory, not compute load, drives bars and sorting, including AMD-style memory-only reports. Windows-native `TestWindowsDXCoreInventoryAndMemory` exercises available system APIs without elevation and skips if DXCore inventory is unavailable. Actual readings still need comparison with Task Manager on a GPU-equipped Windows host; cross-builds and synthetic parsing do not establish runtime driver compatibility.

The suite also runs generated Bash installers in the test container, verifies online registration and remote execution, rejects reuse and common-key invitation creation, and checks login followed by host setup without credential environment variables, plus MCP/skill installation. Unit tests cover saved login reuse, private credential files, failed-login preservation, concurrent redemption, expiry, revocation, name reservations, and configuration preservation. These Linux container tests do not exercise native launchd or Windows SCM startup.

Private-fleet tests create two user accounts with duplicate machine names, multiple orchestrator nodes, and a worker installed through a user-owned invitation. They verify saved account credentials, same-fleet execution over the required transport, and rejection of cross-user discovery, execution, tasks, artifacts, observation, and invitation revocation. Native tests also send forged signaling/relay packets directly to the gateway, check receiver membership and cross-user artifact grants, reject identity reassignment, and verify transactional JSON-to-SQLite migration and account persistence.

Delegation tests use account clients even when a local API is listening. Over WebRTC and relay they verify delegated execution through another worker, discovery-only node/common-key credentials, changed-argument and wrong-subject rejection, single-use synchronous instructions, original task ownership, idempotent workflows, direct artifact delivery, owner-only revocation, cancellation of durable child tasks, and revocation of derived output access. Node tests cover one-hour idle expiry, active-task retention, stream-traffic renewal, reverse-listener keepalives that do not renew access, and revocation of an established TCP stream. Gateway tests reject peer-supplied execution authority. Existing peer fixtures now separate account clients from enrolled workers.

Standalone client tests exercise named calls, owner-scoped task recovery across invocations, resumed artifact downloads, and cancellable TCP tunnels over both WebRTC and relay. Concurrent clients use independent TLS identities while sharing task submission, cancellation, logs, and lease ownership. Closing the submitting client shuts down its listener and sockets without cancelling durable work. They check account and independent-owner isolation, worker authorization, exclusion of clients from machine discovery and the dashboard, and refusal to switch backends after local authentication or application failures. Gateway tests cover role-bound and owner proofs, forged metadata, immutable roles and bindings, client credential revocation, transient routing metadata, exclusion from managed updates, and database migration of legacy CLI registrations without losing ownership. Negotiation tests reject older gateways and workers before connecting or sending work. CLI tests execute a subprocess with only a saved login, exercise detached submission, timeout selection and followed logs, and verify that explicit API settings disable fallback. MCP tests cover session inspection and start/list/stop forwarding through the local API, socket cleanup, listener limits, and closed-client rejection. These tests run in the core suite inside Compose as well as native CI.

Standalone operations, connection tests, private-fleet checks, MCP workflow recovery and MCP discovery-authority fixtures use per-instance IPv4 loopback candidate settings for their workers and clients, including replacement clients. These constructor-only options are not saved in node or tunnel-service configuration and do not change production networking defaults or unrelated fixtures. ICE, DTLS, pinned TLS, RPCs and strict WebRTC/relay assertions remain real. Replacement-client assertions also check the required transport after owner recovery. No global override or host network change is used.

MCP discovery-authority checks use separate bounded contexts for negative and positive phases. Negative checks require the expected remote execution-authority rejection, not merely a timeout or other error, and still verify that no configured subprocess ran. A no-candidate case forces two cold five-second direct attempts before relay fallback, covering the deadline separation without extending production setup timeouts.

Streaming tests also verify concurrent traffic lanes, one WebRTC carrier across lane channels, control traffic during blocked bulk writes, sibling multiplexer closure, independently cancelled slow-peer setup, stream-capacity cancellation, receive-packet bounds and buffer ownership, changing write deadlines, TCP EOF-driven responses through standalone and local API forwards, malformed duplex records, legacy raw/polling fallback, and binary log offsets with cancellation and owner checks. Adapter microbenchmarks and measured allocation changes are documented in [fleet streaming](streaming.md#verification-and-measurements).

Network-recovery tests inject changed samples without changing the host network. Synthetic interface/route fixtures reproduce macOS alternating temporary IPv6 gateway sources on an otherwise unchanged dual-stack network. They verify stable fingerprints during IPv6 rotation and unrelated address/adapter changes, detection of selected-interface/subnet, IPv4 DHCP and reachability changes, scoped IPv6 targets, and rejection of incomplete samples. Established WebRTC and relay streams in every lane survive the alternating-source samples on either endpoint. Recovery tests also cover lane retirement on a real switch, gateway reconnection, fresh peer setup, cancellation of old setups, rejection of old-network session publication, exclusion of disconnected carriers from inspection, recovery-grace cancellation, and no replay of interrupted bytes. Gateway tests check signed heartbeat negotiation, unchanged legacy proof bytes, and retirement of an endpoint that answers WebSocket pings but not the application nonce, followed by same-identity reconnection. Dashboard tests preserve peer-probe warnings alongside fresh health. Native VPN/Wi-Fi switches still require smoke tests on Windows, macOS, and Linux.

ICE tests use a local, non-answering STUN fixture to verify that trickle connects before gathering completes, while legacy peers retain complete-SDP gathering. The compatibility fixture allows ten seconds for direct setup because legacy negotiation includes two sequential two-second STUN gathers before signaling and the handshake finish. This leaves headroom on race-enabled CI runners without changing production timeouts, the trickle timing assertion, or the requirement for an actual direct connection. They cover bounded sender/session-bound early candidates, gateway STUN inheritance and explicit opt-out, direct/TURN/gateway route classification, fleet-scoped candidate routing, shared lane failure backoff, failed re-probes retaining the working relay, and successful direct replacement without interrupting existing relay streams. Real internet NAT traversal and live TURN allocation still require deployment smoke tests; no test contacts a public STUN provider or changes host networking.

Persistent-tunnel tests use isolated user state and local peers over WebRTC and relay. They cover non-blocking durable acceptance, client/service/destination restart, pinned target identity, forward/reverse traffic, duplicate-ID reconciliation, changed-specification rejection, fixed expiry, disposal surviving restart, local API authentication, credential-scope isolation, failed writes, dashboard counts, and CLI/MCP adapters. Compose also starts real detached client-service processes through the CLI, verifies both directions across containers, restarts the owning process, checks dashboard counts, and verifies that disposed definitions do not return. Startup-generation tests cover quoting and separate user service entries. Native LaunchAgent/systemd/Task Scheduler installation, login/reboot recovery, and privileges require OS runtime smoke tests. No test installs real user startup or touches the user's fleet. See [persistent tunnels](tunnels.md).

Persistent-tunnel restart fixtures choose fixed ports below the supported systems' default ephemeral ranges, avoiding reuse by unrelated outbound sockets while a listener is down. A deterministic endpoint-allocation test forces the service's management listener to choose a retained forward port first, verifies that it selects another port, and checks release of the rejected candidate for IPv4 and wildcard listener specifications.

RPA tests use a fake helper to verify opt-in, whole-batch validation, authorization, leases, durable task idempotency, per-user serialization across profiles, cancellation, partial failures, screenshot path confinement, and PNG artifacts. Peer tests cover both transports without GUI dependencies. Dependency-free Python tests cover exact selector uniqueness, traversal bounds, backend validation, Wayland rejection, and key/button cleanup. Run them with `python3 -m unittest discover -s examples/rpa -p 'test_*.py'`. Live desktop behavior and permissions require native smoke tests; see [GUI automation](rpa.md#verification-and-limits).

Secret tests cover local argument/stdin contracts, private atomic storage, concurrent changes, workspace rejection, reference validation, helper-side injection, chunked diagnostic suppression, masked GUI/HTTP outputs, missing references, literal-header conflicts, and redirect refusal. Timestamp fixtures check persisted creation and update dates, same-value sets, deletion/recreation, legacy entries with unknown dates, unchanged unrelated metadata, failed/cancelled mutations, sanitized corrupt-record errors, and value-free opt-in listings while preserving names-only output. Peer tests over WebRTC and relay verify reference-only durable task metadata, masked retained results, empty helper logs, idempotent reconciliation, and absence of secret management RPCs. Text masking does not verify screenshot privacy or native credential fields. See [limits](secrets.md#what-this-protects).

Native `desktop.run` tests use a Go subprocess stand-in to verify opt-in, private state, strict input validation, pipe-only credential injection, agent-desktop environment isolation, masked status/observations/errors, split stderr suppression, malformed and oversized output refusal, missing references, cancellation, reference-only persisted tasks, empty logs, and idempotent reconciliation. Purego callback fixtures verify actual ABI calls, the 96-byte action layout, argument marshalling, pinned buffers through garbage collection, output bounds, native resource release, and failed-action envelopes. Packet/helper tests reject malformed, truncated, oversized, trailing, and unsupported input without exposing diagnostics. CLI subprocess tests check `__desktop` routing before profile/account initialization. Default tests need no compiler, FFI library, or live desktop. The optional `CONTROL_TEST_DESKTOP_LIBRARY=/absolute/path/to/library CGO_ENABLED=0 go test ./cmd/control -run '^TestDesktopCLIRealLibraryStatus$' -count=1` checks real loading and private native environment settings without GUI mutations. Actual Windows DLL loading, OS permissions, and live application behavior still require native smoke tests. See [native library setup](../examples/agent-desktop/README.md).

Clipboard tests use fake desktop reads/writes without touching the host clipboard. They cover files larger than the control-frame limit, metadata-only source snapshots, delayed streaming until paste acceptance, checksums, empty files, source changes, collision refusal, portable names, URI parsing, cancellation cleanup, workspace confinement, method authorization, admission, local API relay, standalone clients over both transports, and reverse MCP file paste. Native Finder/Explorer/Linux file-manager integration, Unicode desktop text, user-session access, and clipboard tool availability require platform smoke tests. See [clipboard requirements](clipboard.md#desktop-requirements).

On macOS, `CONTROL_TEST_MACOS_CLIPBOARD=1 go test -race ./internal/clipboard -run '^TestClipboardDarwinPrivatePasteboard$' -count=1` verifies native Foundation/JXA reads with Unicode text, empty text, multiple file URLs, and unsupported formats. It uses a private pasteboard, never the user's current clipboard. Real Finder copy and destination text writes still require a desktop smoke test.

From the repository root:

```sh
make test-compose
```

This builds the images, runs vet and the race-enabled Go suite, and runs cross-container tests twice. The first deployment requires WebRTC connections. The second uses fresh node state with `relayOnly: true` and requires relay connections. A silent fallback to the relay fails the WebRTC test.

The script runs the core Go suite once, preserves the failing test's exit status, prints service logs on failure, and removes its containers, network, and volumes when finished. Each invocation uses a unique Compose project name. Images and build caches remain for subsequent runs.

Run one transport mode when investigating a failure:

```sh
make test-compose-webrtc
make test-compose-relay
make test-compose-hybrid
```

Each standalone target includes the core Go checks. To run only the cross-container checks:

```sh
CONTROL_TEST_SUITE=e2e make test-compose-webrtc
```

The equivalent shell entry point is `sh tests/compose/test.sh`, with optional `webrtc` or `relay` arguments.

## Test topology

The [Compose file](../compose.yaml) defines five services:

| Service | Role |
| --- | --- |
| `gateway` | Directory, signaling, and opaque relay |
| `source` | Local API used by the test runner, workflow orchestration, source files |
| `worker` | FFmpeg encoding and remote execution |
| `consumer` | Receives artifacts from another node |
| `tests` | Go checks, CLI tests, HTTP fixtures, and result verification |

Every node has its own state and working-directory volumes. Artifacts cannot pass between machines through a shared filesystem. The network is internal to Docker, no host ports are published, and containers do not mount the Docker socket.

These are fixture names, not fixed runtime types. `source` takes the orchestrator role for the workflow and executes its local generation step as a worker. `consumer` is an enrolled node receiving artifacts. See [terminology](terminology.md).

For this isolated fixture, node APIs listen on all container interfaces so the test runner can reach them. The pool uses a fixed test-only token. This configuration is for testing; normal deployment keeps each local API on loopback.

Gateway and node health checks gate test startup. The [relay override](../compose.relay.yaml) switches every node to relay-only mode and changes the expected transport in the runner.

## What the container tests verify

Connection-test CLI checks measure orchestrator-to-worker and worker-to-worker paths over the required transport, using payloads larger than the control-frame limit. Core tests cover both transports, MCP routing, checksums, short reads/writes, option bounds, instruction authority, diagnostic grant cleanup, stream limits, revocation, and cancellation. Zero-duration clock samples are valid for local echoes; throughput remains finite when a transfer finishes within one clock tick. The multi-operation bulk fixture has a 90-second budget for race-enabled encryption and hashing on slower runners. These tests use synthetic traffic and no installed speed-test application. See [connection tests](connections.md).

- Three distinct enrolled identities, labels, online presence, and rejection of an invalid API token.
- Submission through the shipped CLI and a real FFmpeg workflow spanning the source and worker containers.
- Worker-to-consumer delivery, a partial CLI download resumed by offset, SHA-256 integrity, and encoded video dimensions checked with FFprobe.
- Absence of the encoded artifact in the orchestrator's artifact store.
- Multi-megabyte artifact delivery across all three nodes with byte-for-byte verification.
- HTTP requests and TCP tunnels opened by a remote node to a service on the internal network.
- Reverse forwarding from a worker's loopback port to a dev server bound only to the test runner's loopback. Core tests cover HTTP, WebSocket hot-reload traffic, TCP half-close, remote-port conflicts, MCP reverse start/stop, idle-listener cleanup, authorization, and fleet isolation over both peer transports.
- Actual peer transport selection for source/worker and worker/consumer connections.
- Dashboard snapshots and CLI rendering while another owner has running and queued tasks, a synchronous HTTP call is blocked, a transfer is in progress, and a TCP tunnel carries traffic.
- Direct gateway identity and resource metrics in dashboard output, plus owner-visible work counts and sampled CPU without a local observer. Core tests cover unavailable local nodes, fleet-scoped status, forged health sender IDs, report expiry, common-key restrictions, credential selection, and cached sampling without poll-triggered collection.
- Cached system capabilities and explicit resource refreshes. The core suite also checks periodic/completion-request collection, denied/offline machine states, metadata privacy, and stale-view behavior.
- A development bundle pushed to the gateway while a worker holds a task, lease, and reverse listener. The two idle nodes update independently. In relay mode the listener protects the gateway until closed; accepted local work and the lease then continue across the gateway restart without restarting their worker. After fresh online presence, explicit cancellation and lease release allow the worker to update. Identities, task state, and issued keys survive real process replacements. A fresh account client retains its owner and recovers its task afterward. The test never retries an execution request.
- Rejection of common-key update uploads and omission of administrative commands from common-key CLI help. Core tests also check release retrieval, corrupted binaries, persisted maintenance reservations, and key revocation.
- One-time enrollment using the target's automatic hostname, and Bash installer architecture selection with checksum rejection for Linux/macOS amd64 and arm64. The architecture tests simulate target detection; they do not execute foreign-platform binaries. Core tests cover architecture-bound redemption, pinned downloads across deployment changes, restart recovery, and wizard clipboard failures without duplicate invitations.
- Re-running generated installers on an existing node with the same name and a new name, including when an earlier attempt left pending enrollment, retaining one identity and restoring remote execution while rejecting old credentials. After unregistration, a new installer uses fresh identity state and restores execution through the same profile. Core tests cover replacement of expired pending attempts, preservation of pending replacement identities and profile settings, same-link credential recovery, and rejection of invalid replacements without losing recovery state.
- Enable/disable/unregister through the installed CLI: admission changes on the remote node, unregister revokes its credential, its actual supervisor exits, and its installed executable is removed while its profile remains. Core tests cover existing WebRTC/relay sessions, selector exclusion, accepted-task recovery and completion while disabled, independent update reservations, fleet isolation, signed acknowledgements, durable policy, online/offline unregistration without lifecycle support, inspection without lifecycle acknowledgement, retired identities, and dashboard action selection.

These tests live in [tests/compose/e2e_test.go](../tests/compose/e2e_test.go) and [tests/compose/updates_test.go](../tests/compose/updates_test.go) behind the `compose` build tag. The ordinary Go suite remains independent of Docker and FFmpeg. Its existing tests cover cancellation, leases, grants, MCP, identity ownership, gateway restarts, and task reconciliation.

Core update-cache tests count executable requests across separate User and System state directories for both startup selection and managed offers, sequentially and concurrently. Subprocess fixtures verify the filesystem lock across processes. Tests preserve GitHub-first delivery and gateway fallback, reject corrupt/truncated/symlink cache entries, verify private copies cannot be changed through the cache, bound retention, and cover cancellation, failed downloads, and unavailable-cache fallback. All cache paths are temporary; these tests do not install native services or require elevation. Actual cross-account Windows ACL and Unix startup behavior still require native smoke tests.

Core automatic-name tests cover CLI omitted/`auto` names, blank dashboard input, target-hostname validation, signed name binding, legacy proof compatibility, concurrent hostname claims, explicit reservations, persistence rollback, response recovery across restart, and rejection of foreign or retired identities.

Retirement tests run real process-mode nodes against a local gateway and verify uninstall after online unregistration, offline unregistration followed by startup, and startup with a persisted tombstone while the gateway is unavailable. They copy the already-built test executable and select the real CLI entry point through a test-only environment marker, including in supervisor and cleanup subprocesses. This avoids a nested cold-cache CLI build and keeps fixture execution race-enabled. A startup regression verifies that credential rejection after the first policy read still retrieves and persists retirement, that the saved policy stops a later offline startup, and that ordinary authentication rejection does not retire the node. Unit tests cover identity/policy validation, shutdown locks, repeatable cleanup, replacement-profile protection, shared-launcher preservation, and retained configuration/state/work files. These tests do not install native service-manager registrations. Linux transient-service cleanup, macOS one-shot launchd cleanup, and Windows service-SID delete permissions and scheduled-task removal require native runtime smoke tests.

Retirement fixtures select API ports in 20000..29999, separate from persistent-tunnel fixtures in 10000..19999. Both ranges are below the supported systems' default ephemeral ranges, so outbound sockets cannot claim a released port before the child binds it or while it restarts. The separate ranges prevent concurrent package tests from choosing the same released port. Startup failures include the child node log rather than only its temporary path. The service's 30-second readiness bound is unchanged.

Linux retirement-helper fixtures copy or reuse checksum-verified temporary helpers, then mock `chcon` and `systemd-run` to require executable labeling before a System launch. They cover permissive/disabled SELinux, missing tools, invalid mounts, labeling failures, cancellation, unchanged User/automatic user-manager launches, and exact helper arguments. They never execute a cleanup helper, change actual labels, or install or start a service. Actual SELinux transitions still require a separate approved Fedora runtime check.

Startup-context tests cover the wizard's user/system choice on all three platforms, back navigation, one-time creation, script elevation and explicit-context precedence, gateway persistence and invalid-mode rejection, and older-gateway confirmation failure. Unix tests verify saved system-mode reuse, root checks, profile-scoped service identifiers, systemd boot targets, and valid LaunchDaemon XML with escaped paths. These tests do not install system services. Linux systemd and macOS launchd startup, reboot/logout persistence, service permissions, and root-context provider behavior require native runtime smoke tests.

Hybrid tests cover the third wizard choice, paired ticket derivation, atomic reservation/retraction/revocation and rollback, protected installer generation and recovery branches, automatic-name suffix limits, preservation of unknown configuration fields and existing companion ports, Unix privileged-path ownership, and rejection of companion registrations without instruction-only System execution. Peer tests over WebRTC and relay verify exact account-client grants, rejection of raw account and independent worker execution, explicit User-to-System delegation, exclusion from automatic selection, temporary-grant cleanup, durable task ownership, log cancellation, and task cancellation. The generic peer fixtures simulate the authority boundary without installing privileged services. Windows-native tests parse both Hybrid PowerShell scripts. `make test-compose-hybrid` boots real systemd in a privileged container and runs the generated command as an unprivileged user with sudo. It covers a preflight rejected by a busy port with nothing installed, a failure after redemption fixed by rerunning the same command, the expected accounts and companion settings, `--linger`, a spent command after completion, and retirement removing the companion's binaries while keeping its data. launchd/SCM installation, Windows ACL inheritance, LocalSystem execution, UAC, real logout and reboot behavior still require native runtime smoke tests on disposable machines. Do not run privileged installation tests on a developer's existing fleet.

Hybrid timeout fixtures cover neither context redeemed, redemption without registration, either partial registered context, both successful registrations, the exact expiry boundary, credential revocation, directory removal, retained identity ownership, gateway disconnect, completion and cleanup transaction rollback, retry, and restart recovery. They also preserve pending records through invitation clearing, reject expired partial response recovery before the sweep, retain superseding enrollments, and keep completed contexts independently manageable. These local gateway fixtures do not install OS services.

Revoked Hybrid history fixtures cover pair and System-only revocation before redemption, after redemption, and after registration. Clearing hides revoked rows without deleting retirement records, is idempotent, preserves pending invitations, survives restart, and still retires installed halves at expiry. Failed history writes roll back visibility. Linux service fixtures simulate enabled, permissive, disabled, and invalid SELinux mounts, assert launcher-only `chcon` arguments, and fail on missing tools or labeling errors without changing host labels. Real SELinux domain transitions still require a Fedora runtime check.

Browser smoke checks create real Hybrid invitations in the isolated gateway, show one row per pair, revoke both contexts through that row, keep same-name attempts separate, count grouped history correctly, preserve pending pairs, and confirm cleared rows stay gone after reload. Cancelling Clear history changes no records.

Policy-read fixtures count startup requests, coalesce an already-received registration hint, retain a policy hint arriving while an older response is in flight, and verify continued two-second polling against a gateway without push support. They use local gateways and temporary node state, without installing services or contacting a live fleet.

Hybrid Bash fixtures run fresh installation and companion recovery with temporary paths and mocked elevation. They require one binary download for a fresh pair and none for a matching recovered companion, verify pinned-download fallback and checksum rejection for a different companion binary, and check that a rejected preflight or corrupt download stops before the System companion. CLI preflight tests cover accepted, conflicting, expired, and mismatched invitations, older gateways, and busy ports, and confirm that nothing is written or redeemed. Gateway tests cover downloads while a redeemed pair is incomplete and refusal after completion, non-consuming checks, proof purpose separation, and companion name conflicts. Windows-native fixtures exercise the generated copy/download/verification block with real temporary files and a mocked download, without enrollment or privilege changes. Enrollment test subprocesses use Windows PowerShell's built-in module directory rather than inheriting PowerShell 7's module paths from CI. A contaminated-module fixture verifies real `Get-FileHash` loading and preserves the parent environment.

Windows Hybrid privilege tests require both User phases to use the limited-token helper and check companion paths before executing recovery. Native PowerShell fixtures exercise fresh, existing and pending System branches, preflight/path/System/User failures and environment restoration through harmless stand-ins. A temporary-file ACL fixture verifies that staging copies bytes without importing an untrusted source's write ACL. Portable install-self tests supply a marker-writing executable that spoofs the current checksum and verify that it is rejected without execution or modification. Native token tests reject invalid queries, elevated or impersonation-token parents, different users/sessions, missing parent handles, and cancelled launches. Command inspection requires an explicit desktop parent with no token override. An actual limited-child launch verifies the child's user and elevation when the test runs as an elevated UAC split-token administrator with a desktop shell. UAC-disabled administrators skip that launch because they lack the required limited token; token-query, parent-selection and launch failures with the prerequisites present still fail. Production continues to reject unsafe desktop parents. Elevated-child/pending-enrollment rejection tests run only in an elevated test process. They never install services. Full protected staging, SCM/UAC, firewall, logout and reboot behavior still needs a disposable Windows smoke test.

Generated machine-installer fixtures also check local binary reuse for User, System, and Hybrid System enrollment. Bash fixtures simulate Linux/macOS; Windows-native fixtures run the PowerShell staging block. They count binary downloads for current-directory, `bin`, installed, default-installed, and PATH candidates, including platform-specific filenames and a valid candidate after a stale one. Missing or stale binaries download once; corrupt downloads fail before enrollment. These fixtures do not install services or change privileges.

Portable progress fixtures require each Windows machine/release download to suppress progress in a child PowerShell scope. Windows PowerShell fixtures mock the binary, manifest, and Hybrid companion-script requests, checking suppression during the request and preservation of several caller preferences after success or a thrown download failure. They also check response preservation and a single request without retries. No downloads or installation operations run in these fixtures.

Node startup tests use local gateways to verify recovery after transient policy, authentication, and WebSocket-upgrade failures, plus a real five-second policy-request timeout. They check cancellation during outages, policy refresh after a connection wait, blocked peer work admission until startup completes, durable disabled/retired policy after recovery, and fail-closed handling of authentication, malformed JSON, and local persistence errors. Transport tests cover transient/permanent error classification, TLS validation rejection, cancelled retry waits, and fail-fast command-scoped clients. These run in the core suite inside Compose and native CI; Windows reboot/login recovery still needs a runtime smoke test.

Windows enrollment tests parse generated scripts with the native Windows PowerShell parser and verify that direct PowerShell snippets execute a complete multi-line response through a mocked web request, including literal URL metacharacters. They also run the generated snippets in PowerShell against a local HTTP server returning a harmless multi-line script as plain text. These checks do not download binaries, enroll machines, or change services and privileges. Generated invitation commands no longer support direct invocation from CMD or Git Bash; open PowerShell first.

PowerShell helper tests replace an inherited, case-insensitive `PSModulePath` with the operating system's built-in module directory while preserving the caller's environment and execution policy. Native Windows coverage supplies a deliberately incompatible security module through the parent environment, then reads a temporary directory's ACL using the real `Get-Acl`. It never changes ACLs, services, or scheduled tasks. The inherited-environment test is portable; native module loading must run on Windows.

User login-task fixtures require root-folder-only enumeration, simulating access denial for a request that would inspect protected subfolders. They preserve missing-task success, exact name/path filtering, profile/user ownership checks, and real scheduler-error propagation. Native PowerShell error fixtures also require the failing cmdlet name in diagnostics. The real-scheduler read-only fixture checks a missing temporary profile without modifying tasks. These checks do not establish that local policy permits task registration or start; that still requires the opt-in Windows user-startup lifecycle test.

Portable Hybrid phase tests require protected task preparation exactly once before limited enrollment, no preparation during preflight, cancellation before mutation/launch, and failure propagation without retries. Windows-native removal fixtures mock both task enumeration and unregister: only the matching profile/user task may be removed, a missing task is harmless, and foreign ownership, lookup denial, or removal denial cannot trigger enrollment or a retry. They never change real task registrations or ACLs. Actual deletion of a legacy administrator-owned task by the protected parent still requires a disposable Windows Hybrid smoke test.

Rename tests cover dashboard prefill, editing, paste, cancellation, validation, single submission, CLI name/ID resolution, fleet authorization, invitation/client reservations, persistence rollback, gateway restart and old-config reconnection, signed enrollment recovery, and replacement enrollment. Self-contained peer tests over WebRTC and relay verify new and reused aliases without interrupting an active TCP stream.

Force-invitation tests cover explicit CLI flags, dashboard confirmation and cancellation, no force retry after uncertain errors, atomic retraction with rename or new invitation, persistence rollback, old URL/download/redemption rejection across restart, fleet isolation, and existing-identity replacement bindings. Force rename uses the same project-local scenario migration as ordinary rename.

Runbook tests cover approved OS paths, shared and machine-specific discovery, stable-ID/alias matching, source-qualified complete reads, explicit conflicts and preference, aggregate inline budgets, source failures, invalid paths/text/JSON, symlinks, and cancellation. CLI/MCP fixtures verify node-free local discovery, combined descriptions, unsupported/denied workers, and no fallback or replay. Peer tests over WebRTC and relay verify worker protocol advertisement without content, authorized reads, self-only bindings, traversal rejection, and denial to discovery-only or undelegated worker callers. Rename tests cover CLI name/ID requests, the shared dashboard method, nested Markdown/JSON routing edits, unchanged secret names and unrelated files, missing directories, conflicts, source validation, gateway failures, and concurrent edits after acknowledgment. Fixtures use temporary stores rather than real user runbooks or the user's fleet. See [limits and recovery](runbooks.md).

Applicability tests parse YAML frontmatter and JSON metadata, preserve arbitrary nested VPN/vendor, OS/app/GPU/capability, and custom-service conditions across omission and complete reads, retain exact numeric conditions across worker/client JSON hops, reject invalid values, aliases, merge keys and size/structure limits, and retain healthy stores after metadata errors. Client/MCP/peer fixtures retain structured requirements without interpreting folder names as OS requirements. Requirements and examples do not execute FFmpeg, inventory apps/VPN state, or certify GPU encoder support.

Native CLI self-update tests use a local release server and a real running executable to check replacement, pinned downloads, checksum and version rejection, cancellation, and repeated updates. Windows CI exercises replacement while the previous executable is running. These tests require no published release or gateway credential.

Same-version tests verify that CLI updates skip binary downloads and preserve the installed executable despite different checksums. Managed-update tests cover unchanged deployment publication during staging, installation, and completion, node download/restart skipping, persisted reservations, and completion of an already-current busy fleet without maintenance admission. Binary checksum validation and fresh idle reservations for actual restarts remain strict.

Update-display tests verify disconnect grace across node and gateway restarts, early durable grace removal after confirmed resumption, restored idle/busy health, continued update display while maintenance is paused, and rejection of stale or mismatched deployment progress for display.

Independent rollout tests allow explicitly compatible nodes before the gateway, preserve gateway-first behavior for undeclared builds, reject incompatible declarations, bound reservations/commits, retain busy peers, and persist intent before dispatch. Restart and persistence-failure fixtures require fresh applied/resumed confirmation rather than stale or paused status. Publication tests allow safe supersession but reject unresolved commits, gateway switching, and legacy installing records. Relay tests cover pending setups, two-sided closure, connection generations, legacy blockers, bounded timeout/backoff, HTTP download protection, and atomic relay-open/restart races. Transport fixtures preserve a live standalone-client stream and direct carriers while retiring cached idle relays and restoring admission on resume. Deferral tests retain their two-minute boundary and durable pending identities. Startup tests retain old-gateway HTTP 404, manifest/ID/platform validation, authentication/redirect rejection, and staging without automatic commit. Compose exercises the independent rollout over both transports through real process replacements.

Managed-download fixtures prefer an exact signed release, reject version/size/checksum mismatches before executable download, and verify gateway fallback for missing metadata, signature failures, absent/corrupt public binaries, and GitHub errors. They check staged-file reuse, cancellation without fallback, failure of both sources, and absence of fleet credentials on every public request. Gateway fallback refuses redirects. Fixtures use local HTTP servers and an injected signature verifier; production uses the existing Sigstore verifier covered separately by virtual-authority tests.

Trust-metadata tests simulate CDN location rejection, transient failures, cancellation, response-size bounds, and an official-mirror-only URL policy. An in-memory signed TUF repository verifies successful mirror refresh while rejecting untrusted root rotation, modified targets, and expired timestamps. A missing next root from the mirror remains HTTP 404 so root discovery terminates normally. These tests need no public Sigstore service.

Gateway retention tests cover cleanup after successful completion and restart, preservation of selected assets for offline nodes, current/previous/requested runtimes, live architecture-pinned invitations, active release pins, and recent unpublished uploads. They reject pruning unfinished or superseded deployments, cancel cleanup safely, fail closed on corrupt runtime records, confine symlink removal, and verify that general downloads exclude older binaries while valid installation links still download their pinned assets.

Development-update tests verify private credential storage independent of the dashboard login, operator-login persistence across later fleet logins, repeated login without key entry, reuse without environment variables, gateway scoping, explicit overrides, stdin authorization, rejected keys, and cancellation without credential changes. Check-only authorization cannot prompt, read a key, or save credentials. POSIX Make tests verify that this check precedes bundling and publishing even under `make -j`, and that failed authorization stops before platform builds.

The shared-carrier test also fails if any WebRTC lane falls back to the relay, which a dropped first message on an added data channel causes. Efficiency tests count carrier packets through pinned TLS and yamux for bulk streams, duplex records, and control frames, and negotiate 64 KiB packets over WebRTC and the relay with a 16 KiB fallback for an older gateway. Lookup tests verify reuse, eviction on offline notifications, and that offline machines are never served from cache. Gateway tests reload the exact durable state after a sequence of incremental commits and deletions, accept signed registration bytes with unknown fields while rejecting modified bytes, ignore unknown health fields without retaining them, and close only an overflowing relay session while its receiver stays connected. Node tests cover blocking task waits and their deadline margin, one execution for concurrent duplicate submissions, lease refusal during a reserved acceptance, retention by age and count with tombstones that survive restart and prevent re-execution, artifact owner holds, corrupted resumed prefixes, and bounded parallel workflow steps that stop scheduling after a failure.

Offline-ordering tests hold the gateway's database connection to delay disconnect persistence, verifying that notification waits for offline observation publication. Lookup tests also inject a notification before an older response arrives and reject cache repopulation. Worker-restart fixtures wait for gateway offline presence before reconnecting the same identity; local shutdown alone does not acknowledge remote disconnect.

Short-lived artifact-grant tests establish the download lane before issuing the grant so cold transport setup cannot consume its three-second lifetime. Expiry still cancels the backing access and rejects subsequent token and access-ID reads.

Shared client tests verify that binary request bytes reach the receiver before the producer completes the body, that a 16 MiB binary uses `application/octet-stream` with its exact length and checksum, and that the JSON manifest is published only after successful uploads. HTTP 413 must stop publication without automatically retrying the upload.

The Dockerfile downloads Go dependencies while building the image. The running test network does not need internet access. The initial image build does need access to container registries, Go modules, and Debian package repositories.

## Performance benchmarks

Gateway admission tests cover atomic global/account/class reservations, pre-upgrade handshake rejection and recovery, configurable positive limits, new-identity rate refusal before persistence, known-identity reconnects, and operator-only diagnostics. Packet-budget tests include shared-account and cross-account pressure, control headroom, spare data capacity, non-data and unknown protobuf bytes, queue rejection rollback, failed writes and concurrent disconnect cleanup. The failed-write test closes the WebSocket synchronously before writing; a canceled context alone can race with a ready write and is not a deterministic failure trigger. It checks that destination, account and global reservations remain held until the write and are released on failure. Existing relay tests retain ordering, offline barriers, fairness, overflow isolation and legal 4 MiB bulk-window coverage.

Rename-notification regressions invoke the authenticated rename handler with controlled destination queues. They verify fleet-scoped fan-out, exact global/account/destination reservations for `directory.changed`, no relay-payload charge, reservation retention during writes, successful/failed write release, repeated cleanup, and restart admission only after all reservations drain. Full, closed, and byte-budget-exhausted queues preserve accounting and the existing notification-failure disconnect policy. Local WebSocket fixtures supply write outcomes; no live fleet is contacted. The restart fence also rejects an invalid negative packet count rather than treating it as idle.

`BenchmarkStatusWakeCandidates` compares repeated per-target candidate sorting with one shared candidate order at 100 and 1,000 registrations and 32 offline targets. Selection regressions verify case-sensitive name ordering and fleet/LAN checks without changing display order. Run it with `go test ./internal/gateway -run '^$' -bench '^BenchmarkStatusWakeCandidates$' -benchmem`. This isolates candidate preparation, not end-to-end user capacity. Future sizing still requires isolated multi-account fleets with real node policy/health traffic, reconnect storms, bulk relay, slow consumers and a long soak. Do not drive a live gateway to failure.

An October 31, 2026 macOS arm64 run on an Apple M4 Pro used a 300 ms benchmark duration and three repetitions. At 100 registrations, repeated preparation allocated approximately 1.57 MB and 32 objects per batch of 32 targets, versus 49 KB and one object with a shared order. At 1,000 registrations, the corresponding allocation was approximately 13.89 MB versus 434 KB. Concurrent build/test work made timing results noisy, so these allocation measurements do not establish latency or gateway-capacity improvements.

### Native test runtime

`make test` runs the complete native suite, including local gateways, nodes, and peer transports. Independent long-running fixtures use `t.Parallel()` with their own temporary state, credentials, and listeners. Lifecycle, artifact-expiry, persistent-tunnel, and ICE compatibility cases also run their isolated subtests concurrently. Process-global environment and working-directory changes must stay in serial tests. Call `t.Parallel()` before creating fixture contexts so queueing does not consume their deadlines, and keep shared parent resources alive with `t.Cleanup` if adding parallel children.

The in-memory carrier-retirement test uses `testing/synctest` to exercise the production timer just before and at its deadline without a wall-clock wait. Socket-based heartbeat, startup-timeout, and signed-grant tests still exercise real timers. Production intervals and test coverage are unchanged.

The gateway-restart and simulated network-switch fixtures pin WebRTC to loopback and use stable network samples, keeping host VPN and interface changes out of their recovery scenarios. The restart fixture asserts the expected transport before, during, and after the outage; a relay fallback cannot satisfy the direct-session continuity check. After the gateway returns, it waits for both peers to finish reconnecting as well as for online directory entries.

An October 20, 2026 comparison on macOS arm64 with Go 1.27.3 used `go test -json ./... -count=1 -timeout=180s`. All 367 top-level tests passed in the baseline and final runs. Package execution times were:

| Package | Before | After |
| --- | ---: | ---: |
| `internal/node` | 80.4 s | 30.9 s |
| `internal/client` | 35.0 s | 13.4 s |
| `internal/transport` | 25.2 s | 8.2 s |
| `internal/gateway` | 24.9 s | 24.5 s |

The interval from the first package start to the last result fell from 80.4 to 31.0 seconds, about 61%. This excludes initial compilation; it is a local comparison, not a CI runtime guarantee. The gateway fixture was not changed in this optimization and still includes a 15-second application-heartbeat expiry test.

Go bounds parallel tests per package with `-parallel`, which defaults to `GOMAXPROCS`. On resource-constrained hosts, use `go test ./... -parallel=4 -timeout=180s`; `-p=1` also serializes packages. For a serial diagnostic run, use `-parallel=1 -p=1`. These flags preserve coverage. `-short` does not currently select a smaller internal suite.

Native CI race tests allow five minutes per package, including serial fixtures and time spent waiting for parallel-test slots. The Intel macOS node suite exceeded the previous three-minute budget while individual tests were still progressing. Runbook peer-discovery transport cases use independent clusters and run in parallel; individual operation deadlines remain unchanged.

Standalone peer fixtures select account-client routing explicitly rather than relying on an ephemeral port remaining unused after its listener closes. The concurrent-owner recovery fixture keeps a rejecting local API open and asserts that none of its account clients probe it, including after reconnect. Worker-runbook API failure coverage uses an owned HTTP 503 server. Authentication-error tests still verify that local failures never trigger fallback or replay.

### Throughput and allocation benchmarks

`BenchmarkTaskLogPayload` measures snapshot and streaming reads of a 64 KiB log chunk with empty or large retained task payloads. On October 29, 2026, macOS arm64, Apple M4 Pro and Go 1.27.3, three one-second runs with 2 MiB each of arguments and inline result allocated about 4.42 MB per snapshot read and 4.35 MB per streaming read before removing task payload clones. Afterwards they allocated about 205 KB and 139 KB, respectively. These synthetic legacy-sized records exceed current new-task inline limits. The benchmark isolates local log reading, not end-to-end transport throughput or process peak memory. Full task reads still copy payloads; log reads retain owner checks and completion notifications.

```sh
go test ./internal/node -run '^$' -bench '^BenchmarkTaskLogPayload$' -benchmem -benchtime=1s -count=3
```

MCP regression tests use a local HTTP server to verify successful tool results, bounded error diagnostics, preservation of failed tool output and refusal to start dependent workflow tasks after `isError`. Reconciliation tests verify concurrency bounds, cancellation, plan ordering, timeout isolation, missing stable IDs and structured error limits without sending mutations.

`TestMCPWorkflowRecoveryEndToEnd` runs both MCP adapters through account-client and delegated peer connections over WebRTC and relay. It verifies synchronous error codes, failed tool results above the inline limit downloaded as artifacts, dependent-step refusal, idempotent parent submission, log offsets and owner checks, and fresh-client reconciliation of a twelve-child parallel workflow. The cross-container FFmpeg test also invokes the shipped CLI to reconcile persisted children and drain/resume completed workflow logs.

Reliability tests cover corrupt interior tombstones without journal rewriting, cleanup retry across restart, bounded owner-scoped pages, artifact-backed results, quota accounting, reader pins, independent MCP startup, relay fairness and close/offline ordering, cache eviction with retained identity ownership, fresh resource constraints, and read-only workflow reconciliation. Signature tests use a local virtual Sigstore authority to check exact repository/tag identity and artifact bytes. Update-path tests also verify that invalid signatures stop before executable downloads. They need no live signing service.

Measure local observation and gateway commit contention separately from integration flows:

```sh
go test ./internal/node -run '^$' -bench '^BenchmarkActivitySnapshot$' -benchmem -count=3
go test ./internal/gateway -run '^$' -bench '^BenchmarkGatewayCommitWithDirectoryReads$' -benchtime=3s -count=3
```

The activity benchmark has one active task and 100, 10,000, or 100,000 retained terminal tasks, requesting five recent records. The largest fixture exceeds the default retention limit to check scaling when retention is changed. It does not collect resources or use the network. On a local macOS arm64 Apple M4 Pro, before the bounded recent-task index, 10,000 retained tasks took 2.8 to 3.3 milliseconds and allocated about 11.7 MB per snapshot. Afterwards, the same fixture took about 0.5 microseconds and allocated 1,848 bytes. These are in-process measurements, not pool-wide latency or throughput claims.

The gateway benchmark changes one node record under the gateway mutex with SQLite WAL and `synchronous=FULL`, while another goroutine handles directory reads for 100 or 1,000 machines. It reports commits/second, reads/second, and directory p99 latency from a bounded ring of the latest 4,096 reads. HTTP response encoding is included; network I/O, connection proofs, and resource sampling are excluded. Set `CONTROL_BENCH_DIR` to an existing filesystem directory to compare disk with a Linux tmpfs mount. Each run creates and removes only its own benchmark subdirectory. The default uses the Go test temporary directory.

The following measurements reviewed the performance changes now recorded in [D073](decisions.md#d073-committed-observation-views-and-safe-performance-boundaries) on October 13, 2026, using Go 1.27.2 on macOS arm64, Apple M4 Pro, default `GOMAXPROCS=14`, and the test temporary directory on the local filesystem. Each unprofiled benchmark ran for one second, three times. Figures below are medians, not confidence intervals. "Before" means the working-tree implementation at the start of that review, not a previous release.

A separate October 20 check compared detached WebRTC reads with the previous callback implementation using a temporary Go overlay, on the same machine and toolchain. Across three one-second runs per variant, median allocation per 16 MiB download fell from 266.4 MB to 244.7 MB, about 8%. Median throughput was 81.9 versus 81.0 MB/s; this does not establish a speed increase. Current relay runs after adding session scheduling measured 504–560 MB/s and 46.2–47.0 MB allocated per download. These are aggregate allocation and loopback measurements, not peak memory or WAN predictions. An initial 2 MiB session queue was rejected because it could overflow a valid 4 MiB bulk window; the 6 MiB limit has a regression test including TLS framing.

| Measurement | Before | After |
| --- | --- | --- |
| Directory p99 during commits, 100 machines | 1.57 ms | 0.17 ms |
| Directory p99 during commits, 1,000 machines | 3.95 ms | 0.95 ms |
| Directory reads/second during commits, 1,000 machines | 447 | 1,774 |
| Commits/second with directory reads, 1,000 machines | 15,922 | 18,145 |
| Relay artifact download, 16 MiB | 536 MB/s | 679 MB/s |
| Allocated bytes per relay artifact download | 161 MB | 46 MB |
| Allocations per relay artifact download | 96,806 | 35,290 |

The directory change separates the committed observation view from mutation/fsync ordering and maintains sorted fleet membership. Mutation validation and SQLite commits still serialize, and `synchronous=FULL` remains enabled. CPU/mutex/block profiles of the initial 1,000-machine benchmark attributed 94% of mutex delay to the commit-holder benchmark goroutine. Allocation counts in this concurrent benchmark include reader work normalized by writer iterations, so they are not per-commit allocation costs.

`BenchmarkFleetDashboard` starts real in-process gateways, 16 or 64 enrolled nodes, and an authenticated standalone client over the relay. It includes HTTP status, bounded peer RPC fan-out, pinned TLS, decoding, and merging. With 64 machines, a warmed uncached poll took 6.17 ms and allocated 7.38 MB; a cache-hit poll took 0.70 ms and allocated 0.52 MB. These are cache-hit costs, not sustained five-second averages. Automatic polls reuse peer detail for up to five seconds while still reading current gateway status; explicit refresh and direct activity calls bypass reuse. Startup, sampling, and connection warm-up are outside the timer. It verifies every requested peer returned detail, rather than timing an unavailable fallback.

`BenchmarkArtifactDownload` exports a real 16 MiB immutable artifact, downloads it into a checksum writer, and verifies SHA-256 and actual transport. It measures disk reads, TLS/yamux, WebRTC or relay, and receiver hashing; export, startup, and warm-up are outside the timer. The initial combined artifact allocation profile attributed 43% of allocated space to growing `io.ReadAll` buffers. Reusable bounded WebSocket readers and gateway protobuf output buffers reduced relay allocation and improved throughput as shown above. Direct WebRTC remained about 92 MB/s with roughly 264 MB allocated per download; this change does not tune Pion buffering or crypto. Same-process loopback rates are not internet bandwidth predictions, and aggregate allocations are not peak retained memory.

Paired adapter benchmarks isolate the relay changes from that review. At 16 KiB, pooled marshal took 0.27 microseconds with zero allocations after warm-up, versus 1.29 microseconds and one 18,432-byte allocation without reuse. Ownership-based receive took 2.00 microseconds versus 2.11 microseconds with a pooled payload copy, about 5% less time in this run, not the earlier inferred 27%. Both receive paths still allocate decoded protobuf payloads. The activity snapshot remained about 0.5 microseconds at 100,000 retained tasks, independent of history size.

`BenchmarkMetadataWrite` compares compact and indented task JSON with the same atomic replacement and file/directory sync. Compact encoding allocated about 7.4 KiB rather than 12.3 KiB, but both took roughly 11 to 13 ms on this filesystem. Encoding is not the durable-write bottleneck here. File sync and directory sync protect different recovery steps; neither was removed. Executable verification also retains byte-for-byte hashing, since size/mtime caches can accept corrupted content with preserved metadata.

Run the measurements:

```sh
go test ./internal/gateway ./internal/node ./internal/transport ./internal/store ./internal/client -run '^$' -bench 'Benchmark(GatewayCommitWithDirectoryReads|ActivitySnapshot|PacketConnTransfer|RelayDataPacket|RelayMarshal|DuplexTransfer|MetadataWrite|FleetDashboard|ArtifactDownload)$' -benchmem -benchtime=1s -count=3
```

Capture profiles one package at a time, without running other builds/tests concurrently. Use an existing output directory outside the checkout, replacing `/path/to/profiles`:

```sh
go test ./internal/gateway -run '^$' -bench '^BenchmarkGatewayCommitWithDirectoryReads/nodes=1000$' -benchtime=3s -o /path/to/profiles/gateway.test -cpuprofile=/path/to/profiles/gateway.cpu -mutexprofile=/path/to/profiles/gateway.mutex -blockprofile=/path/to/profiles/gateway.block
go test ./internal/client -run '^$' -bench '^BenchmarkArtifactDownload$' -benchtime=3s -o /path/to/profiles/client.test -cpuprofile=/path/to/profiles/artifact.cpu -memprofile=/path/to/profiles/artifact.mem -mutexprofile=/path/to/profiles/artifact.mutex -blockprofile=/path/to/profiles/artifact.block
go tool pprof -top /path/to/profiles/gateway.mutex
go tool pprof -top -alloc_space /path/to/profiles/artifact.mem
```

These fixtures do not establish WAN latency, concurrent enrollment throughput, multi-observer fleet polling bandwidth, control latency behind a saturated socket, or production storage limits. Priority queues cannot preempt an in-progress WebSocket write. Encrypted RPC traffic still shares the relay data queue with bulk traffic; priority applies to gateway signaling and control packets, not per-stream bandwidth guarantees. Durable fleet mutations remain serialized. Measure those separately on the deployment filesystem and both transports before introducing group commit, push observation, or transport tuning. Adapter framing and compatibility tests remain documented in [fleet streaming](streaming.md#verification-and-measurements).

## Keep a deployment for debugging

To inspect a failed environment, use Compose directly instead of the cleanup script:

```sh
docker compose up --build --abort-on-container-exit --exit-code-from tests tests
docker compose logs worker gateway
```

The first command stops the services when the test runner exits and returns its exit code, but retains containers and volumes. Restart the nodes for inspection:

```sh
docker compose up -d source worker consumer
docker compose exec source control machines
docker compose exec source control call worker node.describe
```

For relay-only testing, use the same override on each Compose command:

```sh
docker compose -f compose.yaml -f compose.relay.yaml up --build --abort-on-container-exit --exit-code-from tests tests
```

Remove the test deployment and its data when finished:

```sh
docker compose down --volumes --remove-orphans
```

Start with fresh volumes when switching transport modes. The automated Make target does this by using separate project names and cleaning up each mode.

## CI and platform coverage

The [CI workflow](../.github/workflows/ci.yml) runs on pushes and pull requests to `main`. It cancels superseded runs, pins actions to commit SHAs, and reads the Go toolchain version from `go.mod`.

Runner labels are pinned to `ubuntu-24.04`, `macos-15-intel`, and `windows-2025` to avoid automatic OS migrations. macOS uses Intel runners to avoid ARM64 capacity queues. The [release workflow](../.github/workflows/release.yml) also uses `ubuntu-24.04`.

Separate jobs cover:

- Dependency verification, race tests, and native executable builds on Windows, macOS, and Linux.
- Formatting, vet, and golangci-lint.
- All six CGO-free release binaries and manifest/checksum verification.
- Both Compose transport modes.
- govulncheck, also run by itself every Monday at 06:00 UTC.

Linux containers on Docker Desktop do not verify Windows process handling or macOS filesystem behavior, so container testing complements the native matrix. The Compose image uses Go 1.27.3. Keep its version synchronized with `go.mod` when updating Go.

## Local development commands

Windows-native tests cover SCM stop/shutdown cancellation, failure reporting, and console-free background children. GUI-bootstrap tests start a derived PE GUI image without hidden-window creation flags and require `GetConsoleWindow` to return zero. Bootstrap child tests check `CREATE_NO_WINDOW`, supervisor exit/failure propagation, path quoting, and retained log output. Portable tests verify PE32/PE32+ subsystem conversion, unchanged original CLI bytes, checksummed reuse and repair, distinct copies across CLI changes, and source-size bounds. Login-task tests also read the real scheduler to verify that an absent task is a successful no-op, and use PowerShell fixtures to verify exact task filtering, ownership checks, and readable errors without CLIXML progress output. These checks need no elevation and do not create or remove tasks. To exercise an actual service, run from Administrator PowerShell:

```powershell
$env:CONTROL_TEST_WINDOWS_SERVICE = '1'
go test ./cmd/control -run '^TestWindowsServiceLifecycle$' -timeout 180s
```

This opt-in test builds a temporary executable, migrates a live detached node to SCM, verifies automatic startup settings and LocalService permissions, kills the supervisor to check SCM recovery and identity preservation, then stops, restarts, and removes the service. It uses a local gateway and paths containing spaces. Boot and logout persistence still need a Windows machine smoke test.

To test user-login startup in a logged-in Windows session, without an account password:

```powershell
$env:CONTROL_TEST_WINDOWS_USER_STARTUP = '1'
go test ./cmd/control -run '^TestWindowsUserStartupLifecycle$' -timeout 180s
```

This creates a temporary login task, checks execution under the installing user's SID, and verifies saved-mode stop/start, repeated start, and identity preservation. It removes the task and stops the node on completion. Pure Go tests cover launcher encoding, path quoting, task ownership guards, profile scope, and limited interactive-token registration. Cross-compilation does not verify Task Scheduler permissions, console visibility, login/logout behavior, or SCM migration; verify those on a Windows machine before deployment.

`make` and `make help` list available targets. Make recipes use a POSIX shell; Windows developers can use Git Bash with Make or run the corresponding Go commands directly.

Firewall tests verify profile-scoped rule names, path quoting, gateway-port validation, and the absence of inbound TCP rules. Windows-native tests execute generated PowerShell with mocked firewall cmdlets to check creation, repeated refresh, and refusal to change unrelated rules, without elevation or firewall mutations. Native runtime tests verify stable launch paths across updates, checksum validation, preserved versioned sources, and preservation of the old copy on failed publication. Real UAC approval/cancellation, firewall policy, and direct WebRTC after a managed update require a Windows smoke test. Run `service firewall` against an actual node profile only when those firewall changes are intended.

| Target | Purpose |
| --- | --- |
| `deps`, `tidy` | Download/tidy dependencies, or just tidy module files |
| `deps-update`, `deps-check` | Update current-major dependencies, or list available versions |
| `go-update` | Update the Go requirement to the latest stable version |
| `fmt`, `fmt-check` | Apply formatting or fail on unformatted Go files |
| `lint` | Run pinned golangci-lint through `go run` |
| `lint-hint`, `lint-hint-all` | Run installed gopls hints on changed or all Go files |
| `test`, `test-race`, `check` | Native tests, race tests, or vet plus race tests |
| `cov`, `test-coverage` | Generate `tmp/coverage.out` and `tmp/coverage.html` |
| `test-cov-check` | Regenerate coverage and enforce `COVERAGE_MIN`, default 50% |
| `bench` | Run available Go benchmarks without ordinary tests |
| `build-all`, `bundle` | Build release binaries for all six platforms |
| `release-local` | Build a bundle and check its manifest, files, and checksums |
| `release-check` | Validate the existing bundle without rebuilding it |
| `vuln` | Run pinned govulncheck |
| `ci`, `ci-compose` | Run local CI checks, optionally followed by both Compose modes |
| `clean` | Remove build, bundle, and coverage output |

`GO`, `BIN_DIR`, `DIST_DIR`, `COVER_PROFILE`, and `COVER_HTML` can override the local tool/output paths. `PLATFORMS` narrows a local bundle. `make check` retains the existing vet and race-enabled checks.
