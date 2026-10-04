# Testing with Docker Compose

[Contributor guide](../AGENTS.md) · [Architecture](architecture.md) · [Engineering principles](principles.md) · [Design decisions](decisions.md)

Use Docker Compose for repeatable integration testing. Docker supplies Go, FFmpeg, and the node runtime, so the host only needs Docker Engine or Docker Desktop with Compose v2 or newer. The Make targets also need Make and a POSIX shell.

## Run the tests

The suite also runs generated Bash installers in the test container, verifies online registration and remote execution, rejects reuse and common-key invitation creation, and checks host setup with saved credentials plus MCP/skill installation. Unit tests cover concurrent redemption, expiry, revocation, name reservations, and configuration preservation. These Linux container tests do not exercise native launchd or Windows login startup.

Private-fleet tests create two user accounts with duplicate machine names, multiple host agents, and a worker installed through a user-owned invitation. They verify saved account credentials, same-fleet execution over the required transport, and rejection of cross-user discovery, execution, tasks, artifacts, observation, and invitation revocation. Native tests also send forged signaling/relay packets directly to the gateway, check receiver membership and cross-user artifact grants, reject identity reassignment, and verify transactional JSON-to-SQLite migration and account persistence.

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
- Cached system capabilities and explicit resource refreshes. The core suite also checks periodic/completion-request collection, denied/offline machine states, metadata privacy, and stale-view behavior.
- A development bundle pushed to the gateway, deferred by a running task and lease, then applied by real process restarts on all three nodes and the gateway. Identities, task state, and issued keys survive the rollout.
- Rejection of common-key update uploads and omission of administrative commands from common-key CLI help. Core tests also check release retrieval, corrupted binaries, persisted maintenance reservations, and key revocation.

These tests live in [tests/compose/e2e_test.go](../tests/compose/e2e_test.go) and [tests/compose/updates_test.go](../tests/compose/updates_test.go) behind the `compose` build tag. The ordinary Go suite remains independent of Docker and FFmpeg. Its existing tests cover cancellation, leases, grants, MCP, identity ownership, gateway restarts, and task reconciliation.

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

The Linux Compose job in [CI](../.github/workflows/ci.yml) runs both transport modes. The existing native Go matrix still tests Windows, macOS, and Linux. Linux containers on Docker Desktop do not verify Windows process handling or macOS filesystem behavior, so container testing complements that matrix.
