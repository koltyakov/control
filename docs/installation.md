# Installation and machine enrollment

[README](../README.md) · [Protocol](protocol.md) · [Managed updates](updates.md) · [Testing](testing.md)

Use [orchestrator machine](terminology.md) for the coordinating host, worker for a node receiving work, and client, node, and gateway for the components. A main host is an installation convenience, not a separate runtime type.

## Install the host CLI

Linux or macOS:

```sh
curl -fsSL https://raw.githubusercontent.com/koltyakov/control/main/scripts/install.sh | sh
```

Windows, from PowerShell, CMD, or Bash:

```powershell
powershell.exe -NoProfile -c "irm https://raw.githubusercontent.com/koltyakov/control/main/scripts/install.ps1|iex"
```

The scripts download the native platform binary from GitHub Releases, verify its SHA-256, and install it for the current user. Defaults are `~/.local/bin/control` on Unix and `%LOCALAPPDATA%\Programs\control\control.exe` on Windows. The PowerShell script adds the directory to its own session and saved user PATH, not the calling shell. Open a new terminal after installation; other applications may need a new login session to receive it. On Unix, add `~/.local/bin` to PATH if needed.

The release scripts, `make install`, and `control install-self` also install the bundled Control skill at `~/.config/opencode/skills/control/SKILL.md`, or under `XDG_CONFIG_HOME` when set. OpenCode does not need to be installed yet. Reinstallation refreshes Control-managed skills and refuses to overwrite an unmanaged skill file. This needs no gateway login or local node and does not configure MCP. Restart OpenCode to load the skill; use `control install-mcp opencode` separately if needed.

Set `CONTROL_INSTALL_DIR` to change the executable directory, `CONTROL_VERSION` to select a release tag, or `CONTROL_RELEASE_REPO` to select another repository. The scripts require published release assets. Before publishing a release, use `make build` and run `bin/control setup` from a checkout.

After installation, run `control update` to update the CLI in place, or `control upgrade` as an alias. `control update --version TAG` selects a release. This command needs no gateway login; running nodes use the gateway's separate managed rollout. Older binaries without this command need a current build or a one-time reinstall. See [CLI updates](updates.md#update-the-local-cli).

## Log in once

```sh
control login --gateway https://control.example.com
```

Enter your gateway account key at the hidden prompt. Login validates it through `/v1/auth` before atomically saving the URL and key in `admin.json` under the Control user configuration directory. On Unix, the file uses `0600` permissions. Failed credential validation leaves existing credentials intact. Repeating login for the same gateway revalidates the saved key without prompting. Use `--api-key` or `--key-stdin` to select a replacement key.

