# Control

A Go peer execution network for Windows, Linux, and macOS. Connect machines to a gateway, then address them by name through the CLI, MCP, or local HTTP API.

An orchestrator coordinates work through a client. Worker nodes execute it and exchange files directly. The gateway handles enrollment, discovery, signaling, encrypted relay fallback, and fleet administration. Orchestrator and worker are operation roles, not fixed machine types. AI agents are optional callers or providers.

## What it does

- Private user fleets with persistent machine identities and names.
- Direct WebRTC connections with WebSocket relay fallback and mutually authenticated TLS on both paths.
- Durable tasks with idempotent submission, cancellation, streamed logs, and restart reconciliation.
- Commands, scripts, AI CLIs, MCP servers, and opt-in GUI automation.
- SHA-256 artifacts, resumable downloads, and direct worker-to-worker delivery.
- Forward/reverse TCP tunnels, private-network HTTP requests, and explicit clipboard pastes.
- Leases, bounded parallel workflows, a live dashboard, and idle-only managed updates.

Account-authenticated clients initiate execution. Workers can request peer work only through instruction-bound orchestrator delegation. Fleet membership alone permits discovery, not execution. See [authority and delegation](docs/delegation.md).

## Install the gateway on a Linux VPS

Use the [Linux VPS guide](docs/gateway.md) for a complete Ubuntu/Debian installation: install the verified binary, create a private operator key, run the gateway as an unprivileged systemd service, configure HTTPS with Caddy, and provision a user account.

Point a domain at the VPS and allow TCP 80/443. Keep the gateway's listener on `127.0.0.1:7330` behind the reverse proxy. The operator creates accounts with `control users create NAME` and gives each user only their own account key. Do not distribute the gateway superuser key to workers.

## Install the orchestrator CLI

Linux or macOS:

```sh
curl -fsSL https://raw.githubusercontent.com/koltyakov/control/main/scripts/install.sh | sh
```

Windows, from PowerShell, CMD, or Bash:

```powershell
powershell.exe -NoProfile -c "irm https://raw.githubusercontent.com/koltyakov/control/main/scripts/install.ps1|iex"
```

On Windows, open a new terminal to pick up the saved PATH. The installers require published GitHub release assets.

Log in with the account key supplied by your gateway operator:

```sh
control login --gateway https://control.example.com
control install-mcp opencode
control install-skill opencode
control machines
control dashboard
```

