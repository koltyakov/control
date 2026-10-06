# Working on Control

Control is a Go peer execution network for Windows, Linux, and macOS. Every node can request work, execute capabilities, and exchange artifacts with other nodes. AI agents and application-specific tools are optional providers.

These instructions apply throughout the repository.

## Terminology

Follow [Terminology](docs/terminology.md) in documentation, code, CLI/dashboard text, and MCP instructions. Orchestrator coordinates work; worker executes requested work; gateway handles enrollment, discovery, signaling, relay, and fleet administration. Orchestrator and worker are operation roles, not fixed machine types. Client is the CLI/MCP component; node is the enrolled execution service and can perform either role. An orchestrator can use a standalone client without a local node. Reserve agent for AI software, not Control services. Preserve existing command, configuration, protocol, authentication-role, and session-backend identifiers.

## Read first

- [README](README.md): build, deployment, examples, and current limitations.
- [Terminology](docs/terminology.md): canonical roles, components, and naming rules.
- [Architecture](docs/architecture.md): components, connections, identity, and execution lifecycle.
- [Engineering principles](docs/principles.md): constraints to preserve when changing the system.
- [Design decisions](docs/decisions.md): current choices, their reasons, and consequences.
- [Configuration and protocol](docs/protocol.md): configuration, operations, schemas, and limits.
- [Docker Compose testing](docs/testing.md): repeatable multi-node tests and debugging.
- [Dashboard and system metrics](docs/dashboard.md): pool observation, availability, and sampling behavior.
- [Managed updates](docs/updates.md): superuser keys, release bundles, idle reservations, and service restarts.
- [Installation and enrollment](docs/installation.md): host profiles, MCP/skills, user startup, and one-time installation links.
- [Users and private fleets](docs/users.md): account provisioning, isolation, SQLite ownership, and migration.

## Repository map

| Path | Responsibility |
| --- | --- |
| `cmd/control` | Binary entry point, CLI commands, gateway/node startup |
| `internal/client` | Shared local API client and AI-facing MCP server |
| `internal/enrollment`, `internal/installation` | Invitation contracts/scripts, host profiles, agent configuration, and user startup |
| `internal/dashboard` | Interactive and plain-text pool activity views |
| `internal/system` | Periodic, completion-triggered, and requested resource sampling |
| `internal/update`, `internal/workgate` | Verified binary staging, rollout state, and idle maintenance admission |
| `internal/gateway` | Enrollment, naming directory, presence, signaling, relay |
| `internal/identity` | Persistent Ed25519 identity and pinned peer TLS |
| `internal/transport` | WebRTC/relay connections and multiplexed peer sessions |
| `internal/node` | Providers, access checks, tasks, artifacts, leases, workflows |
| `internal/model` | Shared application types |
| `internal/protocol` | Protobuf routing envelope and generated Go bindings |
| `internal/store` | Atomic metadata writes and platform-specific directory sync |
| `examples` | Node configurations, task specifications, subprocess provider |
| `tests/compose` | Container images, test lifecycle scripts, cross-container tests |
| `compose.yaml`, `compose.relay.yaml` | Isolated test pool and relay-only override |

## Implementation rules

- Keep the core in Go, compatible with Go 1.27 and the three supported operating systems. Preserve CGO-free builds.
- Keep execution in nodes and connection handling in the transport package. Add application integrations through providers or MCP.
- Share client behavior through `internal/client`; keep CLI and MCP adapters consistent.
- Propagate contexts and cancellation. Bound queues and buffers, and shut down owned goroutines, streams, subprocesses, and files.
- Preserve task acceptance durability, owner checks, and idempotent submission semantics. Never retry an uncertain side effect automatically.
- Preserve peer identity checks and equivalent authorization on both transports. Keep artifact grants bound to their recipient, artifact, and expiry.
- Derive fleet ownership from authenticated credentials. Scope discovery, signaling, relay, enrollment, and management to that user. Preserve immutable identity ownership and receiver membership checks, including before artifact grants. Persist gateway account and enrollment changes transactionally in SQLite before acknowledging them.
- Use streamed artifacts for large results. Keep bulk bytes out of orchestration messages and avoid routing transfers through the orchestrator unnecessarily.
- Keep OS-specific process and filesystem behavior in platform-specific files. Do not assume a Unix shell exists on a remote node.
- Edit `internal/protocol/control.proto`, then regenerate bindings with `make proto`. Do not hand-edit `control.pb.go`.
- Update capability descriptions, API/MCP routing, examples, and protocol documentation when their contracts change.
- Keep resource sampling separate from dashboard polling. Preserve cached sample times and explicit refresh behavior. Node-wide observation must not bypass owner-scoped task control or expose execution arguments and credentials.
- Keep update publication superuser-only. Common keys must not expose administrative CLI help. Preserve idle reservations across service restarts and account for accepted tasks, outgoing work, transfers, tunnels, and leases before updating.

## Verification

Tests are not required for every change. Run relevant tests when requested or when the change warrants them. Use Docker Compose when running integration tests:

```sh
make test-compose
```

This runs the core Go checks once and cross-container tests over both WebRTC and the relay. Use `make test-compose-webrtc` or `make test-compose-relay` for a focused run. The script handles startup readiness, failure logs, exit status, and cleanup of its test volumes. See [testing documentation](docs/testing.md) for keeping a deployment around for debugging.

For code changes, format the affected Go files. Linting is mandatory. Before completing any code change, run the following checks after the final edits and verify that they pass:

```sh
make fmt-check
make lint
go vet ./...
go build -o bin/control ./cmd/control
```

Fix lint and test failures caused by the changes, then rerun the affected checks. Report the commands run and their results in the final response. If a check is blocked or fails for an unrelated reason, state that explicitly; do not claim verification passed or skip a failing check.

The Compose runner executes vet, race tests, and the build, so a successful full Compose run does not require repeating those checks on the host. Formatting and golangci-lint must still be verified separately with `make fmt-check` and `make lint`. `make check` remains available for native Go checks. CI runs Compose on Linux and native tests and builds on Linux, macOS, and Windows. For platform-sensitive changes, check the affected cross-platform builds; cross-compilation alone does not verify runtime behavior.

Prefer behavioral tests covering peer interactions and failures. Existing tests exercise three-node execution and delivery, WebRTC and relay paths, reconnects, cancellation, ownership, grants, leases, MCP, and restart reconciliation. Keep the core Go tests self-contained with temporary state and local listeners. Tests requiring installed applications such as FFmpeg belong in the tagged Compose suite, where the test image supplies those dependencies.

For documentation-only changes, check relative links and confirm described behavior against the implementation. A full Go test run is unnecessary.

## Documentation maintenance

Keep responsibilities separate:

- `docs/architecture.md` describes how the implementation works.
- `docs/principles.md` states the invariants future changes should preserve.
- `docs/decisions.md` records choices and tradeoffs. Append a numbered decision when changing an architectural contract; mark an older decision superseded and link its replacement.
- `docs/protocol.md` is the operational reference for current configuration and APIs.
- `docs/testing.md` describes container test commands, topology, and coverage.
- `docs/dashboard.md` documents observation permissions, machine states, resource sampling, and terminal usage.
- `docs/updates.md` documents administrative keys, release assets, development pushes, and managed restart behavior.
- `docs/installation.md` documents host setup, MCP/skills, user startup, and machine invitations.
- `docs/users.md` documents user accounts, fleet boundaries, SQLite persistence, and migration.

Distinguish implemented behavior from proposed work. Preserve links between these documents and the README when adding or moving documentation.
