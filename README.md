# Control

A Go peer execution network for Windows, Linux, and macOS. Connect machines to a public gateway, then address them by name through a CLI, MCP server, or local HTTP API.

Every node can execute requested work and discover peers. An account-authenticated orchestrator can authorize one worker to fetch input from another machine, execute a specific instruction, and deliver output to a third machine. Workers cannot independently execute peer work. AI agents are optional providers. See [orchestrator authority and delegation](docs/delegation.md).

## Roles and components

| Role | Responsibility | Component |
| --- | --- | --- |
| Orchestrator | Coordinates work and collects results | CLI or MCP client, optionally through a local node |
| Gateway | Enrollment, discovery, signaling, encrypted relay fallback, and fleet administration | Gateway service |
| Worker | Executes requested work and exchanges artifacts | Node service |

Orchestrator and worker are roles, not fixed machine types. A node can perform both roles; an orchestrator can also use a standalone client without a local node. Reserve "agent" for AI software. See [terminology](docs/terminology.md) for the naming rules used in documentation and code.

## What works

- Persistent machine identities, enrollment, names, labels, and online discovery.
- SQLite-backed user accounts and private fleets, isolated across discovery, enrollment, WebRTC, relay, and execution.
- Outbound gateway connections, direct WebRTC data channels, and WebSocket relay fallback.
- Mutually authenticated TLS 1.3 sessions over both transports. The relay forwards encrypted execution traffic.
- Durable task acceptance, idempotent submission IDs, cancellation, offset-based logs, results, and restart reconciliation.
- Commands, scripts, configured AI CLIs, versioned subprocess providers, and local MCP servers.
- Opt-in GUI automation through a desktop helper, with native accessibility selectors, mouse/keyboard actions, and screenshot artifacts. See [requirements and limitations](docs/rpa.md).
- Filesystem reads/writes, private-network HTTP requests, and TCP forwarding for database or other protocols.
- Worker-local secret references for GUI credential entry and HTTP authentication, without a secret-value read API. See [secrets and their limits](docs/secrets.md).
- Immutable SHA-256 artifacts, resumable transfers, worker-to-worker delivery, and subject-bound artifact grants.
- Instruction-bound worker delegation with revocation, cancellation, and one-hour idle expiry.
- Explicit bidirectional clipboard pastes through CLI/MCP, with regular files streamed only when paste is requested. See [clipboard requirements and limits](docs/clipboard.md).
- Label-based selection, exclusive execution leases, bounded task concurrency, and dependency-ordered workflows.
- A local MCP server exposing routing tools for AI clients.
- Independent control/bulk/interactive traffic lanes, shared WebRTC carriers, streamed task logs, and negotiated TCP half-close.
- A live CLI dashboard with registered machines, availability, in-flight work, and sampled system resources.
- Superuser-only development pushes and GitHub Release updates, applied when the pool is idle.
- Host setup with MCP and skills, plus expiring one-time installation commands for new machines.

## Install and connect your main host

Linux or macOS:

```sh
curl -fsSL https://raw.githubusercontent.com/koltyakov/control/main/scripts/install.sh | sh
```

Windows, from PowerShell, CMD, or Bash:

```powershell
powershell -NoProfile -c "irm https://raw.githubusercontent.com/koltyakov/control/main/scripts/install.ps1 | iex"
```

On Windows, open a new terminal after installation to pick up the saved user PATH.

Log in once with the account key supplied by your gateway operator:

```sh
control login --gateway https://control.example.com
```

The prompt hides your key. Control validates it and saves the gateway URL and key in your user configuration directory. Future commands load them automatically:

```sh
control machines
control dashboard
```

Remote execution also works with just that login, without a local service:

```sh
control exec worker -- hostname
control call worker node.describe
```

The CLI opens an account-authenticated client session for remote work, even when a local node is running. Concurrent CLI/MCP processes use independent connections with a shared persistent task-owner identity, so a tunnel does not block task monitoring or cancellation. The client is not a fleet machine and never appears in the dashboard or managed rollouts. Upgrade the gateway and target nodes for instruction-bound delegation. Explicit `--api` or `CONTROL_API` selects only the local API, which cannot independently execute peer work. See [delegation and upgrade requirements](docs/delegation.md).