Login validates and saves your gateway URL and key. The integration commands above target OpenCode; see [MCP and skills](#install-mcp-and-skills) for other clients. Scripts and human CLI users can skip the integrations. Remote work needs no local node:

```sh
control exec worker -- hostname
control call worker node.describe
```

To make this machine an execution host and configure an AI client:

```sh
control setup --name main --client opencode
```

Setup reuses your saved login and installs node startup, MCP, and the Control skill. On Windows, default setup requires Administrator PowerShell; use `--service user` for startup at your login under your normal account. Supported AI clients include OpenCode, Claude Code, Cursor, Copilot, Codex, Windsurf, Antigravity, and generic agents.

## Install workers on Windows, macOS, and Linux

```sh
control machines add win-worker --platform windows --service user
control machines add mac-worker --platform macos --service user
control machines add linux-worker --platform linux --service system
```

Run each printed installation command on its target. Windows uses PowerShell under the intended user and requests UAC for firewall setup. macOS uses Bash in that user's terminal. The Linux system-context command uses sudo and requires systemd. Installers detect amd64/arm64, verify the binary, redeem the invitation, and start the node. No worker login or account key is needed. Verify enrollment with `control machines` and `control call win-worker node.describe` from the orchestrator.

Use `auto`, or omit the name, to select the target's hostname. Invitations are single-use and expire after 15 minutes by default. You can also press `a` in the dashboard. See [platform requirements and worker startup](docs/installation.md#install-workers-on-each-platform).

User context runs with the installing user's files and credentials, but can stop after logout. System context starts at boot and survives logout. Linux/macOS system nodes run as root; Windows uses LocalService. Choose the context deliberately, especially for desktop tools. See [installation and startup](docs/installation.md).

## Run and inspect work

```sh
control exec worker --detach --id job-001 --timeout 4h -- COMMAND ARG...
control task logs worker job-001 --follow
control task wait worker job-001
control task cancel worker job-001
```

Stopping a wait or log follower does not cancel accepted work. Interrupted tasks are not automatically replayed after a node restart. Use a new task ID for new work.

```sh
control system worker
control system worker --refresh
control speedtest worker --size 64 --json
control artifact export worker input.mp4
control artifact get worker ARTIFACT_ID ./output.mp4
```

Resource sampling is separate from dashboard polling. Connection tests run only when requested. Artifact downloads retain partial files after interruption, resume by offset, and verify SHA-256 before publication. See the [protocol reference](docs/protocol.md) for capability calls, grants, and workflows.

## Install MCP and skills

Run the pair for your AI client on the orchestrator. MCP exposes Control tools; the skill teaches the client how to use the CLI. You can install either independently. Both default to global installation.

```sh
# OpenCode
control install-mcp opencode
control install-skill opencode

# OpenAI Codex
control install-mcp codex
control install-skill codex

# Claude Code
control install-mcp claude
control install-skill claude

# Generic agents using .agents
control install-mcp agents
control install-skill agents
```

Other client selectors are `cursor`, `copilot`, `windsurf`, and `antigravity`. Use the same commands with that name. Add `--project .` to either command for project-local installation, then restart the AI client to reload its configuration and skills.

The generic `agents` target writes `~/.agents/mcp.json` and `~/.agents/skills/control/SKILL.md`, or their project-local `.agents` equivalents. Your client may need to import that MCP config manually. See [all supported selectors and configuration paths](docs/installation.md#mcp-and-skills).

For another MCP-compatible client, configure a stdio server to run `/absolute/path/to/control mcp`. It uses the saved account login without a local node. Ask your AI client to work on a named machine; use durable tasks for long operations. Explicit `--api` or `CONTROL_API` selects local-node authority and cannot independently initiate peer execution.

## Build and test

Go 1.27.2 or newer is required. Builds have no CGO dependency.

```sh
make build
make fmt-check
make lint
make check
make test-compose
```

`make test-compose` runs an isolated gateway, three nodes, and cross-container checks over WebRTC and relay, including an FFmpeg workflow. Docker supplies Go and application dependencies. Run `make help` for other development targets and see [testing](docs/testing.md) for focused checks and performance benchmarks.

## Updates

`control update` updates the invoked CLI from GitHub Releases. Running nodes use the gateway's separate managed rollout. With saved superuser authorization, `make update` pushes a development bundle to the gateway and enrolled nodes; `make update-status` reports progress. Actual managed restarts require idle reservations. See [managed updates](docs/updates.md).

## Documentation

- [Linux VPS gateway](docs/gateway.md), [installation and enrollment](docs/installation.md), [users and private fleets](docs/users.md), [authority and delegation](docs/delegation.md).
- [Configuration and protocol](docs/protocol.md), [dashboard and metrics](docs/dashboard.md), [managed updates](docs/updates.md).
- [TCP tunnels](docs/tunnels.md), [connection tests](docs/connections.md), [fleet streaming](docs/streaming.md).
- [GUI automation](docs/rpa.md), [worker-local secrets](docs/secrets.md), [clipboard pastes](docs/clipboard.md), [local machine scenarios](docs/scenarios.md).
- [Terminology](docs/terminology.md), [architecture](docs/architecture.md), [engineering principles](docs/principles.md), [design decisions](docs/decisions.md).
- [Testing](docs/testing.md), [examples](examples), [contributor guide](AGENTS.md).

## Limits and trust

Control uses a single gateway, without clustering, public signup, billing, or execution sandboxing. Providers run with their node's OS permissions. Keep account keys and gateway state out of worker-accessible locations, and keep the local node API on loopback. The gateway is trusted for naming, fleet membership, and executable distribution. See [security boundaries](docs/principles.md#authority-is-explicit-at-the-destination).

## Licence

[MIT](LICENSE)
