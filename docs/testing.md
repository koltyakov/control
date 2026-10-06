# Testing with Docker Compose

[Contributor guide](../AGENTS.md) · [Architecture](architecture.md) · [Engineering principles](principles.md) · [Design decisions](decisions.md)

Use Docker Compose for repeatable integration testing. Docker supplies Go, FFmpeg, and the node runtime, so the host only needs Docker Engine or Docker Desktop with Compose v2 or newer. The Make targets also need Make and a POSIX shell.

## Run the tests

The suite also runs generated Bash installers in the test container, verifies online registration and remote execution, rejects reuse and common-key invitation creation, and checks login followed by host setup without credential environment variables, plus MCP/skill installation. Unit tests cover saved login reuse, private credential files, failed-login preservation, concurrent redemption, expiry, revocation, name reservations, and configuration preservation. These Linux container tests do not exercise native launchd or Windows SCM startup.

Private-fleet tests create two user accounts with duplicate machine names, multiple orchestrator nodes, and a worker installed through a user-owned invitation. They verify saved account credentials, same-fleet execution over the required transport, and rejection of cross-user discovery, execution, tasks, artifacts, observation, and invitation revocation. Native tests also send forged signaling/relay packets directly to the gateway, check receiver membership and cross-user artifact grants, reject identity reassignment, and verify transactional JSON-to-SQLite migration and account persistence.

Standalone client tests exercise named calls, owner-scoped task recovery across invocations, resumed artifact downloads, and cancellable TCP tunnels over both WebRTC and relay. Concurrent clients use independent TLS identities while sharing task submission, cancellation, logs, and lease ownership. Closing the submitting client shuts down its listener and sockets without cancelling durable work. They check account and independent-owner isolation, worker authorization, exclusion of clients from machine discovery and the dashboard, and refusal to switch backends after local authentication or application failures. Gateway tests cover role-bound and owner proofs, forged metadata, immutable roles and bindings, client credential revocation, transient routing metadata, exclusion from managed updates, and database migration of legacy CLI registrations without losing ownership. Negotiation tests reject older gateways and workers before connecting or sending work. CLI tests execute a subprocess with only a saved login, exercise detached submission, timeout selection and followed logs, and verify that explicit API settings disable fallback. MCP tests cover session inspection and start/list/stop forwarding through the local API, socket cleanup, listener limits, and closed-client rejection. These tests run in the core suite inside Compose as well as native CI.

