# Install a gateway on a Linux VPS

[README](../README.md) · [Installation](installation.md) · [Users](users.md) · [Managed updates](updates.md)

This example uses an Ubuntu or Debian VPS with systemd, a dedicated unprivileged service account, and Caddy for HTTPS. You need sudo access, a domain such as `control.example.com`, and a published Control release. Replace the domain below with your own. Nothing here installs a worker on the VPS.

To use expose instead of Caddy, follow [install Control behind expose](expose.md).

## 1. Prepare DNS and ports

Point your domain's A record at the VPS. Add an AAAA record only if its IPv6 address is reachable. Allow inbound TCP 80 and 443 in both the VPS firewall and your provider's security rules. Keep SSH available to administrators. Do not expose gateway port 7330 or node API port 7331.

The gateway needs outbound HTTPS for release downloads and DNS resolution. It does not need an inbound UDP listener. Direct WebRTC traffic flows between peers; restricted peers can use the gateway's HTTPS relay. Configure your own STUN server through `CONTROL_STUN_SERVERS` if needed. No public STUN provider is contacted by default.

## 2. Install the binary

```sh
sudo apt-get update
sudo apt-get install -y curl ca-certificates openssl
curl -fsSL https://raw.githubusercontent.com/koltyakov/control/main/scripts/install.sh | sudo env CONTROL_INSTALL_DIR=/usr/local/bin sh
/usr/local/bin/control version
```

The installer detects amd64/arm64, checks the release checksum, and installs `/usr/local/bin/control`. It also installs the bundled OpenCode skill for the installing account, but the gateway does not need an AI client, MCP configuration, or `control setup`. If releases are not published yet, run `make release-local` in a checkout and copy the matching `dist/control_linux_amd64` or `dist/control_linux_arm64` to `/usr/local/bin/control` with executable permissions. `make build` alone produces a binary for the build host, which may not be Linux.

## 3. Create a service account and private configuration

```sh
sudo useradd --system --home-dir /var/lib/control-gateway --shell /usr/sbin/nologin control-gateway
sudo install -d -o control-gateway -g control-gateway -m 0700 /var/lib/control-gateway
sudo install -d -m 0700 /etc/control-gateway
sudo sh -c 'umask 077; printf "CONTROL_SUPERUSER_KEY=%s\nCONTROL_PUBLIC_URL=https://control.example.com\n" "$(openssl rand -hex 32)" > /etc/control-gateway/environment'
```

For an existing installation, reuse its account and files; do not regenerate the operator key. Use `sudoedit /etc/control-gateway/environment` to set the correct public URL. The environment file is root-readable only; systemd reads it before dropping privileges.

Keep the generated superuser key private. It authorizes account provisioning and executable updates across every fleet. Do not put it in worker profiles, project files, or shared shell history. Leave `CONTROL_TOKEN` unset for a new private-fleet deployment. Each user gets a separate account key later.

By default, the gateway retrieves published stable releases on startup and every 15 minutes. To disable this, add `CONTROL_RELEASE_REPO=` to the environment file and publish development bundles with operator-authorized `make update`. Invitations for other platforms require a selected bundle containing those platforms' binaries. See [release settings](updates.md#set-up-the-gateway).

## 4. Run it under systemd

Create `/etc/systemd/system/control-gateway.service` with `sudoedit`:

```ini
[Unit]
Description=Control gateway
Wants=network-online.target
After=network-online.target

[Service]
Type=simple
User=control-gateway
Group=control-gateway
WorkingDirectory=/var/lib/control-gateway
EnvironmentFile=/etc/control-gateway/environment
ExecStart=/usr/local/bin/control gateway --listen 127.0.0.1:7330 --data /var/lib/control-gateway
Restart=on-failure
RestartSec=5s
TimeoutStopSec=120s
UMask=0077
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=true
ReadWritePaths=/var/lib/control-gateway
PrivateTmp=true

[Install]
WantedBy=multi-user.target
```

```sh
sudo systemctl daemon-reload
sudo systemctl enable --now control-gateway
sudo systemctl status control-gateway
curl -fsS http://127.0.0.1:7330/healthz
```

The last command should print `ok`. The installed CLI is the stable supervisor; managed updates run verified versioned binaries from the service's state directory. That directory must allow execution. Logs are available through `sudo journalctl -u control-gateway -f`.

## 5. Enable public HTTPS

