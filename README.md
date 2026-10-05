# Control

A Go peer execution network for Windows, Linux, and macOS. Connect machines to a public gateway, then address them by name through a CLI, MCP server, or local HTTP API.

Every node can execute work and initiate connections to other nodes. An orchestrator can ask one worker to fetch input from another machine, run an installed tool, and deliver its output to a third machine. AI agents are optional providers.

## What works

- Persistent machine identities, enrollment, names, labels, and online discovery.
- SQLite-backed user accounts and private fleets, isolated across discovery, enrollment, WebRTC, relay, and execution.
- Outbound gateway connections, direct WebRTC data channels, and WebSocket relay fallback.
- Mutually authenticated TLS 1.3 sessions over both transports. The relay forwards encrypted execution traffic.
- Durable task acceptance, idempotent submission IDs, cancellation, offset-based logs, results, and restart reconciliation.
- Commands, scripts, configured AI CLIs, versioned subprocess providers, and local MCP servers.
- Filesystem reads/writes, private-network HTTP requests, and TCP forwarding for database or other protocols.
- Immutable SHA-256 artifacts, resumable transfers, worker-to-worker delivery, and subject-bound artifact grants.
- Label-based selection, exclusive execution leases, bounded task concurrency, and dependency-ordered workflows.
- A local MCP server exposing routing tools for AI clients.
- A live CLI dashboard with registered machines, availability, in-flight work, and sampled system resources.
- Superuser-only development pushes and GitHub Release updates, applied when the pool is idle.
- Host setup with MCP and skills, plus expiring one-time installation commands for new machines.

## Install and connect your main host

Linux or macOS:

```sh
curl -fsSL https://raw.githubusercontent.com/koltyakov/control/main/scripts/install.sh | sh
```

Windows PowerShell:

```powershell
irm https://raw.githubusercontent.com/koltyakov/control/main/scripts/install.ps1 | iex
```

Log in once with the account key supplied by your gateway operator:

```sh
control login --gateway https://control.example.com
```

The prompt hides your key. Control validates it and saves the gateway URL and key in your user configuration directory. Future commands load them automatically:

```sh
control machines
control dashboard
```

To execute work from this machine and configure an AI client, start a host node using that saved login. On Windows, run setup in Administrator PowerShell:

```sh
control setup --name main --client opencode
```

Setup reuses the saved account key, starts the local node, and installs MCP plus the Control skill. Without a saved login, provide `--gateway` and enter the key when prompted. The gateway operator creates your account with `control users create NAME`. Supported clients include OpenCode, Claude Code, Cursor, Copilot, Codex, Windsurf, Antigravity, and generic agents.

Using your saved account credentials, create an installation command for another machine in your fleet:

```sh
control machines add render-01 --platform windows
```

Copy the printed PowerShell command onto the target and run it in Administrator PowerShell. Windows nodes run as automatic system services, without a console window, including before login and after logout. Or press `a` in the dashboard to enter a name, select a platform, and copy the command automatically. Linux and macOS invitations use Bash. The installer detects amd64 or arm64, verifies the selected binary, creates an identity, redeems the single-use ticket, and starts the node. Invitations expire after 15 minutes by default. Success means the machine is registered and available.

See [installation and enrollment](docs/installation.md) for prerequisites, saved profiles, startup, and invitation management. Public installers require published release assets. From a checkout, use `make build` followed by `bin/control setup`.

Running a new invitation on an already registered machine replaces its enrollment in the current local profile. A different name renames the same machine instead of adding a duplicate. Its identity, workspace, and settings are retained.

One account can have multiple host agents and collaborating workers. Other users cannot see or connect to its machines, even when they know a machine ID. See [users and private fleets](docs/users.md) for account provisioning, permissions, SQLite persistence, and upgrading an existing gateway.

## Build

Go 1.27.1 or newer is required. The binary has no CGO dependency. CI reads the Go version from `go.mod`; the Compose test image uses Go 1.27.1.

