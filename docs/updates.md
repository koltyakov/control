# Managed updates

[README](../README.md) · [Architecture](architecture.md) · [Protocol](protocol.md) · [Testing](testing.md)

The gateway accepts development bundles and can poll GitHub Releases. It distributes the selected platform binary across all user fleets, waits for idle nodes, restarts the nodes, then restarts itself. Offline nodes receive the current deployment when they reconnect. Publication and global rollout status are restricted to the gateway superuser; user-account and common keys cannot access them.

## Set up the gateway

The superuser key must be at least 32 characters. For a shared gateway, provision separate user accounts instead of distributing a common bootstrap token. An optional `CONTROL_TOKEN` enables the legacy operator fleet and must differ from the superuser key.

```sh
export CONTROL_SUPERUSER_KEY='your-separate-random-superuser-key-at-least-32-characters'
bin/control gateway --listen 0.0.0.0:7330 --data /var/lib/control-gateway
```

The gateway checks the latest stable, published GitHub release on startup and every 15 minutes using the repository embedded in the binary. `CONTROL_RELEASE_REPO` overrides that repository when present; an explicitly empty value disables release polling. Set `CONTROL_RELEASE_INTERVAL=30m` to change the interval. `CONTROL_RELEASE_TOKEN` supplies a GitHub token for private release downloads or API rate limits. Use HTTPS for remote gateway connections.

`make build`, `make bundle`, and `make update` embed the GitHub repository detected from `git remote get-url origin`, accepting SSH and HTTPS remotes. Without Git metadata, the builder uses `GITHUB_REPOSITORY` or falls back to `koltyakov/control`. Pass `--repository owner/repository` to `go run ./cmd/control-bundle` to select the embedded value explicitly. This build setting never overrides `CONTROL_RELEASE_REPO` at runtime.

The builder also injects a UTC `buildTime`. All binaries in a bundle share the manifest's `createdAt` timestamp. `control version --json` and update status expose `buildTime` and `releaseRepo`. Extended build metadata stays out of signed machine registration. A direct `go build` retains the source repository default but has no injected build timestamp.

Configure nodes with a common key for their owning user or a redeemed installation credential. Host setup retains the user's fleet-management credential separately. The node command removes `CONTROL_USER_KEY`, `CONTROL_SUPERUSER_KEY`, and `CONTROL_RELEASE_TOKEN` from its environment before launching its service and providers.

Managed updates require compatible Control versions on both the gateway and nodes. When upgrading from the pre-isolation release, pause automatic polling and upgrade the gateway first so its SQLite migration and fleet-aware authentication are available before new nodes connect. After that one-time migration, nodes-first managed rollouts apply normally. Without `CONTROL_SUPERUSER_KEY`, user provisioning and executable update administration are disabled; existing user-account fleet management remains available.

## Push from a development machine

```sh
export CONTROL_GATEWAY='https://control.example.com'
export CONTROL_SUPERUSER_KEY='your-gateway-superuser-key'
make update
make update-status
```

`make update` builds the CLI and a bundle for Linux, macOS, and Windows on amd64 and arm64. It uploads the binaries and publishes the manifest. A successful push means the gateway has accepted the rollout; use `update status` to follow staging, waiting for idle, installation, and completion.

For a Linux-only pool, build just its required platforms:

```sh
make update PLATFORMS=linux/amd64,linux/arm64
```

The bundle must include the gateway platform and every registered node's platform, including offline nodes. Development versions include a UTC timestamp. Set `VERSION=dev-my-change` to name one explicitly.

The equivalent commands are:

```sh
make bundle
bin/control update push dist
bin/control update status
bin/control update check
```

`update check` fetches the latest release from the repository configured on the gateway. It uses the same validation and rollout as periodic release checks.

## Publish GitHub releases

Build with the release tag as the version:

```sh
make bundle VERSION=v0.2.0
```

Attach `dist/control-manifest.json`, `dist/checksums.txt`, and all six `dist/control_OS_ARCH` binaries to the GitHub release. Windows binaries have an `.exe` suffix. The gateway expects raw binaries, not archives. The manifest records each binary's OS, architecture, filename, byte size, and SHA-256 digest. Its version must equal the release tag. The [release workflow](../.github/workflows/release.yml) builds and publishes these assets for `v*` tags. The checksum file also supports the [shell installer](installation.md).

The gateway ignores drafts and prereleases. It skips a release already selected, and a release published before the current deployment's creation time. This prevents periodic checks from immediately replacing a newer development push with an older release. A development push can explicitly select an older bundle. The gateway rejects a different deployment while installation is in progress.

The configured GitHub repository and its release publishers are trusted to supply executable code. Checksums detect damaged or mismatched downloads; they are not an independent publisher signature.

## Common keys and hidden administration help

