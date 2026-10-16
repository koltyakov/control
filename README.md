# Control

<p align="center">
  <img src="./assets/logo.png" alt="expose logo" width="220" />
</p>

A Go peer execution network for Windows, Linux, and macOS. Connect machines to a gateway and address them by name through CLI or MCP. A local HTTP API handles node-local operations.

Orchestrators coordinate work; worker nodes execute it and exchange files directly. These are operation roles, not machine types. The gateway handles enrollment, discovery, connections, and fleet administration. AI agents are optional.

## What it does

- Private user fleets with persistent machine identities and names.
- Direct WebRTC connections with WebSocket relay fallback and mutually authenticated TLS on both paths.
- Durable tasks with idempotent submission, cancellation, streamed logs, and restart reconciliation.
- Commands, scripts, AI CLIs, MCP servers, and opt-in GUI automation.
- SHA-256 artifacts, resumable downloads, and direct worker-to-worker delivery.
- Forward/reverse TCP tunnels, private-network HTTP requests, and explicit clipboard pastes.
- Leases, bounded parallel workflows, a live dashboard, and idle-only managed updates.

Account-authenticated clients initiate execution. Workers need instruction-bound delegation to request peer work; fleet membership permits discovery only. See [authority and delegation](docs/delegation.md).

## Architecture

![Architecture](./assets/architecture.svg)

See [architecture](docs/architecture.md) for roles and connections.

## Install the gateway on a Linux VPS

Follow the [Linux VPS guide](docs/gateway.md) for Ubuntu/Debian setup with systemd, Caddy HTTPS, and user accounts.

Point a domain at the VPS, allow TCP 80/443, and keep the gateway on `127.0.0.1:7330` behind the proxy. Create accounts with `control users create NAME`. Give users their own account keys, never the gateway superuser key.

## Install the orchestrator CLI

Linux or macOS:

```sh
curl -fsSL https://raw.githubusercontent.com/koltyakov/control/main/scripts/install.sh | sh
```

Windows, from PowerShell, CMD, or Bash:

```powershell
powershell.exe -NoProfile -c "irm https://raw.githubusercontent.com/koltyakov/control/main/scripts/install.ps1|iex"
```

Installers require published GitHub releases. On Windows, open a new terminal to reload PATH.

Log in with the account key supplied by your gateway operator:

```sh
control login --gateway https://control.example.com
control install-mcp opencode
control install-skill opencode
control machines
control dashboard
```

Login saves the validated URL and key. MCP and skill installation is optional; see [other clients](#install-mcp-and-skills). Remote work needs no local node:

```sh
control exec worker -- hostname
control call worker node.describe
```

To also run this machine as a worker:

```sh
control setup --name main --client opencode
```

Setup reuses your login and installs node startup, MCP, and the skill. On Windows, it requires Administrator PowerShell by default; `--service user` selects startup at login under your normal account.

## Install workers on Windows, macOS, and Linux

```sh
control machines add win-worker --platform windows --service user
control machines add mac-worker --platform macos --service user
control machines add linux-worker --platform linux --service system
```

Run each printed command on its target under the intended user. Windows uses PowerShell and requests UAC for firewall setup. macOS uses Bash; Linux system mode needs sudo and systemd. Installers detect amd64/arm64, verify the binary, and enroll and start the node without an account key. Verify with `control machines` and `control call win-worker node.describe`.

Use `auto` or omit the name for the target's hostname. Invitations are single-use and expire after 15 minutes by default. Press `a` in the dashboard to add a machine. See [platform requirements](docs/installation.md#install-workers-on-each-platform).

Use `--force` when adding or renaming to retract a conflicting pending invitation, never an enrolled identity.

User context has the installing user's files and credentials but may stop at logout. System context starts at boot as root on Linux/macOS or LocalService on Windows. See [startup contexts](docs/installation.md#choose-a-startup-context).

## Run and inspect work

```sh
control exec worker --detach --id job-001 --timeout 4h -- COMMAND ARG...
control task logs worker job-001 --follow
control task wait worker job-001
control task cancel worker job-001
```

Stopping a wait or log follower does not cancel work. Node restarts do not replay interrupted tasks. Use a new task ID for new work.

```sh
control system worker
control system worker --refresh
control speedtest worker --size 64 --json
control artifact export worker input.mp4
control artifact get worker ARTIFACT_ID ./output.mp4
```

Dashboard polls read cached resource samples; connection tests run on request. Artifact downloads resume and verify SHA-256. See the [protocol reference](docs/protocol.md) for calls, grants, and workflows.

## Install MCP and skills

Install either or both on the orchestrator. MCP exposes tools; the skill teaches CLI usage. Both install globally by default.

```sh
control install-mcp opencode
control install-skill opencode
```

Replace `opencode` with `codex`, `claude`, `cursor`, `copilot`, `windsurf`, `antigravity`, or `agents` for other clients. Add `--project .` for project-local installation. Restart the AI client afterward.

The `agents` target uses `~/.agents/mcp.json` and `~/.agents/skills/control/SKILL.md`, or project-local equivalents. Manual MCP import may be needed. See [configuration paths](docs/installation.md#mcp-and-skills).

For other MCP clients, configure a stdio server running `/absolute/path/to/control mcp`. It uses your saved login, without a local node. Explicit `--api` or `CONTROL_API` selects local-node authority, not independent peer execution.

## Build and test

Requires Go 1.27.2 or newer. No CGO dependency.

```sh
make build
make fmt-check
make lint
make check
make test-compose
```

Compose tests run a gateway, three nodes, and an FFmpeg workflow over WebRTC and relay. Docker supplies dependencies. See `make help` and [testing](docs/testing.md) for other targets and benchmarks.

## Updates

`control update` updates the CLI from GitHub Releases. Managed node restarts wait for idle reservations. With saved superuser authorization, `make update` pushes a development bundle to the gateway and nodes; `make update-status` tracks it. See [managed updates](docs/updates.md).

## Documentation

- [Linux VPS gateway](docs/gateway.md), [installation and enrollment](docs/installation.md), [users and private fleets](docs/users.md), [authority and delegation](docs/delegation.md).
- [Configuration and protocol](docs/protocol.md), [dashboard and metrics](docs/dashboard.md), [managed updates](docs/updates.md).
- [TCP tunnels](docs/tunnels.md), [connection tests](docs/connections.md), [fleet streaming](docs/streaming.md).
- [GUI automation](docs/rpa.md), [worker-local secrets](docs/secrets.md), [clipboard pastes](docs/clipboard.md), [local machine scenarios](docs/scenarios.md).
- [Terminology](docs/terminology.md), [architecture](docs/architecture.md), [engineering principles](docs/principles.md), [design decisions](docs/decisions.md).
- [Testing](docs/testing.md), [examples](examples), [contributor guide](AGENTS.md).

## Limits and trust

One gateway, no clustering, public signup, billing, or execution sandbox. Providers have their node's OS permissions. Keep account keys and gateway state inaccessible to workers, and the local API on loopback. Trust the gateway for naming, membership, and executable distribution. See [security boundaries](docs/principles.md#authority-is-explicit-at-the-destination).

## Licence

[MIT](LICENSE)