To make this machine an execution host and configure an AI client, start a host node using that saved login. On Windows, run setup in Administrator PowerShell:

```sh
control setup --name main --client opencode
```

Setup reuses the saved account key, starts the local node, and installs MCP plus the Control skill. Without a saved login, provide `--gateway` and enter the key when prompted. The gateway operator creates your account with `control users create NAME`. Supported clients include OpenCode, Claude Code, Cursor, Copilot, Codex, Windsurf, Antigravity, and generic agents.

Using your saved account credentials, create an installation command for another machine in your fleet:

```sh
control machines add render-01 --platform windows
control machines add auto --platform windows
```

Use `auto`, or omit the name, to register the target under its own hostname when it runs the installer. In the dashboard's Add machine form, a blank name also selects automatic naming.

The wizard also asks for User context or System context. User context runs with your files and credentials but can stop after logout. System context starts at boot and survives logout. It requires sudo on Linux/macOS and Administrator PowerShell on Windows. Linux/macOS system nodes run as root; Windows uses LocalService. The CLI equivalent is `control machines add auto --platform linux --service system`. See [startup contexts](docs/installation.md#choose-a-startup-context) before switching an existing installation.

Copy the printed PowerShell command onto the target and run it as the intended Windows user. Windows invitation scripts default to user-login startup, allowing access to that user's files and application credentials. The node is available only while that user is logged in. Set `$env:CONTROL_SERVICE_MODE = 'auto'` before running the command in Administrator PowerShell to select a boot-time LocalService system service instead. Switching an existing system service to user startup also requires Administrator PowerShell under the intended user. See [user-login startup](docs/installation.md#switch-windows-to-user-login-startup). Or press `a` in the dashboard to enter a name, select a platform, and copy the command automatically. Linux and macOS invitations use Bash. The installer detects amd64 or arm64, verifies the selected binary, creates an identity, redeems the single-use ticket, and starts the node. Invitations expire after 15 minutes by default. Success means the machine is registered and available.

See [installation and enrollment](docs/installation.md) for prerequisites, saved profiles, startup, and invitation management. Public installers require published release assets. From a checkout, use `make build` followed by `bin/control setup`.

Windows worker installers request UAC elevation to configure executable-scoped firewall rules for WebRTC UDP and outbound gateway TCP. They do not open the loopback API or elevate user-mode nodes. A stable, verified runtime path keeps these rules valid across managed updates. See [Windows firewall setup](docs/installation.md#windows-firewall).

Running a new invitation on an already registered machine replaces its enrollment in the current local profile. A different name renames the same machine instead of adding a duplicate. Its identity, workspace, and settings are retained.

One account can have multiple orchestrator machines and collaborating worker nodes. Other users cannot see or connect to its machines, even when they know a machine ID. See [users and private fleets](docs/users.md) for account provisioning, permissions, SQLite persistence, and upgrading an existing gateway.

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

This builds and installs `control` to `~/.local/bin` on Linux/macOS or `%LOCALAPPDATA%\Programs\control` on Windows. It also installs the Control skill globally for OpenCode, just like the release installers. MCP configuration remains opt-in through `control install-mcp opencode` or `setup --client opencode`. Set `CONTROL_INSTALL_DIR` to choose another executable directory, for example `make install CONTROL_INSTALL_DIR=/your/bin`. On Linux/macOS, ensure the installation directory is on PATH.

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

For execution, configure a separate gateway superuser key or user account and log in from the orchestrator with `control login --gateway http://127.0.0.1:7330`. Keep that key out of node terminals and configuration. The common token above permits node enrollment and discovery, not execution initiation. Node configuration paths are relative to the configuration file. These examples keep state in `examples/state` and working files in `examples/work`.

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

With an account login and authorized peer observation, the dashboard also shows running and queued tasks from every owner, synchronous operations, artifact transfer progress, TCP tunnel traffic, and recent completions. It lists idle and offline machines and reports their OS, CPU, RAM, and disk capacity and usage.

Tables adapt to terminal width. Use `d` for full details, arrow keys or Page Up/Page Down to scroll, `r` to refresh activity, and `q` to quit. Drag across text to select it; releasing the mouse copies only that selection to the local clipboard. The display stays fixed during the drag and resumes on release. Press `c` to copy the last selection again. Refreshes use steady text with no blinking indicators.

The machine table includes running versions. Press `m` to rename, enable, disable, or unregister a selected machine. Renaming sets its routing alias without restarting it or interrupting work. Disabling keeps it registered and blocks new work. Unregistering removes it and retires its identity, including while offline; lifecycle-capable nodes stop on their next gateway contact. CLI equivalents are `control machines rename NAME NEW_NAME`, `disable NAME`, `enable NAME`, and `unregister NAME`. See [registration management](docs/installation.md#manage-registrations).

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

On your development machine, using persisted gateway superuser authorization:

```sh
make update
make update-status
```

`control login` persists a validated superuser key for update administration as well as the regular login. Switching back to a normal fleet login retains that update authorization. `make update` checks saved credentials before building the platform bundle and never asks for a key. A login without superuser authorization produces a permission error instead. No environment variables or repeated key entry are needed once authorized.

This pushes the local checkout's build, not a GitHub release. The gateway stages platform-specific binaries, waits for idle reservations, updates every enrolled node across user fleets, then restarts itself. That includes nodes on orchestrator machines and all workers; offline nodes catch up when they reconnect. Standalone CLI/MCP installations are not enrolled nodes and need a separate CLI update. Common keys cannot publish updates or see administrative CLI help. Create common keys with `control keys create NAME` using your superuser credentials.

See [managed updates](docs/updates.md) for initial setup, release assets, key permissions, and the versioned service launcher.

## Encode and transfer a video

Install FFmpeg on the source and worker machines. The included workflow generates a short source video, then asks the worker to fetch and encode it directly:

```sh
bin/control task start source @examples/workflow-task.json
bin/control task wait source media-workflow-001
```

The completed task's `result.encode.artifacts[0]` is the encoded artifact reference. Deliver it from the worker to the consumer, or download it with your account client:

```sh
bin/control artifact deliver worker ARTIFACT_ID consumer
bin/control artifact get consumer ARTIFACT_ID ./output.mp4
```

Downloads use WebRTC when available and fall back to the relay. The CLI retains a `.partial` file after interruption, resumes at its byte offset, and verifies SHA-256 before renaming it.

To use an existing file, place it under the source's configured `workDir` and run:

```sh
bin/control artifact export source input.mp4
bin/control call source artifacts.grant '{"id":"ARTIFACT_ID","target":"worker"}'
```

Copy the granted artifact reference, including its actual size and `grant`, into `examples/encode-task.json`, then submit it to the worker. The recipient-bound grant authorizes the worker to fetch that input. Use a new task ID for new work. Reusing an ID with the same specification returns the existing task; reusing it with different arguments fails.

The orchestrator receives metadata and results. Source-to-worker and worker-to-consumer transfers do not pass through its storage. The public gateway carries the bytes only when those peers need a relay.

## Paste a clipboard

Paste local clipboard text onto a worker, or stream copied files into an existing workspace directory. Add `--reverse` to paste the worker's clipboard onto this machine:

```sh
control clipboard paste worker --dir incoming
control clipboard paste worker --reverse --dir ./downloads
```

Text replaces the destination OS clipboard; files go into the destination directory without overwriting existing entries. MCP exposes `control_clipboard_paste` with `node`, optional `reverse`, and `dir`. There is no background synchronization or native desktop paste hook. See [clipboard pastes](docs/clipboard.md) for desktop requirements and supported file types.

## Use an AI client

Configure your AI client's stdio MCP integration to run:

```text
/absolute/path/to/control mcp
```

The MCP process uses the saved account login without requiring a local node. Concurrent CLI and MCP processes share their stable client owner but have separate transport identities. Use named execution targets; account clients can aggregate peer activity directly. Explicit `CONTROL_API` selects local-node work and discovery, not independent peer execution. See [orchestrator authority and delegation](docs/delegation.md).

The MCP server exposes tools for finding machines, inspecting capability schemas, submitting and inspecting tasks, discovering remote MCP tools, invoking capabilities, and exporting/delivering artifacts. Its initialization instructions explain machine-name routing. You can then ask the AI to do work on a named machine.

Long operations should use `control_task_start`, rather than an open-ended synchronous tool call. Accepted tasks survive the requesting AI client disconnecting.

The CLI can return after task acceptance and follow its logs in another process:

```sh
control exec worker --detach --id job-001 --timeout 4h -- COMMAND ARG...
control task logs worker job-001 --follow
control task wait worker job-001
control task cancel worker job-001
```

Stopping a wait or log follower does not cancel the task. Use `control session` to inspect the current process's owner identity and connections.

Compatible workers stream followed logs instead of polling. Peer sessions separate control calls from bulk artifacts and long-lived TCP/log streams. Upgraded peers share one WebRTC carrier across independent lane channels; older peers use separate connections. See [fleet streaming](docs/streaming.md) for scenarios, compatibility, measured allocation improvements, and limits.

## Installed AI agents and MCP servers

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

To open your local dev server from a remote machine, start the application locally, then run this in another terminal:

```sh
npm run dev
# In another terminal, assuming the dev server uses port 3000:
control tunnel worker 127.0.0.1:3000 --reverse --listen 127.0.0.1:3000
```

On `worker`, open `http://localhost:3000`. Requests reach the dev server on your orchestrator machine, including WebSocket connections for hot reload. The two ports can differ if the remote port is occupied. Keep the tunnel command running; Ctrl+C closes the remote listener without stopping your dev server. No local node or public dev-server binding is required. Restricted workers need `tcp.listen` permission.

MCP clients use `control_forward_start` with `node`, `address`, and optional `listen`. Add `reverse: true` to bind `listen` on the remote machine and dial `address` on the orchestrator. It returns a forward ID and bound address without holding the tool call open. `control_forward_list` reports active sockets and errors; `control_forward_stop` closes the listener and sockets. These forwards belong to the MCP process, default to loopback, and close when it exits. They require remote `tcp.open` permission, or `tcp.listen` for reverse forwarding. See [orchestrator sessions and forwards](docs/protocol.md#orchestrator-sessions-and-forwards) for limits.

## Configuration and extension

- [Terminology](docs/terminology.md): orchestrator, gateway, and worker roles; client and node components; naming rules.
- [Configuration and protocol](docs/protocol.md): methods, leases, providers, task semantics, and limits.
- [Orchestrator authority and delegation](docs/delegation.md): worker isolation, instruction-bound access, revocation, and idle expiry.
- [Architecture](docs/architecture.md): components, transport, identity, and recovery.
- [Engineering principles](docs/principles.md): invariants to preserve as the system evolves.
- [Design decisions](docs/decisions.md): implementation choices and their tradeoffs.
- [Fleet streaming](docs/streaming.md): traffic lanes, multiplexers, duplex forwarding, followed logs, limits, and remaining gaps.
- [Docker Compose testing](docs/testing.md): isolated multi-node checks and debugging.
- [Dashboard and system metrics](docs/dashboard.md): pool-wide activity, availability, and sampled resources.
- [Managed updates](docs/updates.md): release polling, development pushes, idle rollout, and superuser keys.
- [Installation and enrollment](docs/installation.md): host setup, MCP/skills, user startup, and one-time machine links.
- [Users and private fleets](docs/users.md): account registration, isolation, SQLite persistence, and migration.
- [GUI automation](docs/rpa.md): desktop helper setup, accessibility selectors, input actions, screenshot artifacts, and safety limits.
- [Worker-local secrets](docs/secrets.md): hidden credential entry, GUI/HTTP references, text masking, and security limits.
- [Clipboard pastes](docs/clipboard.md): explicit local/remote text transfer and streamed regular-file pastes through CLI/MCP.
- [Contributor and agent guide](AGENTS.md): repository layout, coding instructions, and verification.

The gateway supports multiple isolated user fleets with SQLite-backed registration and credential storage. It is a single-gateway deployment, without gateway clustering, public self-service signup, billing, or execution sandboxing. Node task and artifact metadata use locked local directories and atomic JSON writes. Windows invitation scripts default to user-login startup; direct `control setup` defaults to an automatic LocalService service. Linux and macOS support user startup. Desktop and application integrations can be attached through MCP or custom providers; GUI tools need a provider running in the user's desktop session.