```sh
make build
go test -race ./... -timeout=120s
```

Without Make, run `go run ./cmd/control-bundle --binary bin/control`. On Windows, use `--binary bin/control.exe`. The builder embeds the source GitHub repository and UTC build time. Run `control help` for the CLI reference.

Build versions come from `git describe --tags --always --dirty`, matching expose: a tagged checkout reports `v0.2.0`, later commits report `v0.2.0-3-gabc1234`, and tracked local changes append `-dirty`. Before the first tag, the version is the short commit hash. Without Git metadata it is `dev`. Override it with `make build VERSION=v0.2.0` or the builder's `--version` flag. Pushing a `v*.*.*` tag triggers the [release workflow](.github/workflows/release.yml).

Install the CLI from the checkout:

```sh
make install
```

This builds and installs `control` to `~/.local/bin` on Linux/macOS or `%LOCALAPPDATA%\Programs\control` on Windows. Set `CONTROL_INSTALL_DIR` to choose another directory, for example `make install CONTROL_INSTALL_DIR=/your/bin`. On Linux/macOS, ensure the installation directory is on PATH.

Run `make help` for development targets. `make deps-update` upgrades dependencies within their current major versions and tidies the module files; `make go-update` updates the Go requirement to the latest stable release. Major-version migrations need import/API changes. After updating Go, keep `tests/compose/Dockerfile` on the same version. `make ci` runs formatting, lint, vet, race tests, vulnerability scanning, native and six-platform builds, bundle verification, and workflow validation. `make ci-compose` adds WebRTC and relay integration tests. See [testing](docs/testing.md) for coverage and focused commands.

## Test with Docker Compose

```sh
make test-compose
```

This starts an isolated gateway, three nodes, and a test runner. It runs the Go checks and a real cross-container FFmpeg workflow, verifies artifact delivery and resumable downloads, and tests both WebRTC and relay-only connections. Docker supplies Go and FFmpeg. Test containers and volumes are removed afterward.

See [Docker Compose testing](docs/testing.md) for individual transport targets, debugging commands, and CI coverage.

## Run three nodes locally

This local example places all three nodes in one legacy fleet. Set the same token in each terminal, using a randomly generated value of at least 16 characters. For unrelated users, provision separate [user accounts](docs/users.md) instead.

```sh
export CONTROL_TOKEN='replace-with-your-random-pool-token'
```

PowerShell equivalent:

```powershell
$env:CONTROL_TOKEN = 'replace-with-your-random-pool-token'
```

Start the gateway and each node in separate terminals:

```sh
bin/control gateway --listen 127.0.0.1:7330
bin/control node --config examples/source.json
bin/control node --config examples/worker.json
bin/control node --config examples/consumer.json
```

The CLI uses the source node's local API by default. Node configuration paths are relative to the configuration file. These examples keep state in `examples/state` and working files in `examples/work`.

```sh
bin/control machines
bin/control call worker node.describe
bin/control call worker files.list '{"path":"."}'
bin/control exec worker -- ffmpeg -version
bin/control call '' nodes.select '{"labels":{"role":"worker"},"capability":"exec.run"}'
```

Each machine in a real deployment runs its own node, normally on local API port 7331. Set its unique name and the public gateway URL in its config. The distinct local ports above are only for running three nodes on one computer.

For an internet gateway, use `https://` in node configs. Terminate HTTPS/WSS at a reverse proxy or use `gateway --tls-cert cert.pem --tls-key key.pem`. Forward WebSocket upgrades and allow long-lived connections. No inbound SSH port is required on nodes.

## Monitor the machine pool

Use your saved host profile to open the dashboard:

```sh
bin/control dashboard
```

