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

After installation, run `control update` to update the CLI in place, or `control upgrade` as an alias. `control update --version TAG` selects a release. This command needs no gateway login; running nodes use the gateway's separate managed rollout. Older binaries without this command need a current build or a one-time reinstall. See [CLI updates](updates.md#update-the-local-cli).

## Log in once

```sh
control login --gateway https://control.example.com
```

Enter your gateway account key at the hidden prompt. Login validates it through `/v1/auth` before atomically saving the URL and key in `admin.json` under the Control user configuration directory. On Unix, the file uses `0600` permissions. Failed login leaves existing credentials intact.

For automation, pipe a key into `control login --gateway URL --key-stdin`. Login also accepts `--api-key` or the existing credential environment variables. Subsequent `control machines`, `control machines add`, and `control dashboard` commands use the saved login without environment variables or repeated keys. These gateway commands need no local node. A common node key can read the directory and gateway status, but invitation creation requires an account key.

## Configure the main host once

```sh
control setup --name main --client opencode
```

Setup reuses the gateway and account key saved by login. For first-time setup without login, supply `--gateway` and enter the account key at the prompt, or set `CONTROL_USER_KEY`. The gateway operator registers users with `control users create NAME`. Setup issues a common key for the host's node and saves the account credential separately for fleet management. A common `CONTROL_TOKEN` sets up execution and MCP access without management. The operator can use `CONTROL_SUPERUSER_KEY` to set up its own legacy-fleet host. See [users and private fleets](users.md).

