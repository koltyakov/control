# Control Vault

[Installation](installation.md) · [CLI and MCP authority](delegation.md) · [Tunnels](tunnels.md)

Vault makes access to Control on an orchestrator opt-in. One approval lets one connected caller issue supported commands across the account's fleet. The default is 24 hours from approval. Workers still enforce their own access rules. A Vault grant cannot authorize another fleet or confer gateway administration.

To lock worker access from the gateway, use the [console's fleet and gateway controls](console.md#lock-worker-access). A console allowance can limit an access key to selected workers even when the local CLI or MCP connection is approved.

## Approve a caller

For a CLI command, approve the invocation in your own terminal:

```sh
control vault run -- control machines
control vault run --ttl 2h -- control system win-build
```

For an agent, leave the requesting session open. Its first fleet action returns `approval_required` and a short request code. The broker sends nothing to a worker and does not queue the action. Compare the code with the one in the conversation you intend to authorize, then run:

```sh
control vault grant R7K2-HX94
control vault list
control vault revoke GRANT_ID
```

`R7K2-HX94` and `GRANT_ID` are examples. A code expires after five minutes. It selects the pending connection; it is not a login token. The `grant` command needs the operator's passphrase in a local terminal. Read the fleet and scope before confirming. Never enter the passphrase in chat. After approval, tell the agent to continue if its host needs a new turn. Only a call explicitly refused before dispatch is safe to issue as new work. If dispatch was uncertain, reconcile the original operation.

A grant ends at its absolute deadline, explicit revocation, loss of the caller connection, or broker restart. It is not inherited by a shell command, a later connection, or a process with the same name. Accepted worker tasks still follow [normal task ownership and cancellation](delegation.md). Other approved clients sharing the account's stable owner can observe those tasks.

## Choose the MCP scope

The [OpenCode V2 adapter](../examples/vault-opencode/README.md) routes each conversation and subagent to a separate connection. A plain `control mcp` registration cannot identify a conversation in a shared editor. `control mcp --vault-process` explicitly selects **the entire stdio connection**, including every caller the host lets use it. Use that mode only when this is the intended scope. MCP arguments, labels, environment session IDs and the name of an agent are not proof of identity.

## Install the broker and activate the frontend

The broker is a standalone client. Run it under a dedicated, unprivileged OS account. The desktop account must not be able to read its state, debug its process, replace its executable, modify its service definition, or switch to the broker account without an administrator's approval. Do not enroll the broker account as a worker.

1. Install the trusted Control binary in an administrator-controlled location and create a dedicated non-administrative broker account. On Linux, create `/var/lib/control-vault`; on macOS, create `/Library/Application Support/Control Vault`. Make the broker account the directory owner, allow others to traverse it and read the public marker, and deny them write access. For example, with existing Linux user and group `control-vault`, an administrator can run `install -d -o control-vault -g control-vault -m 0755 /var/lib/control-vault`.
2. In the broker account, run `control vault provision --frontend YOUR_DESKTOP_USERNAME --gateway https://control.example.com`. Enter an account or execution-only orchestrator key and a new approval passphrase at the hidden prompts. Provisioning publishes the broker pin and approval salt in `vault.json` at the fixed location. It puts keys and task-owner state beneath a separate private subdirectory. Do not copy these to the frontend.
3. Run `control vault serve` under the same broker account. For restoration across restarts, configure the host service manager to run that command with the same identity, private storage and trusted binary. Starting it in a terminal provides no reboot supervision.
4. Under the ordinary desktop login, run `control vault enable`. It verifies the broker and binds this frontend to the published record. `control vault status` reports activation. Do not leave usable account or operator keys in the desktop environment, service profiles or backups accessible to ordinary applications.

An existing plaintext installation requires a cutover. Stop clients with live credentials. An enrolled local node's token can authorize its own HTTP API independently of the gateway login. A legacy tunnel service can retain an account key and endpoint token even after the main login changes. Activation checks the selected Control home, node config and credential environment variables. It cannot discover other profile directories, backups, shell histories, or a key already loaded by another process. Remove those copies or revoke their credentials separately.

### Clean installation on Windows

Use the default, local NTFS `ProgramData` directory. Windows Vault refuses to migrate a plaintext profile. Use a clean desktop profile and retire previously accessible credentials separately. The broker needs its own non-administrative Windows login and an administrator-installed executable under `Program Files`.

On a system where Windows is on `C:`, an administrator can prepare the directory below for an existing local account named `control-vault`. Start with a new directory; do not adopt an existing directory of unknown ownership.

```powershell
$root = 'C:\ProgramData\Control Vault'
if (Test-Path -LiteralPath $root) { throw 'Inspect the existing Vault root before continuing' }
New-Item -ItemType Directory -Path $root -ErrorAction Stop | Out-Null
icacls $root /inheritance:r /grant:r '*S-1-5-32-544:(OI)(CI)F' '*S-1-5-18:(OI)(CI)F' 'control-vault:(OI)(CI)F' '*S-1-5-11:(RX)'
if ($LASTEXITCODE) { throw 'Could not set Vault root permissions' }
icacls $root /setowner '*S-1-5-32-544'
if ($LASTEXITCODE) { throw 'Could not set Vault root owner' }
```

Sign in as the broker user and run `& 'C:\Program Files\Control\control.exe' vault provision --frontend 'DOMAIN\desktop-user' --gateway https://control.example.com`, substituting your account and gateway. Run `vault serve` under that same limited account. As the desktop user, run `vault enable` using the full path to the installed Control executable.

### Migrate an existing Unix frontend

Provision the broker first. Stop the broker and the previous login's retained tunnel service before importing state. From an operator terminal, pipe the everyday account's export **directly** to the broker account's import. The export contains credentials. Do not redirect it to a file or print it; only the import receipt is public. For example, with both OS accounts configured and a trusted `/usr/local/bin/control`:

```sh
/usr/local/bin/control vault migrate export |
  sudo -u control-vault /usr/local/bin/control vault migrate import > vault-receipt.json
```

Start `control vault serve` again, then run `control vault enable --receipt vault-receipt.json` as the everyday user. Activation checks the signed receipt against the exact files still present before removing them. Inspect any listed superseded credentials and revoke them before acknowledging revocation with `--revoked-old-keys`. A retained tunnel can move only to the same gateway and account credential. Its worker ID and expiry stay unchanged. Disable the old tunnel service's startup entry so only the new runtime restores definitions. If cutover reports a change or a busy profile, resolve it before proceeding.

To return to ordinary CLI access on Unix, stop the broker and disable its startup entry. As the frontend user, run `sudo -u control-vault /usr/local/bin/control vault rollback export | /usr/local/bin/control vault rollback restore`. This disables the broker profile and puts the credentials back in the desktop account. Restore refuses to overwrite conflicting files. Keep the protected copies until you verify the restored login, task owner and tunnels; only then run `control vault rollback purge` as the broker account. Purge refuses an active profile.

### Supported actions

Vault supports fleet discovery, diagnostics, worker calls and execution, tracked tasks, worker runbooks, artifact listing/export/delivery, and tunnel management. Unsupported operations fail locally. In particular, the broker does not open frontend file paths as the service account. Interactive shells, bulk file upload/download, the desktop clipboard, and gateway administration are not available through a session grant. Use the Vault MCP tool catalog for the exact set advertised to an agent.

## Tunnels and proxies

For unattended restore, [systemd](../examples/vault/control-vault.service) and [launchd](../examples/vault/com.control.vault.plist) examples run `vault serve` under `control-vault`. Set the account and absolute binary path to match provisioning. Install the service definition as an administrator; the frontend must not be able to change it. On Windows, configure Task Scheduler to launch the trusted binary with arguments `vault serve` under the dedicated account, with its normal profile loaded. The chosen start trigger determines when retained definitions become available after reboot.

Starting, listing, stopping or disposing a tunnel is a Control operation and needs an active local grant. Opening a connection **through** an established listener does not need that grant. A browser can make a new request through an existing proxy after its creator's local grant is revoked, even though that creator can no longer issue a stop command. Vault adds no listener password and does not shorten a tunnel's lifetime. A separate fleet lock still applies when a listener needs the worker to admit a new peer stream; it does not inspect bytes already flowing over an admitted stream.

Foreground listeners still belong to the requesting process. Retained definitions belong to the tunnel runtime in the broker account. The runtime can restore them without a session grant and without publishing an account key or a management token to the desktop account. A retained tunnel without a TTL remains until disposed. One with a TTL retains its original absolute expiry through restore and reconnect. Interrupted TCP connections are not replayed. See [tunnel lifetime and recovery](tunnels.md#lifetime-and-recovery).

## Security boundary

The broker guards its local command interface; it does not sandbox an approved agent, its remote commands or applications using forwarded ports. Someone who can modify the approved host or plugin, replace the operator's CLI, read its memory, capture the approval passphrase, or administer the machine is outside this boundary. A separately obtained valid gateway credential still authenticates at the gateway. The web console has its own login.