Install Caddy from your distribution or its official package repository. On distributions that provide it:

```sh
sudo apt-get install -y caddy
```

Add this block to `/etc/caddy/Caddyfile`, preserving unrelated sites:

```caddyfile
control.example.com {
    reverse_proxy 127.0.0.1:7330
}
```

```sh
sudo caddy validate --config /etc/caddy/Caddyfile
sudo systemctl enable --now caddy
sudo systemctl reload caddy
curl -fsS https://control.example.com/healthz
```

Caddy obtains and renews the certificate and forwards WebSocket upgrades. DNS and ports 80/443 must be reachable for certificate issuance. If using another reverse proxy, enable long-lived WebSocket connections and stream binary upload/download bodies without whole-file buffering. Permit request bodies up to 128 MiB. Alternatively, Control accepts `--tls-cert` and `--tls-key`, but certificate issuance/renewal then belongs to your deployment.

## 6. Provision the first fleet

On an administrator's machine with the [orchestrator CLI installed](installation.md#install-the-orchestrator-cli-and-ai-integrations):

```sh
control login --gateway https://control.example.com
control users create alice
```

At login's hidden prompt, enter the generated `CONTROL_SUPERUSER_KEY` from the private VPS environment file. Account creation prints Alice's account key once. Give Alice only that key, not the superuser key. Alice then follows the [orchestrator and worker setup](installation.md#install-the-orchestrator-cli-and-ai-integrations). To use Alice's fleet yourself, log in again with her key through `--key-stdin` or `--api-key`; repeating a plain login to the same URL otherwise revalidates the saved operator key. Prefer `--key-stdin` to avoid recording a key in shell history.

Use `control update status` to inspect the selected bundle and rollout. If automatic release retrieval is enabled but no selected bundle is available yet, an operator can run `control update check` before creating worker invitations.

## 7. Enable browser access

With the saved operator login, run `control console password` and enter a separate browser password at the hidden prompts. Open the gateway's HTTPS URL and sign in as `@operator`. To give Alice browser access, run `control console password alice`; she signs in as `alice`. Existing CLI keys are not browser passwords. The console also lets the operator provision fleet accounts and their browser passwords.

Keep `CONTROL_PUBLIC_URL` set to the exact HTTPS origin and preserve the Host header at the loopback proxy. No extra port, frontend build, or web service is needed. See [web console](console.md) for security, sessions, and supported actions.

## Metrics

Superuser `GET /v1/admin/metrics` returns gateway-wide diagnostics as JSON by default. Add `?format=prometheus`, or send an `Accept` header that prefers `text/plain` or `application/openmetrics-text`, to receive the classic Prometheus text format (`text/plain; version=0.0.4`). Prometheus and compatible scrapers accept this format even when they prefer OpenMetrics. There is no unauthenticated metrics endpoint or separate listener. Use the gateway's public HTTPS URL. Other credentials receive HTTP 403.

Only the superuser key can read the route. A scrape host that holds it can also administer the gateway, so keep the key in a file readable only by the scraper's service account:

```sh
sudo install -m 0400 -o prometheus -g prometheus /dev/stdin /etc/prometheus/control-superuser-key
```

Paste the key, press Enter, then Ctrl-D. Then add a scrape job:

```yaml
scrape_configs:
  - job_name: control-gateway
    scheme: https
    metrics_path: /v1/admin/metrics
    params:
      format: [prometheus]
    authorization:
      type: Bearer
      credentials_file: /etc/prometheus/control-superuser-key
    static_configs:
      - targets: ['control.example.com']
```

The format exports only aggregates for the whole gateway, the same data as the JSON response. The JSON response also holds only aggregates, so the text format leaves nothing out. No account, node, credential, or address appears as a label, so series count stays fixed regardless of fleet size:

| Metric | Type | Meaning |
| --- | --- | --- |
| `control_gateway_build_info{version,gateway_protocol,goos,goarch,goversion}` | gauge | Always 1; running build metadata |
| `control_gateway_start_time_seconds` | gauge | Gateway start time, Unix seconds |
| `control_gateway_routed_connections{kind="client"\|"node"}` | gauge | Registered connections currently routed |
| `control_gateway_admitted_connections`, `control_gateway_active_handshakes` | gauge | Admission reservations held, and those still in handshake |
| `control_gateway_queued_packet_bytes`, `control_gateway_queued_packet_peak_bytes` | gauge | Retained packet bytes, including writes in progress, and the high-water mark since startup |
| `control_gateway_queued_relay_bytes` | gauge | Queued relay data payload bytes |
| `control_gateway_limit_connections{scope}`, `control_gateway_limit_handshakes{scope}`, `control_gateway_limit_queued_packet_bytes{scope}` | gauge | Configured limits; `scope` is `gateway` or `account` |
| `control_gateway_limit_account_clients`, `control_gateway_limit_account_nodes`, `control_gateway_limit_account_new_identities_per_minute` | gauge | Configured per-account limits |
| `control_gateway_connects_total`, `control_gateway_disconnects_total` | counter | Node and client registrations and closures |
| `control_gateway_auth_failures_total` | counter | Requests and connections with no valid credential |
| `control_gateway_admission_rejects_total`, `control_gateway_new_identity_rejects_total`, `control_gateway_packet_queue_rejects_total` | counter | Admission, new-identity rate, and packet budget refusals |
| `control_gateway_packet_write_failures_total` | counter | Failed queued packet writes |
| `control_gateway_handshake_duration_seconds_sum`, `_count` | summary | Handshake time and attempts, including failures |
| `control_gateway_relay_packets_total`, `control_gateway_relay_bytes_total` | counter | Relay data packets and payload bytes forwarded |
| `control_gateway_relay_overflows_total`, `control_gateway_relay_dropped_packets_total` | counter | Relay sessions closed for a full destination queue, and dropped signaling packets |

Counters reset when the gateway restarts. Prometheus `rate()` and `increase()` handle resets, and `control_gateway_start_time_seconds` shows when one happened. The browser console keeps using JSON: its `/console/api/admin/metrics` proxy ignores the format parameter and `Accept`. See [gateway observation](protocol.md#gateway-observation) for negotiation details.

## Maintenance

- Follow [managed updates](updates.md) for release selection and idle-only rollout. `control update` replaces the local CLI, not the gateway's selected runtime.
- Back up the gateway database while it runs with `control gateway backup create`. Protect the environment file separately. See [back up and restore](#back-up-and-restore).
- Diagnose startup with `systemctl status` and `journalctl`. A permission error often means the service cannot write or execute files under its state directory.
- This is a single-gateway deployment. Do not share its state directory between gateway processes or place it under worker-accessible storage.

## Back up and restore

### Online backup

On the gateway host, run the backup as the gateway service account. The destination directory must already exist and be outside the state directory:

```sh
sudo install -d -o control-gateway -g control-gateway -m 0700 /var/backups/control-gateway
sudo -u control-gateway /usr/local/bin/control gateway backup create \
  /var/backups/control-gateway/gateway-$(date -u +%Y%m%dT%H%M%SZ).db --data /var/lib/control-gateway
```

The gateway keeps serving. The command prints JSON with the backup's `path`, `schemaVersion`, `bytes`, `sha256`, `createdAt`, and counts of accounts, machines, credentials, invitations, and identities. It never prints record contents.

The command opens the database through its own SQLite connection and runs `VACUUM INTO`. Under WAL, that read transaction sees exactly the transactions committed when it starts, so the backup holds every mutation the gateway had acknowledged by then and none it had not. It does not use the gateway's mutation lock, and it does not block gateway writers. While the snapshot runs, SQLite cannot checkpoint past it, so the WAL grows by whatever is committed meanwhile; normal checkpoints resume afterwards. On a developer laptop, a 30 MB database, with the audit log at its 100,000-event ceiling, took about 0.3 seconds, and account creations committed concurrently in under 1 ms at the 99th percentile. The only write is the audit event below: one short transaction with one fsync, which a concurrent gateway commit may wait for.

Safety rules:

- The output is never overwritten. An existing file fails the command and is left unchanged.
- The snapshot is written to a hidden `.control-backup-*` file in the destination directory with mode `0600`, synced, and checked with `PRAGMA integrity_check`, `PRAGMA foreign_key_check`, and the schema version. It is then published without replacement: by hard link on Unix, so the destination file system must support hard links, and by a non-replacing write-through move on Windows.
- Interrupting the command or any failure removes the temporary file. Only a crash or power loss can leave a `.control-backup-*` file; delete it.
- One backup runs at a time per state directory. The command holds `gateway-backup.lock` there, separate from the `gateway.lock` the serving gateway holds.
- On Unix, the command refuses to run unless its effective user owns `gateway.db`. A SQLite connection from another account, such as root, could create `-wal`/`-shm` files the gateway account cannot open.
- It refuses a database whose schema is newer than its executable, or older than schema version 10 from Control v0.14.0. The installed CLI can be older than the gateway's managed runtime; after a schema-changing update, update the CLI with `control update`, which does not touch the gateway runtime. Executables without this command do not recognize it.

Authority comes from filesystem access to the state directory, the same access an offline copy needs. There is no network route, the superuser key is not involved, and credential hashes never leave the host through Control. `control gateway backup` appears only in superuser help.

Each published backup is recorded in the operator's activity history as `gateway.backup` ("Backed up gateway database") with the backup's SHA-256, schema version, size, and the host account, plus the `sudo` caller when present. The destination path and database contents are not recorded. The event is written before publication, so a published backup always has one, and a failed publication can leave an event without a file. Anyone who can read the state directory can still copy it without an event, so treat the record as accountability for this command, not as access control.

Protect backups like the state directory. They contain credential digests, Argon2id browser password hashes, console session digests, invitations, identity ownership, and the activity log. Encrypt copies stored off the host.

### What a backup contains

A backup is the SQLite database: accounts, credential hashes, invitations, registrations, permanent identity and client bindings, machine policy and names, proxy dependencies, browser passwords and sessions, and the activity log.

It does not contain:

- Release storage under `updates/`. Executables are large and can be fetched again with `control update check` or pushed with `make update`.
- Rollout state in `updates/deployment.json`: the selected deployment, its progress and acknowledgements, and the [persisted release version floor](updates.md#publish-github-releases). A database snapshot and a separate copy of this file cannot be made consistent while the gateway runs. Restoring an old rollout state could reselect older software or lower the floor, so it is left out deliberately.
- The managed runtime selection (`runtime*.json`) and versioned gateway executables, which belong to the installed host.
- The environment file with `CONTROL_SUPERUSER_KEY` and any release token, TLS certificates, and reverse-proxy configuration, which live outside the state directory. Keep them in your own secret storage.

The gateway has no identity key of its own. Nodes trust it through HTTPS and authenticate with credentials whose digests are in the database.

To preserve rollout state and the release floor exactly, use the offline method instead: stop the gateway and copy its whole state directory, including release storage.

### Verify and restore

There is no online restore. Restore while the gateway is stopped:

1. Check the backup with the executable that will serve it: `control gateway backup verify FILE`. It opens the file read-only without creating SQLite sidecar files and prints the same summary. It refuses a corrupt file, a schema newer than that executable, or a schema older than version 10.
2. Stop the gateway: `sudo systemctl stop control-gateway`.
3. Move the current `gateway.db`, `gateway.db-wal`, and `gateway.db-shm` out of the state directory. Do not leave an old `-wal` or `-shm` next to the restored file: SQLite would apply it to the wrong database. Keep the moved files until the restored gateway is checked.
4. Copy the backup to `gateway.db` in the state directory, owned by the service account with mode `0600`.
5. Start the gateway and check `control users list`, `control machines`, and `control update status`.

On the same host, keep the existing `updates/` directory and runtime files; they are current, unlike the database. On a new host, start from an empty state directory containing only the restored `gateway.db`. The gateway then has no selected deployment: it retrieves the latest release when release polling is enabled, or waits for `make update`. The release floor starts again from that selection. Confirm the selected version with `control update status` before creating invitations.

After a restore:

- A schema from Control v0.14.0 or later migrates forward on startup as usual. Restore an older backup with v0.14.0 and start that gateway once before upgrading; see [database schema](users.md#database-schema). A newer schema is refused with "gateway database is newer than this executable", as for any database.
- Everything committed after the backup is lost. Accounts and keys created later no longer authenticate. Keys, accounts, and invitations revoked later are valid again: revoke them again. Renames, enable/disable policy, and unregistrations made later revert.
- Machines enrolled after the backup are unknown and cannot authenticate; enroll them again. Machines that existed at backup time reconnect with their existing identity and credential. CLI and MCP client sessions register again on their next connection.
- Browser passwords and sessions are as of the backup. Sessions created later require signing in again. Sessions signed out later are valid again until their original idle or absolute expiry, and passwords changed later revert. Reset affected passwords, which also ends their sessions.
- Activity after the backup is lost.
- Rollout progress is not in the backup. On the same host the existing rollout state continues; nodes report their versions again when they reconnect.
