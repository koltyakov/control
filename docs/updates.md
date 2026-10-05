# Managed updates

[README](../README.md) · [Architecture](architecture.md) · [Protocol](protocol.md) · [Testing](testing.md)

The gateway accepts development bundles and can poll GitHub Releases. It distributes the selected platform binary across all user fleets, waits for idle nodes, restarts the nodes, then restarts itself. Offline nodes receive the current deployment when they reconnect. Publication and global rollout status are restricted to the gateway superuser; user-account and common keys cannot access them.

## Update the local CLI

```sh
control update
control upgrade
control update --version v0.2.0
```

Bare `update` downloads the latest stable GitHub release and replaces the invoked executable. `upgrade` is an alias. It requires write access to the executable's directory, but no gateway credential or node profile. `--version TAG` selects a specific release, including an older version; `CONTROL_VERSION` supplies the default tag. `CONTROL_RELEASE_REPO=owner/repository` overrides the embedded repository. An empty repository produces a configuration error. CLI self-updates use public GitHub release downloads.

The command validates the release manifest, selects the current OS and architecture, downloads the tag-pinned binary, verifies its size and SHA-256, and checks its `version --json` response before replacement. An identical installed checksum reports that the release is already installed. Failed downloads or validation leave the executable intact. Symlinks resolve to their target, and concurrent updates to the same executable are rejected.

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

The bundle must include the gateway platform and every registered node's platform, including offline nodes. Versions follow `git describe --tags --always --dirty`: an exact tag such as `v0.2.0`, a post-tag version such as `v0.2.0-3-gabc1234`, or a short commit hash before the first tag. Tracked local changes append `-dirty`; unavailable Git metadata falls back to `dev`. This applies to Make targets and direct `go run ./cmd/control-bundle` builds. Set `VERSION=dev-my-change` or pass `--version` to name one explicitly. Build timestamps remain separate metadata, so rebuilding the same checkout keeps its version label while producing new build metadata and checksums.

The equivalent commands are:

```sh
make bundle
bin/control update push dist
bin/control update status
bin/control update check
```

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

`control help` always includes local CLI updates. It shows user fleet commands for an authenticated account and global account/update administration only for a verified superuser. Common-key, unauthenticated, and unreachable-gateway help omit management commands. `control update --help` describes local self-updates without authentication. The `update push`, `update status`, and `update check` subcommands require the superuser role. An explicit `--token` overrides the environment credential for administration. The MCP server advertises no account, update, or key-management tools.

Keys govern Control's API. Providers still run with their node's OS privileges. Keep gateway secrets and gateway state out of accounts or filesystems accessible to unrestricted worker commands.

## Idle reservations and restart behavior

Nodes stage downloads while work continues. Before restarting, the gateway requires fresh status from every online participant and an acknowledged idle reservation on each one. A node reserves itself only when it has no accepted running or queued tasks, synchronous operations, transfers, open tunnels, or exclusive lease. Outgoing operations count too, so an orchestrator running a workflow cannot restart in the middle of delegation.

Once reserved, the node waits before admitting new work. Observation remains available. Accepted work finishes normally; updates do not cancel it. If work appears during reservation, the gateway releases partial reservations and waits again. A reservation expires after two minutes without renewal, allowing the pool to recover from a lost coordinator.

The gateway persists installation participants. A disconnected participant is not treated as successfully updated. The gateway waits for each participant to reconnect and report the expected checksum before restarting itself. The reservation survives a node restart, keeping new admissions closed until the gateway resumes the pool.

`gateway` and `node` run under a small built-in supervisor. The supervisor launches a verified, versioned executable from the writable state directory. It waits for the old service to shut down and release its directory lock before starting the new one with the same arguments and environment. This works without overwriting a running Windows executable and keeps the outer process alive in a container or service manager.

The installed binary is the launcher. Standalone CLI and MCP invocations use their installed version; `control update` replaces that installation, and `make update` rebuilds the development machine's CLI. A running launcher continues using its loaded version until it exits. The managed service's current version and checksum appear in machine registration and update status. Restarting the launcher uses the persisted `runtime.json` selection.

On Windows, SCM hosts the supervisor as an automatic LocalService service. Updates restart only the managed child; the SCM process remains running. Background children and executable validation suppress console windows. SCM stop/shutdown controls cancel the supervisor and wait for the child to finish. Migrating an older login-based installation requires a current CLI and `control service start` in Administrator PowerShell; see [installation](installation.md#migrate-an-older-windows-installation).

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