Setup creates a node configuration, identity, and workspace, installs background startup, and waits until the node is registered and online. On Windows, run setup in Administrator PowerShell to install an automatic Windows service. `--client` installs MCP plus the Control CLI skill. Restart the AI client to load the integration. Afterward, use the saved configuration:

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
control machines add render-01 --platform windows
control machines add linux-worker --platform linux
control machines add mac-worker --platform macos
```

Or press `a` in `control dashboard`, enter a name, and select the platform. The wizard creates the invitation and copies the command to the local clipboard. It keeps the command visible if copying fails and lets `c` retry without creating another invitation.

Each CLI command prints a copy-and-run command for the target machine. Windows uses PowerShell; Linux and macOS use Bash. The script detects `amd64` or `arm64` on the target, including Apple Silicon under Rosetta. The default lifetime is 15 minutes. Advanced CLI callers can still pin `OS/ARCH` and override `--ttl` from one minute to 24 hours. `darwin` remains an alias for `macos`. `--json` returns the invitation ID, expiry, URL, command, and pinned assets.

Run the printed command on the target, using Administrator PowerShell on Windows. It checks the platform, downloads the pinned binary, verifies its checksum, creates a local identity and credential, redeems the invitation, and starts the node. Windows checks service-installation privileges before redeeming the invitation. It reports success only after the node is registered and online. The machine then appears in `control machines` and the dashboard.

On an already registered machine, a new invitation replaces the enrollment in the selected local profile, even when the invitation uses a different name. The installer stops the existing agent, rotates its enrollment credential, updates its name, and restarts it. The gateway keeps one directory entry with the same identity and revokes previous invitation credentials. The profile's workspace, task/artifact state, providers, access rules, and local API port are retained. Stopping the agent interrupts running work. Replacement requires the same gateway and fleet; use the same OS user and `CONTROL_HOME` or `CONTROL_CONFIG` as the existing installation.

If the machine was unregistered first, run a new invitation against the same local profile. The installer detects the retired identity using a signed policy inspection and creates a fresh identity in a sibling `state-<random ID>` directory. It retains the workspace and profile settings, and leaves the old state directory intact. Old task/artifact metadata remains in that retired directory; the new agent starts with fresh state. A retired identity itself can never rejoin.

You can also create an invitation for the machine's current name. That invitation reserves the existing identity and can only be redeemed on that machine's profile. Creating it does not interrupt the existing agent. Only one unredeemed invitation can reserve a name at a time. Re-enrollment requires a gateway and installer binary that support replacement.

Architecture-detecting invitations require both amd64 and arm64 binaries for the selected OS in one published release or development bundle. The gateway and installer binaries must support architecture selection. Publish a current bundle with `make update` before using the wizard on an older deployment. The invitation pins both assets from that bundle, so a later update cannot change either download. Redemption permanently binds the selected architecture.

For an explicitly pinned `OS/ARCH` invitation, the gateway can serve its own matching executable if no bundle is available. Other platforms need an operator-supplied bundle. Only the operator can trigger a release fetch or publish a bundle; ordinary invitations do not initiate a global software rollout.

Set `CONTROL_PUBLIC_URL` on the gateway if installation links must use a different public address, such as a reverse-proxy URL. Otherwise, links use the gateway address supplied by the host. Use HTTPS for a public gateway. Scripts contain neither the superuser key nor a shared pool token.

## Single use and recovery

The owning user can create invitations for their fleet. The gateway reserves the name within that fleet and persists the ticket hash in SQLite. A live ticket permits script, metadata, and binary downloads until expiry or redemption. Fetching a URL does not consume it.

The target generates its Ed25519 identity and a random credential locally. Signed redemption binds the credential hash to the inviting user, one identity, name, OS, and architecture. Concurrent redemption by a second identity fails. Existing identities cannot change fleet ownership. After redemption, script and download requests return HTTP 410.

The target saves pending enrollment before redeeming. If the response is lost, the same identity and credential can repeat the signed request, including after the original expiry. This recovers the same installation. Revocation prevents recovery. Resume with the existing executable and original URL:

```sh
control enroll --url https://control.example.com/install/TICKET
```

If startup fails, inspect `node.log` or the service logs. Retry enrollment with the original URL, or use `control service start` once the new profile has been written. Replacement saves its pending profile before stopping the existing agent, and writes the active profile only after successful redemption. A lost response can be recovered with the same URL and identity. Finish a pending enrollment before using another invitation. Treat installation links as temporary bearer credentials and avoid recording them in reverse-proxy access logs.

## Manage registrations

```sh
control machines invites
control machines revoke INVITATION_ID
control machines disable worker
control machines enable worker
control machines unregister worker
```

`invites` lists only your fleet's metadata and redeemed identity IDs, without ticket or credential hashes. `revoke` disables your invitation and its issued credential, then disconnects the gateway session. It does not terminate work or an established direct peer session.

Names and stable IDs are accepted. In the dashboard, press `m`, select a machine, and choose `e` to enable, `d` to disable, or `u` to unregister. The unregister screen confirms which agent will stop.

Disable keeps the agent connected and observable, excludes it from `nodes.select`, and blocks new work at the destination, including through existing direct connections. Running and queued accepted tasks can finish. Their status, cancellation, lease cleanup, and idempotent submission recovery remain available. Enable restores admission after the agent acknowledges the new policy. `disabling` and `enabling` indicate a pending acknowledgement.

Unregister removes the directory entry, revokes its invitation credential, disconnects its gateway session, and permanently retires that identity. It works for online and offline machines, including older agents that never reported lifecycle support. A lifecycle-capable agent learns the stop policy through a signed identity query even after its credential is revoked, persists it, cancels work, and exits the service cleanly. A stopped or unreachable compatible agent applies the policy when it next contacts the gateway. Older agents cannot rejoin with the retired identity, but require a local stop if still running. Running a new installation invitation stops the old local agent and replaces it with a fresh identity. Existing direct peer sessions on older agents can continue until that local stop. Installed service entries and local files remain after unregistration. Shared common keys remain valid for other machines.

Policy changes are durable before the command returns and compatible agents check them at startup and every two seconds. Commands report a request, not a guarantee that an unreachable agent has applied it. Enable and disable require lifecycle support; upgrade and reconnect older agents before using those controls. Unregister requires no agent upgrade or reconnection. The legacy `machines forget ID` also remains available for offline registrations.

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

Configuration defaults to the OS user config directory under `control`: `~/.config/control` on Linux, `~/Library/Application Support/control` on macOS, and `%APPDATA%\control` on Windows. Files include `node.json`, `state/identity.key`, `work/`, `node.log`, and the saved gateway login in `admin.json`. Pending installations also have `pending-enrollment.json`, removed after verified startup.

`CONTROL_HOME` selects another installation directory. `CONTROL_CONFIG` or global `--config PATH` selects a node profile for CLI/MCP. `CONTROL_API`, `CONTROL_TOKEN`, and explicit API/token flags override profile settings. Gateway commands select their URL from `CONTROL_GATEWAY`, the selected node profile, or the saved login. Credentials use explicit global `--token`, then `CONTROL_USER_KEY`, `CONTROL_SUPERUSER_KEY`, `CONTROL_TOKEN`, a matching saved login, or the matching node profile token. Saved keys are not reused for a different gateway URL. Node API commands use the separate node token, never the saved account key.

| Platform | Background startup |
| --- | --- |
| Linux | `systemd --user`, `control-node.service` |
| macOS | LaunchAgent, `com.koltyakov.control` |
| Windows | Automatic Windows service, `ControlNode-<profile hash>`, running as LocalService |

Windows nodes start at boot, remain running after logout, and open no console window. SCM restarts a failed supervisor after 5 seconds, then 15 seconds, then 60 seconds on subsequent failures. Setup, enrollment, and service start/stop/uninstall require Administrator PowerShell. Status and ordinary CLI commands do not require elevation. Automatic mode fails if service installation fails; it does not fall back to a login entry or detached process. `--service user` remains a compatibility alias for the Windows service.

The Windows service runs under the restricted `NT AUTHORITY\LocalService` account. Setup grants that account read access to the executable and node config, and write access to node state, work files, and `node.log`. State and work directories must not contain the node or administration profiles. Setup does not grant access to `admin.json`. Providers use the service account's environment and permissions, so configure executable paths and credentials for that account. User desktop applications require a separate desktop-session provider.

Linux and macOS use per-user startup and follow the login/session lifecycle. Linux nodes that must outlive logout need the usual user-manager/linger configuration. On these platforms setup does not elevate privileges; automatic mode reports an unavailable user service manager and starts a detached process. `--service user` requires user startup. On all platforms, `--service process` explicitly selects process-only startup. `CONTROL_SERVICE_MODE` supplies the default for invitation scripts.

```sh
control service start
control service stop
control service uninstall
```

`stop` stops the local node. On Windows it asks SCM to stop the supervisor and waits for shutdown. `uninstall` also removes startup registration while retaining state. `scripts/uninstall.sh` and `scripts/uninstall.ps1` additionally remove the executable. Remove MCP `control` entries and installed skills manually when no longer needed.

### Migrate an older Windows installation

Stop the old node with `control service stop`, then install a current CLI. In Administrator PowerShell, run `control service start`. This installs the automatic service using the existing profile and identity, gracefully stops any remaining detached supervisor, and removes the matching legacy `Run\ControlNode` entry. Use global `--config PATH` if the existing profile is in a custom location. A managed node update alone does not replace the installed CLI or register an SCM service.

## API reference

| Endpoint | Permission | Purpose |
| --- | --- | --- |
| `POST /v1/fleet/installations` | Fleet owner | JSON `name`, `os`, `gateway`, optional `arch` and `ttlSeconds` |
| `GET /v1/fleet/installations` | Fleet owner | List own metadata |
| `DELETE /v1/fleet/installations/{id}` | Fleet owner | Revoke own invitation and credential |
| `PATCH /v1/fleet/nodes/{id}` | Fleet owner | JSON `disabled` boolean; return durable machine policy |
| `DELETE /v1/fleet/nodes/{id}?stop=true` | Fleet owner | Unregister own machine and retire its identity, including connected older agents |
| `DELETE /v1/fleet/nodes/{id}` | Fleet owner | Legacy removal of an offline registration |
| `POST /v1/node/state` | Fresh Ed25519 identity proof | Read and acknowledge only that identity's policy, including after credential revocation |
| `GET /install/{ticket}` | Live unredeemed ticket | Platform script |
| `GET /install/{ticket}/info` | Live unredeemed ticket | Pinned metadata; `?arch=amd64` or `arm64` selects `asset` |
| `GET /install/{ticket}/binary` | Live unredeemed ticket | Stream executable; architecture-detecting invitations require `?arch=amd64` or `arm64` |
| `POST /install/{ticket}/redeem` | Ticket and signed identity proof | Bind machine credential and selected architecture |

API OS names are `darwin`, `windows`, and `linux`. Omitted `arch` pins both supported architectures in `assets`; `asset` holds the first candidate until selected by `/info?arch=...` or redemption. Architecture-detecting redemption includes `arch` in both the request and signed proof. Recovery must repeat the same architecture, identity, and credential. Legacy pinned invitations retain their original proof format when `arch` is omitted.

An invitation for an existing name includes `replaceId`, which restricts redemption to that identity. A signed redemption from an existing identity can also adopt a new, available name within its own fleet. The gateway commits the rename, new credential, and revocation of previous installation credentials in one transaction. Installed identities must register using their current bound installation credential. Retired identities cannot be reused.

Installers query `/v1/node/state` with signed `inspectOnly: true` before replacing an existing profile. This reads the policy without acknowledging it or advertising lifecycle support on behalf of the old agent. Upgrade the gateway before using installers that send this proof field.

Each fleet can hold at most 4,096 invitation records, including redeemed installation credentials. Creating an invitation prunes that fleet's expired unredeemed records. Local service stop uses authenticated `POST /v1/service/stop` on the node API, with no peer RPC or MCP exposure. Legacy `/v1/admin/` aliases remain superuser-only and operate only on the legacy fleet.