With the superuser credentials configured on your administration machine:

```sh
bin/control keys create colleague
bin/control keys list
bin/control keys revoke KEY_ID
```

Key creation prints the common token once. The gateway persists only its hash in SQLite, bound to its user. A user account can issue common keys for its own fleet using the same commands with `CONTROL_USER_KEY`. The superuser issues keys only for the legacy operator fleet. Each node's local API accepts its own configured token, not every key in the gateway's registry.

Common keys can enroll peers and read the directory only within their owning fleet, and download shared update binaries. They cannot upload binaries, publish deployments, inspect rollout administration, trigger release checks, or manage keys. Update endpoints enforce the superuser role. Fleet-management endpoints require the owning user's account key or the operator for its legacy fleet. Revocation rejects new gateway requests and disconnects gateway sessions authenticated with that key. It does not revoke an already-established direct peer session or change a node's local API token.

`control help` shows user fleet commands for an authenticated account and global account/update administration only for a verified superuser. Common-key, unauthenticated, and unreachable-gateway help omit management commands. `control update --help` requires the superuser role. An explicit `--token` overrides the environment credential for this check. The MCP server advertises no account, update, or key-management tools.

Keys govern Control's API. Providers still run with their node's OS privileges. Keep gateway secrets and gateway state out of accounts or filesystems accessible to unrestricted worker commands.

## Idle reservations and restart behavior

Nodes stage downloads while work continues. Before restarting, the gateway requires fresh status from every online participant and an acknowledged idle reservation on each one. A node reserves itself only when it has no accepted running or queued tasks, synchronous operations, transfers, open tunnels, or exclusive lease. Outgoing operations count too, so an orchestrator running a workflow cannot restart in the middle of delegation.

Once reserved, the node waits before admitting new work. Observation remains available. Accepted work finishes normally; updates do not cancel it. If work appears during reservation, the gateway releases partial reservations and waits again. A reservation expires after two minutes without renewal, allowing the pool to recover from a lost coordinator.

The gateway persists installation participants. A disconnected participant is not treated as successfully updated. The gateway waits for each participant to reconnect and report the expected checksum before restarting itself. The reservation survives a node restart, keeping new admissions closed until the gateway resumes the pool.

`gateway` and `node` run under a small built-in supervisor. The supervisor launches a verified, versioned executable from the writable state directory. It waits for the old service to shut down and release its directory lock before starting the new one with the same arguments and environment. This works without overwriting a running Windows executable and keeps the outer process alive in a container or service manager.

The original installed binary remains the launcher. Standalone CLI and MCP invocations use their installed version; `make update` rebuilds the development machine's CLI. The managed service's current version and checksum appear in machine registration and update status. Restarting the launcher uses the persisted `runtime.json` selection.

The updater verifies size and checksum, then runs `version --json` to check the staged executable before selecting it. It preserves identities, tasks, leases, keys, and other state. It does not migrate incompatible state schemas or automatically roll back after a service startup failure. The supervisor saves `runtime-previous.json` before switching. For manual recovery, stop the supervisor, restore that file as `runtime.json`, resolve or replace the gateway deployment, and restart. Existing binaries remain in state storage until the operator removes unused versions.

## HTTP and control messages

All endpoints use `Authorization: Bearer TOKEN`.

| Endpoint | Permission | Purpose |
| --- | --- | --- |
| `GET /v1/auth` | Any valid key | Current role, user ID, and role-scoped management capabilities |
| `POST /v1/admin/keys` | Superuser | Create legacy-fleet common key with JSON `name` |
| `GET /v1/admin/keys` | Superuser | Legacy-fleet key metadata without secrets or hashes |
| `DELETE /v1/admin/keys/{id}` | Superuser | Revoke a legacy-fleet common key |
| `PUT /v1/admin/updates/blobs/{sha256}` | Superuser | Stream a binary with an exact `Content-Length` |
| `POST /v1/admin/updates` | Superuser | Publish a manifest after its binaries are uploaded |
| `GET /v1/admin/updates` | Superuser | Deployment, gateway software, and node acknowledgments |
| `POST /v1/admin/updates/check` | Superuser | Fetch configured latest GitHub release |
| `GET /v1/updates/blobs/{sha256}` | Any valid key | Download a staged binary |

Binaries are limited to 128 MiB each, manifests to six platform assets and 64 KiB, and key creation requests to 4 KiB. Invalid uploads never replace a verified binary. Uploads and versioned executables remain on disk; storage retention is manual.

The existing Protobuf envelope carries `update.offer`, `update.prepare`, `update.commit`, `update.resume`, and `update.status`. Nodes accept update commands only from the reserved gateway sender. The gateway never forwards peer-supplied update commands. Node reports describe their own progress and idle state, not another node's authority.