The dashboard connects directly to the configured gateway and shows its URL, running version, service uptime, CPU, RAM, and disk usage. With an account key, it also shows each machine's current resources, idle/busy state, and work count without a local node. Nodes report health every five seconds from their independently sampled resource cache. Set `CONTROL_GATEWAY` and `CONTROL_USER_KEY` to use it without a saved profile.

With a local node running, the dashboard also shows running and queued tasks from every owner, synchronous operations, artifact transfer progress, TCP tunnel traffic, and recent completions. It lists idle and offline machines and reports their OS, CPU, RAM, and disk capacity and usage.

Tables adapt to terminal width. Use `d` for full details, arrow keys or Page Up/Page Down to scroll, `r` to refresh activity, and `q` to quit. Refreshes use steady text with no blinking indicators.

The machine table includes running versions. Press `m` to enable, disable, or unregister a selected machine. Disabling keeps it registered and blocks new work. Unregistering removes it and retires its identity, including while offline; lifecycle-capable agents stop on their next gateway contact. CLI equivalents are `control machines disable NAME`, `enable NAME`, and `unregister NAME`. See [registration management](docs/installation.md#manage-registrations).

For scripts or a single view:

```sh
bin/control dashboard --once
bin/control dashboard --json --node worker,consumer
bin/control system worker
bin/control system worker --refresh
```

System metrics are sampled every 15 seconds by default, after heavy tasks finish, or on explicit request. Dashboard refreshes use those cached samples; JSON output retains sampling timestamps. Set `metricsIntervalSeconds` in a node config to change the sampling interval, or `-1` to disable periodic sampling while retaining completion-triggered and explicit refreshes.

See [dashboard and system metrics](docs/dashboard.md) for availability states, permissions, and sampling behavior.

## Update the CLI

```sh
control update
```

This downloads the latest stable GitHub release, verifies it, and replaces the CLI you invoked. `control upgrade` is an alias. No gateway login is required. Use `control update --version v0.2.0` to select a release. Existing binaries without this command need a current build or a one-time reinstall using the installation script above.

Running nodes receive their updates through the gateway's managed rollout, described below. See [CLI updates](docs/updates.md#update-the-local-cli) for repository settings and Windows behavior.

## Update the pool

Set a separate `CONTROL_SUPERUSER_KEY` on the gateway. Release checks use the repository embedded at build time, normally `koltyakov/control`. Set `CONTROL_RELEASE_REPO=owner/repository` to override it, or set it to an empty string to disable release polling. Nodes keep using common enrollment keys.

On your development machine:

```sh
export CONTROL_GATEWAY='https://control.example.com'
export CONTROL_SUPERUSER_KEY='your-gateway-superuser-key'
make update
make update-status
```

The gateway stages platform-specific binaries, waits for idle reservations, updates the nodes, then restarts itself. Common keys cannot publish updates or see administrative CLI help. Create common keys with `control keys create NAME` using your superuser credentials.

See [managed updates](docs/updates.md) for initial setup, release assets, key permissions, and the versioned service launcher.

## Encode and transfer a video

Install FFmpeg on the source and worker machines. The included workflow generates a short source video, then asks the worker to fetch and encode it directly:

```sh
bin/control task start source @examples/workflow-task.json
bin/control task wait source media-workflow-001
```

The completed task's `result.encode.artifacts[0]` is the encoded artifact reference. Deliver it from the worker to the consumer, or download it through your local node:

```sh
bin/control artifact deliver worker ARTIFACT_ID consumer
bin/control artifact get consumer ARTIFACT_ID ./output.mp4
```

Downloads use WebRTC when available and fall back to the relay. The CLI retains a `.partial` file after interruption, resumes at its byte offset, and verifies SHA-256 before renaming it.

To use an existing file, place it under the source's configured `workDir` and run:

```sh
bin/control artifact export source input.mp4
```

Copy the returned artifact reference, including its actual size, into `examples/encode-task.json`, then submit it to the worker. Use a new task ID for new work. Reusing an ID with the same specification returns the existing task; reusing it with different arguments fails.

The orchestrator receives metadata and results. Source-to-worker and worker-to-consumer transfers do not pass through its storage. The public gateway carries the bytes only when those peers need a relay.

## Use an AI client

Start your local node, then configure your AI client's local/stdio MCP integration to run:

```text
/absolute/path/to/control mcp
```

Supply `CONTROL_TOKEN` in its environment and, if needed, `CONTROL_API=http://127.0.0.1:7331`. Multiple CLI and MCP processes share the same local node and identity.

The MCP server exposes tools for finding machines, inspecting capability schemas, submitting and inspecting tasks, discovering remote MCP tools, invoking capabilities, and exporting/delivering artifacts. Its initialization instructions explain machine-name routing. You can then ask the AI to do work on a named machine.

Long operations should use `control_task_start`, rather than an open-ended synchronous tool call. Accepted tasks survive the requesting AI client disconnecting.

## Installed agents and MCP servers

Add providers to a node config:

```json
{
  "agents": {
    "claude": {
      "command": "claude",
      "args": ["--print"]
    }
  },
  "mcp": {
    "local-tools": {
      "command": "path-to-mcp-server",
      "args": []
    },
    "http-tools": {
      "url": "http://127.0.0.1:8000/mcp"
    }
  }
}
```

The programs run as the node's OS user. Configure their credentials on that machine. An agent receives its prompt on stdin, and its output becomes the task result and log. For example:

```sh
bin/control task start worker '{"capability":"agent.run","args":{"agent":"claude","prompt":"Inspect the workspace and summarize its files."}}'
bin/control call worker mcp.discover
bin/control call worker mcp.discover '{"server":"local-tools"}'
bin/control call worker mcp.call '{"server":"local-tools","tool":"TOOL_NAME","arguments":{}}'
```

MCP connections remain open across calls. The bridge supports tool calls, discovery, resource reads/listing, and prompt reads/listing through stdio or Streamable HTTP. Tool results preserve MCP content. Sampling, elicitation, subscriptions, and client-to-server callback forwarding are not implemented.

## Network access

HTTP requests execute on the selected machine, including DNS resolution:

```sh
bin/control call worker http.request '{"url":"http://internal-api/status"}'
```

The response body is base64 encoded. For a database or another TCP service:

```sh
bin/control tunnel worker db.internal:5432 --listen 127.0.0.1:15432
```

Connect your local database client to `127.0.0.1:15432`. The worker opens the connection to `db.internal:5432`.

## Configuration and extension

- [Configuration and protocol](docs/protocol.md): methods, leases, providers, task semantics, and limits.
- [Architecture](docs/architecture.md): components, transport, identity, and recovery.
- [Engineering principles](docs/principles.md): invariants to preserve as the system evolves.
- [Design decisions](docs/decisions.md): implementation choices and their tradeoffs.
- [Docker Compose testing](docs/testing.md): isolated multi-node checks and debugging.
- [Dashboard and system metrics](docs/dashboard.md): pool-wide activity, availability, and sampled resources.
- [Managed updates](docs/updates.md): release polling, development pushes, idle rollout, and superuser keys.
- [Installation and enrollment](docs/installation.md): host setup, MCP/skills, user startup, and one-time machine links.
- [Users and private fleets](docs/users.md): account registration, isolation, SQLite persistence, and migration.
- [Contributor and agent guide](AGENTS.md): repository layout, coding instructions, and verification.

The gateway supports multiple isolated user fleets with SQLite-backed registration and credential storage. It is a single-gateway deployment, without gateway clustering, public self-service signup, billing, or execution sandboxing. Node task and artifact metadata use locked local directories and atomic JSON writes. Windows uses an automatic LocalService service; Linux and macOS support user startup. Desktop and application integrations can be attached through MCP or custom providers; GUI tools need a provider running in the user's desktop session.
