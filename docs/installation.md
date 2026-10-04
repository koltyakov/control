# Installation and machine enrollment

[README](../README.md) · [Protocol](protocol.md) · [Managed updates](updates.md) · [Testing](testing.md)

## Install the host CLI

Linux or macOS:

```sh
curl -fsSL https://raw.githubusercontent.com/koltyakov/control/main/scripts/install.sh | sh
```

Windows PowerShell:

```powershell
irm https://raw.githubusercontent.com/koltyakov/control/main/scripts/install.ps1 | iex
```

The scripts download the native platform binary from GitHub Releases, verify its SHA-256, and install it for the current user. Defaults are `~/.local/bin/control` on Unix and `%LOCALAPPDATA%\Programs\control\control.exe` on Windows. The PowerShell script adds the directory to the current session and saved user PATH. Other applications may need a new login session to receive it. On Unix, add `~/.local/bin` to PATH if needed.

Set `CONTROL_INSTALL_DIR` to change the executable directory, `CONTROL_VERSION` to select a release tag, or `CONTROL_RELEASE_REPO` to select another repository. The scripts require published release assets. Before publishing a release, use `make build` and run `bin/control setup` from a checkout.

## Configure the main host once

```sh
control setup --gateway https://control.example.com --name main --client opencode
```

First obtain your account key from the gateway operator, who registers users with `control users create NAME`. Setup prompts for that key in an interactive terminal. Alternatively, set `CONTROL_USER_KEY`. Setup issues a common key for the host's node and saves the account credential separately for fleet management. A common `CONTROL_TOKEN` sets up execution and MCP access without management. The operator can use `CONTROL_SUPERUSER_KEY` to set up its own legacy-fleet host. See [users and private fleets](users.md).

Setup creates a node configuration, identity, and workspace, installs user startup, and waits until the node is registered and online. `--client` installs MCP plus the Control CLI skill. Restart the AI client to load the integration. Afterward, use the saved configuration:

```sh
control machines
control dashboard
control service status
control machines invites
```

The host permits observation and artifact delivery from its own fleet. Its default access rules prevent remote command execution, file reads, and network proxying under the account holding its management key. Local CLI/MCP requests retain the host's authority. Worker installations trust their own fleet by default. Other users cannot discover or connect to either hosts or workers.

`--listen 127.0.0.1:7332` changes the loopback API port. Setup refuses to overwrite an existing node configuration. Use `service start` to resume an existing installation and `install-mcp` or `install-skill` to add integrations.

## Add a machine

On the configured main host:

```sh
control machines add render-01 --platform windows/amd64 --ttl 15m
control machines add linux-worker --platform linux/arm64 --ttl 30m
control machines add mac-worker --platform darwin/arm64
```

Each command prints a copy-and-run command for the target machine. Windows uses PowerShell; Linux and macOS use Bash. Each OS supports `amd64` and `arm64`. The default lifetime is 15 minutes, with a range of one minute to 24 hours. `--json` returns the invitation ID, expiry, URL, and command.

Run the printed command on the target. It checks the platform, downloads the pinned binary, verifies its checksum, creates a local identity and credential, redeems the invitation, and starts the node. It reports success only after the node is registered and online. The machine then appears in `control machines` and the dashboard.

The gateway serves its own executable for a matching platform if no deployment bundle is available. Other platforms need a release or development bundle supplied by the gateway operator. Only the operator can trigger a release fetch or publish a full bundle with `make update`; ordinary invitations do not initiate a global software rollout. Invitations pin the selected asset even if a later update changes the current deployment.

Set `CONTROL_PUBLIC_URL` on the gateway if installation links must use a different public address, such as a reverse-proxy URL. Otherwise, links use the gateway address supplied by the host. Use HTTPS for a public gateway. Scripts contain neither the superuser key nor a shared pool token.

## Single use and recovery

The owning user can create invitations for their fleet. The gateway reserves the name within that fleet and persists the ticket hash in SQLite. A live ticket permits script, metadata, and binary downloads until expiry or redemption. Fetching a URL does not consume it.

The target generates its Ed25519 identity and a random credential locally. Signed redemption binds the credential hash to the inviting user, one identity, name, OS, and architecture. Concurrent redemption by a second identity fails. Existing identities cannot change fleet ownership. After redemption, script and download requests return HTTP 410.

The target saves pending enrollment before redeeming. If the response is lost, the same identity and credential can repeat the signed request, including after the original expiry. This recovers the same installation. Revocation prevents recovery. Resume with the existing executable and original URL:

```sh
control enroll --url https://control.example.com/install/TICKET
```

If startup fails, inspect `node.log` or the user service logs. Retry enrollment or use `control service start`. A new invitation does not overwrite an existing config or identity. Treat installation links as temporary bearer credentials and avoid recording them in reverse-proxy access logs.

## Manage registrations

```sh
control machines invites
control machines revoke INVITATION_ID
control machines forget NODE_ID
```

`invites` lists only your fleet's metadata and redeemed identity IDs, without ticket or credential hashes. `revoke` disables your invitation and its issued credential, then disconnects the gateway session. It does not terminate work or an established direct peer session.

