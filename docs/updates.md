# Managed updates

[README](../README.md) · [Architecture](architecture.md) · [Protocol](protocol.md) · [Testing](testing.md)

The gateway accepts development bundles and can poll GitHub Releases. It distributes the selected platform binary across all user fleets, waits for idle nodes, restarts the nodes, then restarts itself. Offline nodes receive the current deployment when they reconnect. Publication and global rollout status are restricted to the gateway superuser; user-account and common keys cannot access them.

CLI/MCP client sessions are not managed fleet nodes. Their platforms do not constrain the release bundle, they do not receive update commands, and they are excluded from rollout participants and the gateway's machine-reservation count. Work accepted on their destination nodes is still included in those nodes' idle checks. Upgrade the CLI with `control update`. The gateway and target nodes must both support client sessions before standalone execution can use them; see [standalone usage](installation.md#standalone-cli-and-mcp) and [terminology](terminology.md).

## Update the local CLI

```sh
control update
control upgrade
control update --version v0.2.0
```

Bare `update` downloads the latest stable GitHub release and replaces the invoked executable. `upgrade` is an alias. It requires write access to the executable's directory, but no gateway credential or node profile. `--version TAG` selects a specific release, including an older version; `CONTROL_VERSION` supplies the default tag. `CONTROL_RELEASE_REPO=owner/repository` overrides the embedded repository. An empty repository produces a configuration error. CLI self-updates use public GitHub release downloads.

The command validates the release manifest and selects the current OS and architecture. If the running CLI already has that version, it reports that the release is already installed without downloading or replacing the binary, even if the release checksum differs. An identical installed checksum also skips replacement. Otherwise, it downloads the tag-pinned binary, verifies its size and SHA-256, and checks its `version --json` response before replacement. Failed downloads or validation leave the executable intact. Symlinks resolve to their target, and concurrent updates to the same executable are rejected.

On Windows, the updater preserves the executable's access grants and renames the old executable before installing the new one, allowing running CLI and service-launcher processes to finish. A locked old executable can remain as `previous.exe` in a `.control-update-*` directory beside the CLI. Remove that directory after those processes exit. On Linux and macOS, replacement uses an atomic rename and preserves executable permissions.

This updates future CLI/MCP invocations. Managed node and gateway versions still follow the gateway's idle rollout and persisted runtime selection. Existing binaries that report `unknown command "update"` need a current build or a one-time reinstall using the [installation scripts](installation.md#install-the-host-cli).

## Set up the gateway

The superuser key must be at least 32 characters. For a shared gateway, provision separate user accounts instead of distributing a common bootstrap token. An optional `CONTROL_TOKEN` enables the legacy operator fleet and must differ from the superuser key.

```sh
export CONTROL_SUPERUSER_KEY='your-separate-random-superuser-key-at-least-32-characters'
bin/control gateway --listen 0.0.0.0:7330 --data /var/lib/control-gateway
```

The gateway checks the latest stable, published GitHub release on startup and every 15 minutes using the repository embedded in the binary. `CONTROL_RELEASE_REPO` overrides that repository when present; an explicitly empty value disables release polling. Set `CONTROL_RELEASE_INTERVAL=30m` to change the interval. `CONTROL_RELEASE_TOKEN` supplies a GitHub token for private release downloads or API rate limits. Use HTTPS for remote gateway connections.

`make build`, `make bundle`, and `make update` embed the GitHub repository detected from `git remote get-url origin`, accepting SSH and HTTPS remotes. Without Git metadata, the builder uses `GITHUB_REPOSITORY` or falls back to `koltyakov/control`. Pass `--repository owner/repository` to `go run ./cmd/control-bundle` to select the embedded value explicitly. This build setting never overrides `CONTROL_RELEASE_REPO` at runtime.

The builder also injects a UTC `buildTime`. All binaries in a bundle share the manifest's `createdAt` timestamp. `control version --json` and update status expose `buildTime` and `releaseRepo`. Extended build metadata stays out of signed machine registration. A direct `go build` reports `dev`, retains the source repository default, and has no injected build timestamp. MCP client and server identification use the same embedded software version.

Configure nodes with a common key for their owning user or a redeemed installation credential. Host setup retains the user's fleet-management credential separately. The node command removes `CONTROL_USER_KEY`, `CONTROL_SUPERUSER_KEY`, and `CONTROL_RELEASE_TOKEN` from its environment before launching its service and providers.

Managed updates require compatible Control versions on both the gateway and nodes. When upgrading from the pre-isolation release, pause automatic polling and upgrade the gateway first so its SQLite migration and fleet-aware authentication are available before new nodes connect. After that one-time migration, nodes-first managed rollouts apply normally. Without `CONTROL_SUPERUSER_KEY`, user provisioning and executable update administration are disabled; existing user-account fleet management remains available.

## Push from a development machine

With persisted gateway superuser authorization, run:

```sh
make update
make update-status
```

`control login` validates the key through `/v1/auth` and persists it in `admin.json`. A superuser login also saves update authorization in `update-admin.json`. Later normal fleet logins do not overwrite that update authorization, so the dashboard can use your account while updates retain operator permission. Repeating `control login` for the same gateway revalidates the saved key instead of asking for it again. Supply `--api-key` or `--key-stdin` to replace the login. Both credential files use atomic private-file storage, with `0600` permissions on Unix. Failed credential validation leaves both profiles unchanged.

When replacing an older CLI's saved superuser login with a normal fleet login on the same gateway, login verifies and preserves the previous operator key in `update-admin.json` before overwriting the regular profile. It does not infer operator authority from an ordinary account key.

`make update` builds the native CLI, then runs `update authorize --check` before building the platform bundle. This check reuses saved authorization and never prompts or saves credentials. If no selected credential has superuser permission, the target stops with a permission error. Authentication and connection failures also stop it before bundle builds, including under `make -j`. A normal account login by itself does not authorize gateway-wide updates.

The target then builds the local checkout's bundle for Linux, macOS, and Windows on amd64 and arm64. It uploads the binaries and publishes the manifest without fetching a GitHub release. The rollout covers every enrolled node across user fleets, including nodes on orchestrator machines and all workers, then the gateway. Offline nodes catch up when they reconnect. Standalone CLI/MCP installations are not enrolled nodes and are not updated by this rollout. A successful push means the gateway has accepted the rollout; use `make update-status` to follow staging, waiting for idle, installation, and completion.

Binary uploads stream from an open file as `application/octet-stream`, with an exact `Content-Length`. The gateway streams each upload into a temporary file while hashing it, then publishes the verified file. Only the small manifest uses JSON. A reverse proxy or tunnel must forward request and response bodies without whole-file buffering and permit binaries up to Control's 128 MiB limit. Streaming does not bypass a proxy's total-body limit; HTTP 413 stops the push before manifest publication. Stock expose v0.28.2 has a hard-coded 10 MiB tunnel-body limit, which is too small for Control binaries. Use an expose build with uncapped streamed tunnel bodies; bounded buffers and frame limits still apply. Its static-site `EXPOSE_PUBLISH_MAX_BYTES` setting does not change the tunnel limit.

Update administration selects an explicit global `--token`, then `CONTROL_SUPERUSER_KEY`, then a matching saved update authorization. Otherwise, the usual gateway credential selection applies, including a matching saved login. Saved credentials are never reused for a different gateway. Dashboard, execution, key-management, and user-management commands do not use `update-admin.json`. A normal account or common key still cannot publish updates. Missing credentials, rejected authentication, and gateway connection failures are reported directly rather than as `unknown command "update"`.

Environment variables remain optional for automation. As an explicit alternative to login, `bin/control update authorize` can securely prompt for an operator key and save it without replacing the regular fleet login. `update authorize --key-stdin` reads a replacement key for validation and storage; it cannot be combined with `--check`, global `--token`, or `CONTROL_SUPERUSER_KEY`. This explicit authorization command does not fall back to prompting after a rejected credential or a connection failure. Remove `update-admin.json` to forget the separate update authorization. A superuser key still saved in `admin.json` remains usable for updates. Keep both files out of node workspaces, state directories, and other locations accessible to unrestricted providers.

For a Linux-only pool, build just its required platforms:

```sh
make update PLATFORMS=linux/amd64,linux/arm64
```

The bundle must include the gateway platform and every registered node's platform, including offline nodes. Versions follow `git describe --tags --always --dirty`: an exact tag such as `v0.2.0`, a post-tag version such as `v0.2.0-3-gabc1234`, or a short commit hash before the first tag. Tracked local changes append `-dirty`; unavailable Git metadata falls back to `dev`. This applies to Make targets and direct `go run ./cmd/control-bundle` builds. Set `VERSION=dev-my-change` or pass `--version` to name one explicitly. Build timestamps remain separate metadata, so rebuilding the same checkout keeps its version label while producing new build metadata and checksums.

Publishing the currently selected version returns the existing deployment without starting another rollout or changing its pinned assets, timestamps, phase, or participants. Each node and the gateway also skip binary staging and restart when their running version matches the selected version, regardless of checksum differences. Older nodes still receive the selected bundle. To deploy changed code under a reused `-dirty` or `dev` label, select a new version explicitly. Build and upload steps can still run for an unchanged version; installation and restart are skipped.

The equivalent commands are:

```sh
make build
bin/control update authorize --check
make bundle
bin/control update push dist
bin/control update status
bin/control update check
```

`update push DIR` prints a short summary with the gateway's accepted version, platform count, deployment ID, and current phase. It does not wait for rollout completion. Use `control update push DIR --json` for the full deployment JSON, including the manifest and asset checksums. A repeated same-version push summarizes the retained deployment returned by the gateway.

`update check` fetches the latest release from the repository configured on the gateway. It uses the same validation and rollout as periodic release checks.

## Publish GitHub releases

Commit the release changes, then create and push a version tag:

```sh
git tag v0.2.0
git push origin v0.2.0
```

The release workflow builds with that exact tag as the version. Its verification job checks dependencies, runs vet and race tests, and exercises both Compose transport modes before publication. It supports `v*.*.*` tags and manual dispatch from the matching tag:

```sh
gh workflow run release.yml --ref v0.2.0 -f tag=v0.2.0
```

Dispatch rejects a branch ref or a mismatched tag. Tags with a prerelease suffix, such as `v0.2.0-rc.1`, publish as GitHub prereleases. Release runs for the same tag are serialized. For a manual release build:

```sh
make release-local VERSION=v0.2.0
```

Attach `dist/control-manifest.json`, `dist/checksums.txt`, and all six `dist/control_OS_ARCH` binaries to the GitHub release. Windows binaries have an `.exe` suffix. The gateway expects raw binaries, not archives. The manifest records each binary's OS, architecture, filename, byte size, and SHA-256 digest. Its version must equal the release tag. The [release workflow](../.github/workflows/release.yml) uses Control's bundle builder, checks its output, signs the manifest and checksums with keyless cosign using GitHub OIDC, then publishes the binaries, metadata, and `.sigstore.json` signature bundles with generated release notes. Write and OIDC permissions are limited to the publication job. The checksum file also supports the [shell installer](installation.md).

`make release-check` validates an existing `DIST_DIR`, default `dist`, without rebuilding or publishing. It checks the manifest's platform and filename constraints, verifies every file's size and SHA-256, and confirms `checksums.txt` matches the manifest. The equivalent command is `go run ./cmd/control-bundle --out dist --check`.

The gateway ignores drafts and prereleases. It skips a release already selected, and a release published before the current deployment's creation time. This prevents periodic checks from immediately replacing a newer development push with an older release. A development push can explicitly select an older bundle. The gateway rejects a different deployment while installation is in progress.

The configured GitHub repository and its release publishers are trusted to supply executable code. The updater and installers currently enforce checksums, not cosign identity verification. The published signature bundles allow separate verification of release metadata.

## Common keys and hidden administration help

With the superuser credentials configured on your administration machine:

```sh
bin/control keys create colleague
bin/control keys list
bin/control keys revoke KEY_ID
```

Key creation prints the common token once. The gateway persists only its hash in SQLite, bound to its user. A user account can issue common keys for its own fleet using the same commands with `CONTROL_USER_KEY`. The superuser issues keys only for the legacy operator fleet. Each node's local API accepts its own configured token, not every key in the gateway's registry.

Common keys can enroll peers and read the directory only within their owning fleet, and download shared update binaries. They cannot upload binaries, publish deployments, inspect rollout administration, trigger release checks, or manage keys. Update endpoints enforce the superuser role. Fleet-management endpoints require the owning user's account key or the operator for its legacy fleet. Revocation rejects new gateway requests and disconnects gateway sessions authenticated with that key. It does not revoke an already-established direct peer session or change a node's local API token.

`control help` always includes local CLI updates. It shows user fleet commands for an authenticated account and global account/update administration only for a verified superuser. Common-key, unauthenticated, and unreachable-gateway help omit management commands even when separate update authorization is saved. `control update --help` describes local self-updates without authentication. The `update push`, `update status`, and `update check` subcommands require the superuser role. `update authorize` validates and saves that authorization; it never grants it to an ordinary key. An explicit `--token` overrides saved and environment update credentials. The MCP server advertises no account, update, or key-management tools.

Keys govern Control's API. Providers still run with their node's OS privileges. Keep gateway secrets and gateway state out of accounts or filesystems accessible to unrestricted worker commands.

## Idle reservations and restart behavior

Nodes stage downloads while work continues. Before restarting, the gateway requires fresh status from every online participant and an acknowledged idle reservation on each one. A node reserves itself only when it has no accepted running or queued tasks, synchronous operations, transfers, open tunnels, or exclusive lease. Outgoing operations count too, so an orchestrator running a workflow cannot restart in the middle of delegation.

Once reserved, the node waits before admitting new work. Observation remains available. Accepted work finishes normally; updates do not cancel it. If work appears during reservation, the gateway releases partial reservations and waits again. A reservation expires after two minutes without renewal, allowing the pool to recover from a lost coordinator.

The gateway persists installation participants. A disconnected participant is not treated as successfully updated. The gateway waits for each participant to reconnect and report the selected version or expected checksum before restarting itself. Same-version participants still require idle reservations when another node or the gateway needs a restart. If every online participant and the gateway already match, the deployment completes without reserving idle time. The reservation survives a node restart, keeping new admissions closed until the gateway resumes the pool.

The dashboard shows `update` while nodes report progress for the selected deployment or remain paused for its maintenance. Before an actual restart, the gateway persists a one-minute display grace in that machine's directory record and extends it to one minute after the restart disconnect. Reconnection alone does not clear the grace. A fresh report confirming the selected version or checksum and released maintenance clears it durably, restoring normal idle/busy observation without waiting for the minute to expire. The deadline otherwise survives gateway restart and never counts a disconnected node as online or successfully updated. After expiry, normal availability returns. Older-deployment progress is ignored for display, and same-version skips do not start a restart grace.

`gateway` and `node` run under a small built-in supervisor. The supervisor launches a verified, versioned executable from the writable state directory. It waits for the old service to shut down and release its directory lock before starting the new one with the same arguments and environment. This works without overwriting a running Windows executable and keeps the outer process alive in a container or service manager.

The installed binary is the launcher. Standalone CLI and MCP invocations use their installed version; `control update` replaces that installation, and `make update` rebuilds the development machine's CLI. A running launcher continues using its loaded version until it exits. The managed service's current version and checksum appear in machine registration and update status. Restarting the launcher uses the persisted `runtime.json` selection.

On Windows, SCM hosts the supervisor as an automatic LocalService service. Updates restart only the managed child; the SCM process remains running. Background children and executable validation suppress console windows. SCM stop/shutdown controls cancel the supervisor and wait for the child to finish. Migrating an older login-based installation requires a current CLI and `control service start` in Administrator PowerShell; see [installation](installation.md#migrate-an-older-windows-installation).

Explicit Windows user-login startup runs the same supervisor under the user's limited interactive token, with a scheduled-task launcher instead of SCM. Managed updates retain that token and startup registration. A managed child update alone cannot add the new startup mode to an older installed CLI; install a CLI with user-login support before [migrating startup](installation.md#switch-windows-to-user-login-startup).

Current Windows supervisors run each verified child from a stable `<dataDir>\runtime\control.exe` copy. They replace this copy only after the previous child exits, while retaining the versioned binaries and `runtime.json` selection for validation and recovery. Installer firewall rules cover this stable path, so managed updates do not require new firewall permissions. Older supervisors need a current installed CLI and one restart before using the stable path. See [Windows firewall setup](installation.md#windows-firewall).

The updater verifies size and checksum, then runs `version --json` to check the staged executable before selecting it. It preserves identities, tasks, leases, keys, and other state. It does not migrate incompatible state schemas or automatically roll back after a service startup failure. The supervisor saves `runtime-previous.json` before switching. For manual recovery, stop the supervisor, restore that file as `runtime.json`, resolve or replace the gateway deployment, and restart.

### Gateway binary retention

After the gateway and online participants acknowledge the selected software and the gateway durably marks the rollout complete, it removes obsolete uploaded binaries and staged executable directories. Cleanup also runs for an already-complete deployment after gateway restart, and repeats every minute while that deployment remains complete. Failed or unfinished rollouts are not pruned. Cleanup errors are logged and retried without changing the successful deployment's phase.

The gateway retains all assets in the selected manifest for offline nodes, binaries pinned by unexpired, unrevoked, unredeemed installation invitations, and its current, previous, and requested runtime selections. The previous gateway binary is retained for local recovery, not general download. Active uploads and release downloads are protected. Unreferenced uploads younger than one hour are kept so multi-platform pushes can finish before publication; abandoned uploads are removed after that grace. Unreadable runtime records stop cleanup before deletion. Removal is confined to checksum-named entries within gateway update storage and does not follow symlinks outside it.

Authenticated node downloads serve only the selected manifest. Existing installation links use their separately pinned assets until redemption, revocation, or expiry. This policy applies to gateway storage, not node-local version retention, operator-created upload folders, CLI update backups, or system journals.

## HTTP and control messages

All endpoints use `Authorization: Bearer TOKEN`.

| Endpoint | Permission | Purpose |
| --- | --- | --- |
| `GET /v1/auth` | Any valid key | Current role, user ID, and role-scoped management capabilities |
| `POST /v1/admin/keys` | Superuser | Create legacy-fleet common key with JSON `name` |
| `GET /v1/admin/keys` | Superuser | Legacy-fleet key metadata without secrets or hashes |
| `DELETE /v1/admin/keys/{id}` | Superuser | Revoke a legacy-fleet common key |
| `PUT /v1/admin/updates/blobs/{sha256}` | Superuser | Stream an `application/octet-stream` binary with an exact `Content-Length` |
| `POST /v1/admin/updates` | Superuser | Publish a manifest after its binaries are uploaded |
| `GET /v1/admin/updates` | Superuser | Deployment, gateway software, and node acknowledgments |
| `POST /v1/admin/updates/check` | Superuser | Fetch configured latest GitHub release |
| `GET /v1/updates/blobs/{sha256}` | Any valid key | Download a binary from the selected manifest; other hashes return 404 |

Binaries are limited to 128 MiB each, manifests to six platform assets and 64 KiB, and key creation requests to 4 KiB. Invalid uploads never replace a verified binary. See [gateway binary retention](#gateway-binary-retention) for automatic cleanup and preserved references.

The existing Protobuf envelope carries `update.offer`, `update.prepare`, `update.commit`, `update.resume`, and `update.status`. Nodes accept update commands only from the reserved gateway sender. The gateway never forwards peer-supplied update commands. Node reports describe their own progress and idle state, not another node's authority.