Streaming tests also verify concurrent traffic lanes, one WebRTC carrier across lane channels, control traffic during blocked bulk writes, sibling multiplexer closure, independently cancelled slow-peer setup, stream-capacity cancellation, receive-packet bounds and buffer ownership, changing write deadlines, TCP EOF-driven responses through standalone and local API forwards, malformed duplex records, legacy raw/polling fallback, and binary log offsets with cancellation and owner checks. Adapter microbenchmarks and measured allocation changes are documented in [fleet streaming](streaming.md#verification-and-measurements).

RPA tests use a fake helper to verify opt-in, whole-batch validation, authorization, leases, durable task idempotency, per-user serialization across profiles, cancellation, partial failures, screenshot path confinement, and PNG artifacts. Peer tests cover both transports without GUI dependencies. Dependency-free Python tests cover exact selector uniqueness, traversal bounds, backend validation, Wayland rejection, and key/button cleanup. Run them with `python3 -m unittest discover -s examples/rpa -p 'test_*.py'`. Live desktop behavior and permissions require native smoke tests; see [GUI automation](rpa.md#verification-and-limits).

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

- Three distinct enrolled identities, labels, online presence, and rejection of an invalid API token.
- Submission through the shipped CLI and a real FFmpeg workflow spanning the source and worker containers.
- Worker-to-consumer delivery, a partial CLI download resumed by offset, SHA-256 integrity, and encoded video dimensions checked with FFprobe.
- Absence of the encoded artifact in the orchestrator's artifact store.
- Multi-megabyte artifact delivery across all three nodes with byte-for-byte verification.
- HTTP requests and TCP tunnels opened by a remote node to a service on the internal network.
- Actual peer transport selection for source/worker and worker/consumer connections.
- Dashboard snapshots and CLI rendering while another owner has running and queued tasks, a synchronous HTTP call is blocked, a transfer is in progress, and a TCP tunnel carries traffic.
- Direct gateway identity and resource metrics in dashboard output, plus owner-visible work counts and sampled CPU without a local observer. Core tests cover unavailable local nodes, fleet-scoped status, forged health sender IDs, report expiry, common-key restrictions, credential selection, and cached sampling without poll-triggered collection.
- Cached system capabilities and explicit resource refreshes. The core suite also checks periodic/completion-request collection, denied/offline machine states, metadata privacy, and stale-view behavior.
- A development bundle pushed to the gateway, deferred by a running task and lease, then applied by real process restarts on all three nodes and the gateway. Identities, task state, and issued keys survive the rollout.
- Rejection of common-key update uploads and omission of administrative commands from common-key CLI help. Core tests also check release retrieval, corrupted binaries, persisted maintenance reservations, and key revocation.
- One-time enrollment using the target's automatic hostname, and Bash installer architecture selection with checksum rejection for Linux/macOS amd64 and arm64. The architecture tests simulate target detection; they do not execute foreign-platform binaries. Core tests cover architecture-bound redemption, pinned downloads across deployment changes, restart recovery, and wizard clipboard failures without duplicate invitations.
- Re-running generated installers on an existing node with the same name and a new name, including when an earlier attempt left pending enrollment, retaining one identity and restoring remote execution while rejecting old credentials. After unregistration, a new installer uses fresh identity state and restores execution through the same profile. Core tests cover replacement of expired pending attempts, preservation of pending replacement identities and profile settings, same-link credential recovery, and rejection of invalid replacements without losing recovery state.
- Enable/disable/unregister through the installed CLI: admission changes on the remote node, unregister revokes its credential, and its actual supervisor exits. Core tests cover existing WebRTC/relay sessions, selector exclusion, accepted-task recovery and completion while disabled, independent update reservations, fleet isolation, signed acknowledgements, durable policy, online/offline unregistration without lifecycle support, inspection without lifecycle acknowledgement, retired identities, and dashboard action selection.

These tests live in [tests/compose/e2e_test.go](../tests/compose/e2e_test.go) and [tests/compose/updates_test.go](../tests/compose/updates_test.go) behind the `compose` build tag. The ordinary Go suite remains independent of Docker and FFmpeg. Its existing tests cover cancellation, leases, grants, MCP, identity ownership, gateway restarts, and task reconciliation.

Core automatic-name tests cover CLI omitted/`auto` names, blank dashboard input, target-hostname validation, signed name binding, legacy proof compatibility, concurrent hostname claims, explicit reservations, persistence rollback, response recovery across restart, and rejection of foreign or retired identities.

Native CLI self-update tests use a local release server and a real running executable to check replacement, pinned downloads, checksum and version rejection, cancellation, and repeated updates. Windows CI exercises replacement while the previous executable is running. These tests require no published release or gateway credential.

Development-update tests verify private credential storage independent of the dashboard login, operator-login persistence across later fleet logins, repeated login without key entry, reuse without environment variables, gateway scoping, explicit overrides, stdin authorization, rejected keys, and cancellation without credential changes. Check-only authorization cannot prompt, read a key, or save credentials. POSIX Make tests verify that this check precedes bundling and publishing even under `make -j`, and that failed authorization stops before platform builds.

Shared client tests verify that binary request bytes reach the receiver before the producer completes the body, that a 16 MiB binary uses `application/octet-stream` with its exact length and checksum, and that the JSON manifest is published only after successful uploads. HTTP 413 must stop publication without automatically retrying the upload.

The Dockerfile downloads Go dependencies while building the image. The running test network does not need internet access. The initial image build does need access to container registries, Go modules, and Debian package repositories.

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

Linux containers on Docker Desktop do not verify Windows process handling or macOS filesystem behavior, so container testing complements the native matrix. The Compose image uses Go 1.27.1. Keep its version synchronized with `go.mod` when updating Go.

## Local development commands

Windows-native tests cover SCM stop/shutdown cancellation, failure reporting, and console-free background children. Login-task tests also read the real scheduler to verify that an absent task is a successful no-op, and use PowerShell fixtures to verify exact task filtering, ownership checks, and readable errors without CLIXML progress output. These checks need no elevation and do not create or remove tasks. To exercise an actual service, run from Administrator PowerShell:

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