`forget` removes an offline machine from the directory and revokes its invitation credential. Stop its node first. Local identity and work files remain on the machine. Shared common keys from manual installations are not revoked by forgetting a registration.

## MCP and skills

Install either integration independently, globally by default or in a project:

```sh
control install-mcp opencode
control install-skill opencode
control install-mcp claude --project .
control install-skill agents --project .
```

Supported names are `opencode`, `claude`, `cursor`, `copilot`, `codex`, `windsurf`, `antigravity`, and `agents`.

| Client | Global MCP config | Project MCP config |
| --- | --- | --- |
| OpenCode V2 | `~/.config/opencode/opencode.json` | `opencode.json` |
| Claude Code | `~/.claude.json` | `.mcp.json` |
| Cursor | `~/.cursor/mcp.json` | `.cursor/mcp.json` |
| GitHub Copilot | `~/.copilot/mcp-config.json` | `.vscode/mcp.json` |
| Codex | `~/.codex/config.toml` | `.codex/config.toml` |
| Windsurf | `~/.codeium/windsurf/mcp_config.json` | `.windsurf/mcp_config.json` |
| Antigravity | `~/.gemini/antigravity/mcp_config.json` | `.agents/mcp_config.json` |
| Generic agents | `~/.agents/mcp.json` | `.agents/mcp.json` |

OpenCode uses its native `mcp add` command when available. The file-editor fallback uses V2's `mcp.servers` structure and honors `XDG_CONFIG_HOME`. JSON/JSONC edits preserve unrelated configuration and comments. Codex TOML retains unrelated values but is reformatted. Before changing an existing file, the editor saves its original contents once as `.control-backup`.

Skills go into the client's `skills/control/SKILL.md` directory. Project OpenCode uses `.opencode/skills`, Copilot uses `.github/skills`, and Antigravity and generic clients use `.agents/skills`. Existing unmanaged skill files are not overwritten. Generic MCP configuration may need importing into the chosen client.

MCP entries contain an absolute executable path and `--config PATH mcp`. Credentials stay in the local profile. The [Control skill](../skills/control/SKILL.md) covers discovery, durable tasks, reconciliation after uncertain submission, and direct artifact delivery. MCP also works without the skill.

## Files and startup

Configuration defaults to the OS user config directory under `control`: `~/.config/control` on Linux, `~/Library/Application Support/control` on macOS, and `%APPDATA%\control` on Windows. Files include `node.json`, `state/identity.key`, `work/`, `node.log`, and administrative `admin.json`. Pending installations also have `pending-enrollment.json`, removed after verified startup.

`CONTROL_HOME` selects another installation directory. `CONTROL_CONFIG` or global `--config PATH` selects a node profile for CLI/MCP. `CONTROL_API`, `CONTROL_TOKEN`, and explicit API/token flags override profile settings. Management credentials are selected from `CONTROL_USER_KEY`, then `CONTROL_SUPERUSER_KEY`, then `CONTROL_TOKEN`, then the saved profile. Saved keys are not reused for a different gateway URL.

| Platform | User startup |
| --- | --- |
| Linux | `systemd --user`, `control-node.service` |
| macOS | LaunchAgent, `com.koltyakov.control` |
| Windows | Current user's `Run\ControlNode` registry entry |

User startup follows login/session lifecycle. Linux nodes that must outlive logout need the usual user-manager/linger configuration. Setup does not elevate privileges. In containers or environments without a user service manager, automatic mode reports the limitation and starts a detached process. `--service user` requires user startup. `--service process` selects process-only startup. `CONTROL_SERVICE_MODE` supplies the default for invitation scripts.

```sh
control service start
control service stop
control service uninstall
```

`stop` stops the local node. `uninstall` also removes user startup while retaining state. `scripts/uninstall.sh` and `scripts/uninstall.ps1` additionally remove the executable. Remove MCP `control` entries and installed skills manually when no longer needed.

## API reference

| Endpoint | Permission | Purpose |
| --- | --- | --- |
| `POST /v1/fleet/installations` | Fleet owner | JSON `name`, `os`, `arch`, optional `ttlSeconds`, and `gateway` |
| `GET /v1/fleet/installations` | Fleet owner | List own metadata |
| `DELETE /v1/fleet/installations/{id}` | Fleet owner | Revoke own invitation and credential |
| `DELETE /v1/fleet/nodes/{id}` | Fleet owner | Forget own offline registration |
| `GET /install/{ticket}` | Live unredeemed ticket | Platform script |
| `GET /install/{ticket}/info` | Live unredeemed ticket | Pinned metadata |
| `GET /install/{ticket}/binary` | Live unredeemed ticket | Stream executable |
| `POST /install/{ticket}/redeem` | Ticket and signed identity proof | Bind machine credential |

Each fleet can hold at most 4,096 invitation records, including redeemed installation credentials. Creating an invitation prunes that fleet's expired unredeemed records. Local service stop uses authenticated `POST /v1/service/stop` on the node API, with no peer RPC or MCP exposure. Legacy `/v1/admin/` aliases remain superuser-only and operate only on the legacy fleet.