A superuser login also persists gateway-scoped update authorization in `update-admin.json`. A later normal account login does not erase it. `make update` reuses this authorization without asking for a key; an ordinary account alone still lacks gateway-wide update permission. See [development updates](updates.md#push-from-a-development-machine).

For automation, pipe a key into `control login --gateway URL --key-stdin`. Login also accepts `--api-key` or the existing credential environment variables. Subsequent `control machines`, `control machines add`, and `control dashboard` commands use the saved login without environment variables or repeated keys. These gateway commands need no local node. A common node key can read the directory and gateway status, but invitation creation requires an account key.

## Standalone CLI and MCP

After `control login`, remote commands work without `control setup` or a running local service when the gateway and target nodes support client sessions:

```sh
control exec worker -- hostname
control call worker node.describe
control system worker
control mcp
```

Default CLI/MCP remote work uses the saved account login to open an outbound-only client session, even when a local node is running. Explicit `--api` or `CONTROL_API` selects only that node's local API, which cannot independently execute peer work. No authentication or application failure triggers a backend switch or replay. A redeemed installation key cannot open client sessions; unbound common-key clients permit discovery only. See [orchestrator authority and worker delegation](delegation.md).

The persistent owner identity is stored under `clients/<gateway-and-fleet-hash>/identity.key` in the Control configuration directory. It identifies the requesting client, not an enrolled machine. Each process generates a separate in-memory TLS key and signs its connection metadata with both keys. Client sessions have no execution API, capabilities, health reports, machine lifecycle, or managed binary updates. Optional TCP forward listeners run locally on the orchestrator, not as fleet execution providers. Clients never appear in `control machines`, dashboard machine counts, worker selection, or update participants. Immutable account ownership, client role, and transport-to-owner bindings are persisted. Routing metadata exists while connected and disappears when the process exits. Use `control update` for the CLI.

Upgrade the gateway and target nodes before using standalone mode. The gateway advertises `clientSessions` and `clientOwners` through `/v1/auth`, and compatible nodes advertise them in their directory records. Older gateways and nodes produce an upgrade error before work is sent; the CLI never falls back to enrolling a machine. New nodes remain compatible with older fleet-aware gateways. Gateway startup migrates the previous implementation's exact capability-free `cli-<identity-prefix>` registrations out of the machine catalog, preserving their identities and task ownership. See [client-session separation](decisions.md#d029-authenticated-client-sessions-are-not-fleet-machines), [concurrent owners](decisions.md#d030-concurrent-client-connections-with-stable-task-owners), and [database upgrades](users.md#persistence-and-upgrades).

Tasks and leases retain the same owner across client invocations, including concurrent CLI/MCP processes sharing this state directory. Keep it to recover them. The identity lock is held only during key loading or creation. The gateway allows up to 64 connected client processes per account. Previous node-owned tasks keep their ownership and can be inspected through the executing node's local API; they are not reassigned. Account clients can aggregate peer activity without a local node. Calls require a target except `nodes.list`, `nodes.select`, and `activities.pool`. Artifact downloads and TCP tunnels use the client peer too.

Use `control exec worker --detach --id job-001 --timeout 4h -- COMMAND ARG...` to return after durable acceptance. `control task logs worker job-001 --follow --offset 0` follows bounded logs until completion; stopping the follower or task wait leaves the task running. Use `control task cancel` to stop it. `control session` reports this process's role, stable owner ID, transport ID, and connections without adding a machine registration.

`control tunnel worker HOST:PORT --listen 127.0.0.1:PORT` stays open until cancelled. MCP exposes `control_forward_start`, `control_forward_list`, and `control_forward_stop`, with listeners owned by the MCP process rather than the individual tool call. `control_session` also lists that process's forwards. Forwards default to `127.0.0.1:0`; non-loopback listeners must be explicitly selected and expose the remote service to their local network. See [forwarding semantics and limits](protocol.md#orchestrator-sessions-and-forwards).

Use `control tunnel start worker HOST:PORT --listen 127.0.0.1:PORT --id NAME` for a retained tunnel that outlives the invoking client. Its first start installs a separate profile-scoped user tunnel service without enrolling a node. `tunnel list` inspects state; `tunnel dispose NAME` durably removes it. Optional `--ttl` preserves a fixed expiry across reconnects and restarts. User-manager availability and login requirements apply to automatic restoration. Explicit local API routing is unsupported. See [persistent tunnel startup, private credentials, and recovery](tunnels.md).

To reach a dev server on your orchestrator from a remote browser, use `control tunnel worker 127.0.0.1:3000 --reverse --listen 127.0.0.1:3000`. Open `http://localhost:3000` on `worker`. MCP uses the same `node`, `address`, and `listen` with `reverse: true`. This requires `tcp.listen` permission and a receiving node with reverse-listener support. If using a local node API, upgrade that node too. Standalone clients need no local node. Closing the requesting process closes the remote listener, but leaves your dev server running.

Compatible nodes stream followed logs and negotiate TCP half-close. Older nodes retain polling or raw full-close behavior. Upgrading the gateway and both peers also enables `peerChannels`, which reuses one WebRTC carrier for control, bulk, and interactive traffic. `control session` reports lane/session counts. See [fleet streaming](streaming.md) for compatibility, limits, and remaining gaps.

## Configure the main host once

```sh
control setup --name main --client opencode
```

Setup reuses the gateway and account key saved by login. For first-time setup without login, supply `--gateway` and enter the account key at the prompt, or set `CONTROL_USER_KEY`. The gateway operator registers users with `control users create NAME`. Setup issues a common key for the host's node and saves the account credential separately for fleet management and orchestrator execution. A common `CONTROL_TOKEN` can set up a worker, but cannot initiate remote execution through MCP. The operator can use `CONTROL_SUPERUSER_KEY` to set up its own legacy-fleet host. See [users and private fleets](users.md).

Setup creates a node configuration, identity, and workspace, installs background startup, and waits until the node is registered and online. On Windows, run setup in Administrator PowerShell for the default automatic system service, or use `--service user` for startup at your login under your normal user account. `--client` installs MCP plus the Control CLI skill. Restart the AI client to load the integration. Afterward, use the saved configuration:

```sh
control machines
control dashboard
control service status
control machines invites
```

New host profiles permit discovery from fleet members and execution from account-authenticated clients through the `@orchestrator` access selector. Worker installations also require account-client initiation or instruction-bound delegation. Local worker credentials never authorize independent peer execution. Existing restrictive host profiles need an explicit client-owner or `@orchestrator` rule before account clients can execute on that host. Other users cannot discover or connect to either hosts or workers. Keep account management credentials outside worker-accessible files; see [delegation and upgrade requirements](delegation.md).

`--listen 127.0.0.1:7332` changes the loopback API port. Setup refuses to overwrite an existing node configuration. Use `service start` to resume an existing installation and `install-mcp` or `install-skill` to add integrations.

## Add a machine

On the configured main host:

```sh
control machines add render-01 --platform windows
control machines add linux-worker --platform linux
control machines add mac-worker --platform macos
control machines add auto --platform windows
control machines add --platform linux
```

Or press `a` in `control dashboard`, enter a name, select the platform, and choose User context or System context. Enter `auto` or leave the name blank to use the target machine's hostname. The wizard creates the invitation and copies the command to the local clipboard. It keeps the command visible if copying fails and lets `c` retry without creating another invitation.

With `auto`, or an omitted CLI name, the installer reads `os.Hostname()` on the machine running the installation command. It does not use the orchestrator's hostname or OS username. The hostname is used unchanged and must satisfy the usual 1..63-character machine-name rules. The installer saves the resolved name in pending enrollment and signs it in the redemption proof. Retries keep that name even if the OS hostname changes. No name is reserved when an automatic invitation is created, so multiple automatic invitations can coexist. The gateway claims the resolved name transactionally at redemption; existing fleet names, client names, and explicit invitation reservations cannot be taken over. A collision fails without consuming the invitation, rather than appending a suffix. Use an explicit name if the hostname is invalid or already in use.

Automatic naming requires both a gateway and a pinned installer binary that support `autoName`. Generated automatic-name scripts pass `enroll --auto-name`, so older binaries reject the unknown option before modifying a local installation or consuming the invitation. Upgrade and publish compatible installers before using it. Explicit named invitations retain their existing proof format and replacement behavior.

Each CLI command prints a copy-and-run command for the target machine. Windows commands use `powershell.exe -NoProfile -c "irm 'URL'|iex"` and can run from PowerShell, CMD, or Bash on Windows. The explicit `.exe` avoids extensionless command resolution in Git Bash; `-NoProfile` prevents user profiles from changing the loader. `irm` is the alias for `Invoke-RestMethod`, which returns the gateway's plain-text script as one string. `iex` evaluates that complete string, preserving multi-line blocks. URLs containing shell metacharacters use PowerShell's `-EncodedCommand` instead to preserve the literal URL. Linux and macOS use Bash. The script detects `amd64` or `arm64` on the target, including Apple Silicon under Rosetta. The default lifetime is 15 minutes. Advanced CLI callers can still pin `OS/ARCH` and override `--ttl` from one minute to 24 hours. `darwin` remains an alias for `macos`. `--json` returns the invitation ID, expiry, URL, command, and pinned assets.

If an older Windows invitation command reports a missing closing brace at line 1 of a multi-line block, run the following directly in PowerShell using its installation URL. This avoids launching a nested shell and evaluates the whole downloaded script. Use a fresh invitation if the original has expired:

```powershell
irm 'YOUR_INSTALLATION_URL'|iex
```

Run the printed command on the target under the intended user account on Windows. It checks the platform, downloads the pinned binary, verifies its checksum, creates a local identity and credential, redeems the invitation, and starts the node. Windows invitation scripts without an explicit context default to user-login startup and pass `--service user` explicitly, including when replacing a profile that previously used system startup. Windows checks startup and migration privileges before redeeming the invitation. It reports success only after the node is registered and online. The machine then appears in `control machines` and the dashboard.

A new Windows user-login startup task needs no account password or elevation. Generated worker installers separately request UAC elevation for [firewall setup](#windows-firewall) before starting the node; user-mode nodes remain non-elevated. Switching an existing system service to user startup requires Administrator PowerShell under that same user account. The invitation's pinned binary must support user-login mode and the installer's `--firewall` enrollment option. Select System context in the wizard or use `machines add --service system` for boot-time startup. For invitations without an explicit context, setting `$env:CONTROL_SERVICE_MODE = 'auto'` before running the command in Administrator PowerShell also selects a system service; `user` or `process` environment selections are preserved. The script does not change that variable in the caller's session. Direct `control setup` retains its system-service default; direct `control enroll` without an override retains the saved startup mode.

On an already registered machine, a new invitation replaces the enrollment in the selected local profile, even when the invitation uses a different name. The installer stops the existing node, rotates its enrollment credential, updates its name, and restarts it. The gateway keeps one directory entry with the same identity and revokes previous invitation credentials. The profile's workspace, task/artifact state, providers, access rules, and local API port are retained. Stopping the node interrupts running work. Replacement requires the same gateway and fleet; use the same OS user and `CONTROL_HOME` or `CONTROL_CONFIG` as the existing installation.

If the machine was unregistered first, run a new invitation against the same local profile. The installer detects the retired identity using a signed policy inspection and creates a fresh identity in a sibling `state-<random ID>` directory. It retains the workspace and profile settings, and leaves the old state directory intact. Old task/artifact metadata remains in that retired directory; the new node starts with fresh state. A retired identity itself can never rejoin.

You can also create an invitation for the machine's current name. That invitation reserves the existing identity and can only be redeemed on that machine's profile. Creating it does not interrupt the existing node. Only one unredeemed invitation can reserve a name at a time. Re-enrollment requires a gateway and installer binary that support replacement.

Architecture-detecting invitations require both amd64 and arm64 binaries for the selected OS in one published release or development bundle. The gateway and installer binaries must support architecture selection. Publish a current bundle with `make update` before using the wizard on an older deployment. The invitation pins both assets from that bundle, so a later update cannot change either download. Redemption permanently binds the selected architecture.

For an explicitly pinned `OS/ARCH` invitation, the gateway can serve its own matching executable if no bundle is available. Other platforms need an operator-supplied bundle. Only the operator can trigger a release fetch or publish a bundle; ordinary invitations do not initiate a global software rollout.

Set `CONTROL_PUBLIC_URL` on the gateway if installation links must use a different public address, such as a reverse-proxy URL. Otherwise, links use the gateway address supplied by the host. Use HTTPS for a public gateway. Scripts contain neither the superuser key nor a shared pool token.

### Choose a startup context

The dashboard wizard defaults to User context. The CLI exposes the same choice:

```sh
control machines add auto --platform linux --service user
control machines add auto --platform linux --service system
```

User context uses the installing user's service manager, files, and application credentials. It can stop when that user logs out. On Linux, a `systemd --user` node can remain online through logout and start at boot if an administrator enables linger for that user:

```sh
sudo loginctl enable-linger "$USER"
```

This keeps the existing user identity and workspace and does not run the node as root. Reconnect and run `control service start --mode user` if needed. Linger does not preserve a desktop session or GUI access after logout.

System context installs boot-time startup independent of logins. On Linux it installs a profile-scoped systemd unit under `/etc/systemd/system`, with `multi-user.target`. On macOS it installs a profile-scoped LaunchDaemon under `/Library/LaunchDaemons` in launchd's system domain. Both run as root and require sudo. Generated commands pipe the installer into `sudo bash`, default the executable directory to `/usr/local/bin`, and use `/var/lib/control` on Linux or `/Library/Application Support/control` on macOS for the profile, state, and workspace. Commands and unrestricted providers therefore have root authority. Use this only for a trusted fleet. System startup has no access to your interactive desktop session.

Windows System context uses the existing automatic LocalService service and requires Administrator PowerShell. User context uses the installing user's login task. Windows retains its existing profile-preserving migration behavior.

Explicit invitation context takes precedence over `CONTROL_SERVICE_MODE`. Omitting `--service` in `machines add` keeps the previous defaults and environment overrides: Windows scripts select user startup, while Unix scripts reuse the saved mode or automatic per-user startup. Unix system profiles save `system` in `<node-config>.startup.json`, so subsequent service commands and direct re-enrollment retain it. For Linux service logs, use `journalctl -u control-node-<profile hash>.service`; macOS logs remain in the profile's `node.log`.

Unix system installation uses a separate root-owned profile. It does not migrate a user installation's identity or files automatically. Stop and uninstall the old user startup before installing a replacement to avoid competing on the local API port. A same-name invitation reserves the old identity and cannot be redeemed by a new root profile. Use linger to retain the existing Linux installation, or unregister the old machine before creating a new system installation for that name. Old files remain intact. To manage a generated system profile, supply its configuration path, for example `sudo control --config /var/lib/control/node.json service stop`.

Upgrade the gateway and publish installer binaries with system-mode support before using this option. The client rejects an older gateway that does not confirm `serviceMode`; it does not retry the invitation. Older Unix installers reject `--service system` before redeeming it.

## Single use and recovery

The owning user can create invitations for their fleet. For an explicit name, the gateway reserves that name within the fleet. Automatic invitations defer the name claim to redemption. The gateway persists the ticket hash in SQLite. A live ticket permits script, metadata, and binary downloads until expiry or redemption. Fetching a URL does not consume it.

The target generates its Ed25519 identity and a random credential locally. Signed redemption binds the credential hash to the inviting user, one identity, name, OS, and architecture. Concurrent redemption by a second identity fails. Existing identities cannot change fleet ownership. After redemption, script and download requests return HTTP 410.

The target saves pending enrollment before redeeming. If the response is lost, the same identity and credential can repeat the signed request, including after the original expiry. This recovers the same installation. Revocation prevents recovery. Resume with the existing executable and original URL:

```sh
control enroll --url https://control.example.com/install/TICKET
```

If startup fails, inspect `node.log` or the service logs. Retry enrollment with the original URL, or use `control service start` once the new profile has been written. Replacement saves its pending profile before stopping the existing node, and writes the active profile only after successful redemption. A lost response can be recovered with the same URL and identity.

A new valid invitation from the same gateway and fleet replaces an unfinished enrollment, including one whose ticket expired or was revoked. The installer validates the new invitation and pinned executable before replacing pending recovery state. It retains the profile settings and selected identity, including a fresh identity saved during replacement of a retired node. A new invitation rotates the credential; repeating the same invitation keeps the saved credential and resolved name. Invalid invitations leave the previous pending enrollment and active profile intact. This requires an installer binary with pending-replacement support; publish a current bundle and create a new invitation to use it.

Treat installation links as temporary bearer credentials and avoid recording them in reverse-proxy access logs.

## Manage registrations

```sh
control machines invites
control machines revoke INVITATION_ID
control machines rename worker render-01
control machines disable worker
control machines enable worker
control machines unregister worker
```

`invites` lists only your fleet's metadata and redeemed identity IDs, without ticket or credential hashes. `revoke` disables your invitation and its issued credential, then disconnects the gateway session. It does not terminate work or an established direct peer session.

Names and stable IDs are accepted. In the dashboard, press `m`, select a machine, and choose `r` to rename, `e` to enable, `d` to disable, or `u` to unregister. The unregister screen confirms which installation will be removed.

Rename sets a gateway-owned routing alias immediately, including while offline. Names must start with a letter or digit and contain 1..63 letters, digits, dots, hyphens, or underscores. Names already registered or reserved by an invitation or client identity in your fleet are rejected. The alias survives gateway and node restarts without editing the node's local configuration or rotating its credential. The identity, tasks, artifacts, and active streams remain unchanged. The former name is no longer a routing alias; update name-based access rules and scripts, or use stable IDs. Compatible peers invalidate cached names after a gateway notification; older peers may retain the former name until restarted. A new enrollment invitation replaces the alias with that invitation's name.

Disable keeps the node connected and observable, excludes it from `nodes.select`, and blocks new work at the destination, including through existing direct connections. Running and queued accepted tasks can finish. Their status, cancellation, lease cleanup, and idempotent submission recovery remain available. Enable restores admission after the node acknowledges the new policy. `disabling` and `enabling` indicate a pending acknowledgement.

Unregister removes the directory entry, revokes its invitation credential, disconnects its gateway session, and permanently retires that identity. It works for online and offline machines, including older nodes that never reported lifecycle support. A current installed node learns the policy through a signed identity query even after its credential is revoked, persists it, cancels work, and exits cleanly. An independent helper then removes that profile's startup registration and installed executable. A stopped or unreachable node does this when it next contacts the gateway. A previously saved retirement policy triggers cleanup even if the gateway is unavailable at the next startup.

Configuration, identities, task/artifact state, runtime state, work files, MCP entries, skills, and Windows firewall rules are retained. An executable shared with another installed profile is also retained. Cleanup waits for the node and supervisor to release their locks and rechecks the identity under the enrollment lock, so delayed cleanup cannot remove a replacement installation. Failures are recorded in `node.log`. Running a new invitation against the retained profile installs a fresh identity while preserving its workspace and settings. Shared common keys remain valid for other machines.

Installations created before self-uninstall support need a current installed CLI and one `control service start` to record their launcher and startup mode. Stop and start Windows system services from Administrator PowerShell to apply the profile-specific service SID and self-removal permissions to their running token. Updating only the managed child cannot grant these OS permissions. Older lifecycle-capable nodes only stop, and nodes without lifecycle support require a local stop if still running. Their existing direct peer sessions can continue until then. Manually launched nodes without an installation record stop without deleting their executable.

Policy changes are durable before the command returns and compatible nodes check them at startup and every two seconds. Commands report a request, not a guarantee that an unreachable node has applied it. Enable and disable require lifecycle support; upgrade and reconnect older nodes before using those controls. Unregister requires no node upgrade or reconnection. The legacy `machines forget ID` also remains available for offline registrations.

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

Configuration defaults to the OS user config directory under `control`: `~/.config/control` on Linux, `~/Library/Application Support/control` on macOS, and `%APPDATA%\control` on Windows. Files include `node.json`, `state/identity.key`, `work/`, `node.log`, and the saved gateway login in `admin.json`. Superuser login also saves separate operator authorization in `update-admin.json`, which remains available after a later fleet-account login. Only update administration uses that file; see [development updates](updates.md#push-from-a-development-machine). Keep both credential profiles outside node work and state directories. Pending installations also have `pending-enrollment.json`, removed after verified startup. Service startup records the launcher, profile, identity, and mode in `<dataDir>/installation.json` and registers that profile under `<binary>.profiles/`. Retirement cleanup copies its helper into `<dataDir>/uninstall-*`; these files remain with retained runtime state.

`CONTROL_HOME` selects another installation directory. `CONTROL_CONFIG` or global `--config PATH` selects a node profile for CLI/MCP. `CONTROL_API`, `CONTROL_TOKEN`, and explicit API/token flags override profile settings. Gateway commands select their URL from `CONTROL_GATEWAY`, the selected node profile, or the saved login. Credentials use explicit global `--token`, then `CONTROL_USER_KEY`, `CONTROL_SUPERUSER_KEY`, `CONTROL_TOKEN`, a matching saved login, or the matching node profile token. Saved keys are not reused for a different gateway URL. Node API commands use the separate node token, never the saved account key.

| Platform | Background startup |
| --- | --- |
| Linux | `systemd --user`, `control-node.service` |
| Linux, `system` | System systemd unit, `control-node-<profile hash>.service`, running as root |
| macOS | LaunchAgent, `com.koltyakov.control` |
| macOS, `system` | LaunchDaemon, `com.koltyakov.control.<profile hash>`, running as root |
| Windows, `auto` | Automatic Windows service, `ControlNode-<profile hash>`, running as LocalService |
| Windows, `user` | Login scheduled task, `ControlUser-<profile hash>`, running as the installing user |

Windows `auto` nodes start at boot and remain running after logout. SCM restarts a failed supervisor after 5 seconds, then 15 seconds, then 60 seconds on subsequent failures. System-service setup, enrollment, and service start/stop/uninstall require Administrator PowerShell. Status and ordinary CLI commands do not require elevation. Automatic mode fails if service installation fails; it does not fall back to login startup or a detached process.

The Windows service runs under the restricted `NT AUTHORITY\LocalService` account. Setup grants that account read access to the executable and node config, and write access to node state, work files, `node.log`, and the profile's installation lock. It enables a profile-specific service SID and grants that SID delete/query access to its own service, delete access to the launcher and its own binary-registration file, read access to the binary-registration directory, and write access to its coordination lock. This permits retirement cleanup without Administrator or SYSTEM execution, but does not grant control over unrelated services or files. State and work directories must not contain the node or administration profiles. Setup does not grant access to `admin.json`. Providers use the service account's environment and permissions, so configure executable paths and credentials for that account. User desktop applications require a separate desktop-session provider.

Windows `user` mode registers a scheduled task with an interactive token and limited privileges for the installing user's SID. It starts immediately and at that user's login, without storing an account password. The hidden launcher runs the existing supervisor and appends output to `node.log`; Task Scheduler retries failures up to three times at one-minute intervals. The node uses the user's filesystem permissions, profile, credentials, and desktop session. It is unavailable before login or after logout. Login/logout behavior and console visibility require a Windows runtime smoke test. Unrestricted providers now have that user's authority, so use this mode only for a trusted fleet and configure `allow` as needed.

GUI automation is separately opt-in through an `rpa` helper command. Windows requires interactive user startup, macOS requires Accessibility and Screen Recording permissions, and Linux coordinate input requires the user's X11 session. Setup does not install GUI dependencies or grant OS permissions. See [GUI automation installation and configuration](rpa.md#install-the-helper).

For application passwords and HTTP tokens, enter [worker-local secrets](secrets.md) on the destination machine with `control secrets set NAME`. AI calls use only names. Storage must remain outside `workDir`; Windows uses the profile directory's inherited ACLs. These credentials are separate from the gateway login and are not synchronized across machines.

Windows saves the chosen `auto` or `user` mode in `<node-config>.startup.json`. `control service start|stop|uninstall` and re-enrollment reuse it unless `--mode`, `--service`, or `CONTROL_SERVICE_MODE` overrides it. Restore a missing node configuration before uninstalling user startup so the local API can stop the node gracefully.

Linux and macOS automatic/user modes use per-user startup and follow the login/session lifecycle. Linux user nodes that must outlive logout need user-manager/linger configuration. Automatic mode reports an unavailable user service manager and starts a detached process; `--service user` requires user startup. Explicit `--service system` requires root and never falls back to a detached process or user startup. On all platforms, `--service process` explicitly selects process-only startup. `CONTROL_SERVICE_MODE` supplies the default for invitation scripts without an explicit context.

```sh
control service start
control service stop
control service uninstall
```

`stop` stops the local node. Windows system mode uses SCM; user mode uses the local API and completes the scheduled-task wrapper after the supervisor stops. `uninstall` also removes the selected startup registration while retaining state. `scripts/uninstall.sh` and `scripts/uninstall.ps1` additionally remove the executable. Remove MCP `control` entries and installed skills manually when no longer needed.

### Windows firewall

Generated Windows worker scripts pass `enroll --firewall`. After verified installation and redemption, enrollment adds or refreshes profile-scoped firewall rules before starting the node. If not already elevated, it requests UAC for a separate `service firewall` invocation with absolute executable and profile paths. Approving that prompt does not elevate enrollment or the user-login node. Cancellation or firewall errors fail installation visibly and retain pending enrollment for recovery. Publish a bundle with `--firewall` support before serving the new scripts; an older installer rejects the flag before redeeming the invitation.

For an existing installation or a directly configured host, use the installed CLI explicitly to avoid Windows' unrelated `System32\control.exe`:

```powershell
& "$env:LOCALAPPDATA\Programs\control\control.exe" service firewall
```

Use global `--config PATH` for a custom profile and the corresponding executable path for a custom installation. This operation requests UAC if needed and does not restart the node or change startup mode.

Each profile has six persistent allow rules: inbound UDP, outbound UDP, and outbound TCP to the configured gateway port, for both the installed launcher and `<dataDir>\runtime\control.exe`. UDP uses dynamically selected ICE ports, so the rules restrict executable paths rather than opening a machine-wide port range. Rules apply to Domain, Private, and Public profiles and block edge traversal. No inbound TCP rule is added, port 7331 remains loopback-only, and no firewall profile or unrelated rule is disabled or removed. Explicit block rules and organization policy can still prevent traffic; relay fallback remains available when direct WebRTC is blocked.

Windows supervisors copy checksum-verified binaries to the stable runtime path only between child runs, preserving versioned sources and runtime-selection records. Managed updates therefore keep the same firewall-covered execution path without new UAC prompts. An old supervisor must be replaced and restarted once to use this behavior; updating only its managed child does not change the launcher. `service firewall` does not remove its rules on service stop or uninstall, so restored installations can reuse them. Remove the profile's `ControlFirewall-<profile hash>-*` rules separately when permanently removing the installation.

### Migrate an older Windows installation

Stop the old node with `control service stop`, then install a current CLI. In Administrator PowerShell, run `control service start`. This installs the automatic service using the existing profile and identity, gracefully stops any remaining detached supervisor, and removes the matching legacy `Run\ControlNode` entry. Use global `--config PATH` if the existing profile is in a custom location. A managed node update alone does not replace the installed CLI or register an SCM service.

### Switch Windows to user-login startup

Install a CLI built with user-login support. In Administrator PowerShell under the intended user account, run:

```powershell
control service start --mode user
```

This registers login startup, gracefully stops and disables the old system service, and starts the node under the user's non-elevated interactive token. The profile, identity, workspace, and task/artifact state are retained. Running work is interrupted by this explicit migration, so migrate while idle. The disabled SCM registration remains available for rollback. Later starts and stops need no elevation. To return to boot-time LocalService startup, run `control service start --mode auto` in Administrator PowerShell. That removes this user's login task before starting SCM. Do not run either migration remotely from LocalService or elevate as a different user.

## API reference

| Endpoint | Permission | Purpose |
| --- | --- | --- |
| `POST /v1/fleet/installations` | Fleet owner | JSON `name` or `autoName: true` with an empty `name`, plus `os`, `gateway`, optional `arch`, `ttlSeconds`, and `serviceMode: "user"` or `"system"` |
| `GET /v1/fleet/installations` | Fleet owner | List own metadata |
| `DELETE /v1/fleet/installations/{id}` | Fleet owner | Revoke own invitation and credential |
| `PATCH /v1/fleet/nodes/{id}` | Fleet owner | JSON `disabled` boolean or `name` string, not both; return durable machine policy |
| `DELETE /v1/fleet/nodes/{id}?stop=true` | Fleet owner | Unregister own machine and retire its identity, including connected older nodes |
| `DELETE /v1/fleet/nodes/{id}` | Fleet owner | Legacy removal of an offline registration |
| `POST /v1/node/state` | Fresh Ed25519 identity proof | Read and acknowledge only that identity's policy, including after credential revocation |
| `GET /install/{ticket}` | Live unredeemed ticket | Platform script |
| `GET /install/{ticket}/info` | Live unredeemed ticket | Pinned metadata; `?arch=amd64` or `arm64` selects `asset` |
| `GET /install/{ticket}/binary` | Live unredeemed ticket | Stream executable; architecture-detecting invitations require `?arch=amd64` or `arm64` |
| `POST /install/{ticket}/redeem` | Ticket and signed identity proof | Bind machine credential and selected architecture |

API OS names are `darwin`, `windows`, and `linux`. Omitted `arch` pins both supported architectures in `assets`; `asset` holds the first candidate until selected by `/info?arch=...` or redemption. Architecture-detecting redemption includes `arch` in both the request and signed proof. Recovery must repeat the same architecture, identity, and credential. Legacy pinned invitations retain their original proof format when `arch` is omitted.

Automatic invitation metadata has `autoName: true` and initially an empty `name`. Redemption adds the resolved hostname as `name`; `enrollment.Message` includes this optional lowercase `name` field in the signed JSON. Fixed-name redemption omits it, retaining legacy signature bytes. The gateway persists the selected name in invitation metadata and binds registration credentials to it. Recovery must repeat the same signed name as well as architecture, identity, and credential.

An invitation for an existing name includes `replaceId`, which restricts redemption to that identity. A signed redemption from an existing identity can also adopt a new, available name within its own fleet. The gateway commits the rename, new credential, and revocation of previous installation credentials in one transaction. Installed identities must register using their current bound installation credential. Retired identities cannot be reused.

Installers query `/v1/node/state` with signed `inspectOnly: true` before replacing an existing profile. This reads the policy without acknowledging it or advertising lifecycle support on behalf of the old node. Upgrade the gateway before using installers that send this proof field.

Each fleet can hold at most 4,096 invitation records, including redeemed installation credentials. Creating an invitation prunes that fleet's expired unredeemed records. Local service stop uses authenticated `POST /v1/service/stop` on the node API, with no peer RPC or MCP exposure. Legacy `/v1/admin/` aliases remain superuser-only and operate only on the legacy fleet.
