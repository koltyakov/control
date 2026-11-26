# Managed updates

[README](../README.md) · [Architecture](architecture.md) · [Protocol](protocol.md) · [Testing](testing.md)

The gateway accepts development bundles and can poll GitHub Releases. It stages verified binaries, switches itself first, and then updates idle nodes independently across all user fleets, with at most two live node commits at once. Busy nodes remain pending. Gateway restarts separately drain relay traffic. Nodes that cannot connect to the new gateway epoch update before it. Offline nodes check the selected deployment on startup and catch up when connected. Publication and global rollout status are restricted to the gateway superuser; user-account and common keys cannot access them.

Valid web-console logins survive the gateway restart. Sessions expire after 24 hours idle or 7 days total, measured from their retained last-activity and creation timestamps. Cookies issued by older gateways keep their shorter browser expiry until the next sign-in. The first upgrade from a memory-only session version requires signing in once more; later updates retain sessions. CLI saved credentials are unchanged. See [browser sessions](console.md#credentials-sessions-and-limits).

CLI/MCP client sessions are not managed fleet nodes. Their platforms do not constrain the release bundle, they do not receive update commands, and they are excluded from rollout participants and the gateway's machine-reservation count. Work accepted on their destination nodes is still included in those nodes' idle checks. Upgrade the CLI with `control update`. The gateway and target nodes must both support client sessions before standalone execution can use them; see [standalone usage](installation.md#standalone-cli-and-mcp) and [terminology](terminology.md).

Managed nodes prefer the matching GitHub release for executable downloads. The tag's signed manifest must match the gateway-selected version and exact platform asset, including SHA-256, size, and filename. Nodes verify the release signature before downloading, then verify the executable bytes and version normally. The gateway retains the full uploaded or release-fetched bundle as fallback. Development-only builds, missing or different releases, invalid signatures, unavailable GitHub downloads, and corrupt public bytes fall back to the gateway's authenticated copy. The selected deployment never changes because a download source differs. Gateway credentials are never sent to GitHub. Public-release metadata checks have a ten-second deadline before fallback. Binary downloads from either source are bounded by stalls rather than total time: no response headers within 30 seconds, or no received bytes for 30 seconds, ends the attempt, so a slow link that keeps making progress can finish a large asset. A stalled public download falls back to the gateway; caller cancellation stops rather than trying another source.

Release-sourced selections use their recorded GitHub repository. Development selections use the gateway's configured release repository, falling back to its embedded repository when polling is disabled. Older gateways without delivery metadata retain gateway-only downloads. Private GitHub assets can fall back to the gateway without distributing its GitHub token to workers. `make update` still uploads all assets, so fallback remains available if GitHub goes down later. Rebuilding a release tag usually changes its binary checksum; such development bytes use the gateway unless they exactly match the public release.

[Hybrid installations](installation.md#hybrid-user-and-system-contexts) have two managed node profiles. Each owns its idle reservation, staged binaries, service process, task state, and restart. Updating one context does not restart the other, and an offline User context is not replaced by System execution. Publish new installers with `--hybrid-system` support before offering Hybrid invitations.

### Machine-wide download reuse

Automatic managed updates and `make update` rollouts share a content-addressed download cache across User and System profiles on the same machine. A cross-process filesystem lock covers cache lookup, download, and publication. Sequential and concurrent requests for the same SHA-256 download the binary once while its valid cache entry remains available. GitHub-first delivery and authenticated gateway fallback are unchanged. Repeated offers and restarts also reuse each profile's verified staged copy.

The cache is `/var/tmp/control-update-cache-v1` on Linux and macOS, or `Control Update Cache v1` under Windows' common ProgramData directory. It is writable across execution accounts and contains no credentials, configuration, or runtime selections. Treat its contents as untrusted. Cache reads reject symlinks and non-regular files, copy into private profile staging, and verify the copied size and SHA-256 before executable-version validation. Profiles never execute from the cache or hard-link it into their runtime. Modifying cached bytes cannot modify an already staged private binary or authorize different System executable bytes.

The cache retains the requested binary and one other binary, at most 256 MiB of verified blobs. Nodes fetch only the selected version, so the older entry only helps a rollback. Pruning runs under the same lock and leaves the lock file in place. An evicted, missing, or corrupt entry needs a fresh download. If cache creation, access, or publication fails, updates retain private verified downloads; lock acquisition waits at most five minutes before falling back. Cancellation stops the wait without starting another download. Updaters keep reporting status while they wait or download, so the gateway's freshness check sees them as present. Deduplication therefore requires a usable cache and participating updater versions in both profiles. Existing installations gain it when their managed children upgrade; standalone `control update` remains a separate CLI operation.

## Update the local CLI

```sh
control update
control upgrade
control update --version v0.2.0
```

Bare `update` downloads the latest stable GitHub release and replaces the invoked executable. `upgrade` is an alias. It requires write access to the executable's directory, but no gateway credential or node profile. `--version TAG` selects a specific release, including an older version; `CONTROL_VERSION` supplies the default tag. `CONTROL_RELEASE_REPO=owner/repository` overrides the embedded repository. An empty repository produces a configuration error. CLI self-updates use public GitHub release downloads.

The command validates the release manifest and selects the current OS and architecture. If the running CLI already has that version, it reports that the release is already installed without downloading or replacing the binary, even if the release checksum differs. An identical installed checksum also skips replacement. Otherwise, it downloads the tag-pinned binary, verifies its size and SHA-256, and checks its `version --json` response before replacement. Failed downloads or validation leave the executable intact. Symlinks resolve to their target, and concurrent updates to the same executable are rejected.

On Windows, the updater preserves the executable's access grants and renames the old executable before installing the new one, allowing running CLI and service-launcher processes to finish. A locked old executable can remain as `previous.exe` in a `.control-update-*` directory beside the CLI. The next update removes such directories once their old executable is no longer running. On Linux and macOS, replacement uses an atomic rename and preserves executable permissions.

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

Managed updates require compatible Control versions on both the gateway and nodes. When upgrading from the pre-isolation release, pause automatic polling and upgrade the gateway first, to v0.14.0, so its SQLite migration and fleet-aware authentication are available before new nodes connect. Later releases cannot import that state. An older running gateway still uses its previous rollout order until that coordinator is upgraded. The first upgrade to independent coordination therefore follows the old contract. Without `CONTROL_SUPERUSER_KEY`, user provisioning and executable update administration are disabled; existing user-account fleet management remains available.

## Push from a development machine

With persisted gateway superuser authorization, run:

```sh
make update
make update-status
```

`control login` validates the key through `/v1/auth` and persists it in `admin.json`. A superuser login also saves update authorization in `update-admin.json`. Later normal fleet logins do not overwrite that update authorization, so the dashboard can use your account while updates retain operator permission. Repeating `control login` for the same gateway revalidates the saved key instead of asking for it again. Supply `--api-key` or `--key-stdin` to replace the login. Both credential files use atomic private-file storage, with `0600` permissions on Unix. Failed credential validation leaves both profiles unchanged.

When replacing an older CLI's saved superuser login with a normal fleet login on the same gateway, login verifies and preserves the previous operator key in `update-admin.json` before overwriting the regular profile. It does not infer operator authority from an ordinary account key.

`make update` builds the native CLI, then runs `update authorize --check` before building the platform bundle. This check reuses saved authorization and never prompts or saves credentials. If no selected credential has superuser permission, the target stops with a permission error. Authentication and connection failures also stop it before bundle builds, including under `make -j`. A normal account login by itself does not authorize gateway-wide updates.

The target then builds the local checkout's bundle for Linux, macOS, and Windows on amd64 and arm64. It uploads the binaries and publishes the manifest without fetching a GitHub release. Every enrolled node across user fleets participates independently, including nodes on orchestrator machines and all workers. Offline nodes catch up when they reconnect. Standalone CLI/MCP installations are not enrolled nodes and are not updated by this rollout, but their relay traffic protects the gateway from restarting. A successful push means the gateway has accepted the rollout; use `make update-status` to follow individual node progress and gateway drain blockers.

Binary uploads stream from an open file as `application/octet-stream`, with an exact `Content-Length`. The gateway streams each upload into a temporary file while hashing it, then publishes the verified file. Only the small manifest uses JSON. A reverse proxy or tunnel must forward request and response bodies without whole-file buffering and permit binaries up to Control's 128 MiB limit. Streaming does not bypass a proxy's total-body limit; HTTP 413 stops the push before manifest publication. Stock expose v0.28.2 has a hard-coded 10 MiB tunnel-body limit, which is too small for Control binaries. Use an expose build with uncapped streamed tunnel bodies; bounded buffers and frame limits still apply. Its static-site `EXPOSE_PUBLISH_MAX_BYTES` setting does not change the tunnel limit.

Update administration selects an explicit global `--token`, then `CONTROL_SUPERUSER_KEY`, then a matching saved update authorization. Otherwise, the usual gateway credential selection applies, including a matching saved login. Saved credentials are never reused for a different gateway. Dashboard, execution, key-management, and user-management commands do not use `update-admin.json`. A normal account or common key still cannot publish updates. Missing credentials, rejected authentication, and gateway connection failures are reported directly rather than as `unknown command "update"`.

Environment variables remain optional for automation. As an explicit alternative to login, `bin/control update authorize` can securely prompt for an operator key and save it without replacing the regular fleet login. `update authorize --key-stdin` reads a replacement key for validation and storage; it cannot be combined with `--check`, global `--token`, or `CONTROL_SUPERUSER_KEY`. This explicit authorization command does not fall back to prompting after a rejected credential or a connection failure. Remove `update-admin.json` to forget the separate update authorization. A superuser key still saved in `admin.json` remains usable for updates. Keep both files out of node workspaces, state directories, and other locations accessible to unrestricted providers.

For a Linux-only pool, build just its required platforms:

```sh
make update PLATFORMS=linux/amd64,linux/arm64
```

The bundle must include the gateway platform and every registered node's platform, including offline nodes. Versions follow `git describe --tags --always --dirty`: an exact tag such as `v0.2.0`, a post-tag version such as `v0.2.0-3-gabc1234`, or a short commit hash before the first tag. Tracked local changes append `-dirty`; unavailable Git metadata falls back to `dev`. This applies to Make targets and direct `go run ./cmd/control-bundle` builds. Set `VERSION=dev-my-change` or pass `--version` to name one explicitly. Build timestamps remain separate metadata, so rebuilding the same checkout keeps its version label while producing new build metadata and checksums.

Publishing the currently selected version returns the existing deployment without starting another rollout or changing its pinned assets, timestamps, phase, or participants. Single-process nodes and the gateway also skip binary staging and restart when their running version matches the selected version, regardless of checksum differences. A reported supervised runtime still requires migration. Older nodes still receive the selected bundle. To deploy changed code under a reused `-dirty` or `dev` label, select a new version explicitly. Build and upload steps can still run for an unchanged version; installation and restart are skipped.

`make update` preserves the version label unchanged for both its native CLI and bundle, including `-dirty` and explicit `VERSION` overrides. It never appends a timestamp. Commit changed code or choose a new short label such as `make update VERSION=dev-fix-2` when another development rollout is needed; reusing the selected label still retains the existing deployment.

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

The release workflow verifies and builds the same commit using the GitHub event SHA, with the tag as the version. Its verification job checks dependencies, runs vet and race tests, and exercises both Compose transport modes before publication. It supports `v*.*.*` tags and manual dispatch from the matching tag:

```sh
gh workflow run release.yml --ref v0.2.0 -f tag=v0.2.0
```

Dispatch rejects a branch ref or a mismatched tag. Tags with a prerelease suffix, such as `v0.2.0-rc.1`, publish as GitHub prereleases. Release runs for the same tag are serialized. For a manual release build:

```sh
make release-local VERSION=v0.2.0
```

Attach `dist/control-manifest.json`, `dist/control-manifest.sig`, `dist/checksums.txt`, and all six `dist/control_OS_ARCH` binaries to the GitHub release. Windows binaries have an `.exe` suffix. The gateway expects raw binaries, not archives. The manifest records each binary's OS, architecture, filename, byte size, and SHA-256 digest. Its version must equal the release tag. The [release workflow](../.github/workflows/release.yml) builds and checks the unsigned bundle and build-time signer without secrets. A separate publication job signs the final manifest with Ed25519, verifies it against the compiled public-key pins, and publishes only public release assets. It uses the `release` GitHub environment and has `contents: write`, without OIDC permission. The unsigned checksum file supports the [shell installer](installation.md); no separate checksum signature is published.

`make release-check` validates an existing `DIST_DIR`, default `dist`, without rebuilding or publishing. It checks the manifest's platform and filename constraints, verifies every file's size and SHA-256, and confirms `checksums.txt` matches the manifest. The equivalent command is `go run ./cmd/control-bundle --out dist --check`.

Standard Windows bundles work around [Go #81956](https://go.dev/issue/81956). For the reviewed Go 1.27.4 source, the builder uses a temporary overlay to allocate the 32 MiB entropy scratch buffer only when FIPS entropy generation needs it. The buffer size, entropy source, and retry behavior stay the same. Explicit `GOFIPS140` modes other than `off` bypass the workaround. Go forbids overlays inside its module cache, so the builder copies a cached toolchain to its temporary build directory once and uses the copy for both Windows architectures. It does not modify the original toolchain. The builder checks the executable's writable image footprint before generating release checksums. Plain `go build` does not apply the overlay. This workaround does not affect Linux builds.

The gateway ignores drafts and prereleases. It skips a release already selected, a release published before the current deployment's creation time, and a `vMAJOR.MINOR.PATCH` release not newer than the persisted release floor. The floor in `deployment.json` records the newest version ever selected from GitHub and survives development pushes, so republishing an older signed release with a newer publication date cannot roll the fleet back. Tags outside that form fall back to the publication-date check. This prevents periodic checks from immediately replacing a newer development push with an older release. A development push can explicitly select an older bundle. A new selection may replace a merely pending rollout, but not an unresolved node commit or a gateway switch. Legacy persisted `installing` deployments retain their conservative publication barrier until completion.

The CLI self-updater, gateway release fetcher, and managed nodes' GitHub downloads require `control-manifest.sig`. They verify the exact manifest bytes with a public key compiled from [the release keyring](../internal/update/release_keys.go), scoped by exact repository and key ID. The signature also binds the repository, release tag, key ID, and protocol version. Verification needs no certificates, transparency service, downloaded trust roots, or external executable. Missing, malformed, oversized, unknown-key, or incorrect signatures stop public executable download and validation, including a same-version CLI check. The gateway may skip an already selected release without downloading it again. Managed nodes retain their authenticated gateway fallback.

This is a clean cutover from Sigstore, without legacy filenames or verification fallback. Existing installations need a one-time trusted reinstall or superuser development push. Current updaters reject old Sigstore-only releases. Do not disable verification to migrate. See [D153](decisions.md#d153-pinned-ed25519-release-manifests).

Pinned signatures prove possession of a trusted release key, not execution by a particular GitHub workflow. There is no transparency-log proof or trusted signing timestamp. Protect the long-lived private key and the CI job allowed to use it. A compromised signing job can steal an exportable key. The release floor blocks rollback to older semantic versions but not a freeze: withholding newer releases still leaves the fleet on its current version, and a signed manifest's creation date alone does not prove freshness. Automatic check errors appear in the gateway log as `release check`; publication alone does not confirm installation.

Development bundles remain explicitly superuser-published and do not need the public release key. Node runtime downloads trust the gateway's authenticated selected manifest and verify bytes against it. Bootstrap shell installers retain their checksum-based trust model; runtime signature verification does not authenticate a first installation script or executable. Manual GitHub publication must include a signature from a pinned key.

### Release signing keys

The current `koltyakov/control` pin is `release-2026-11-17`. Its public-key SHA-256 fingerprint is `dc7e0b74b00ee68f22eb92192a9988733baa325bf66519a78b0a839a94aea13f`. The GitHub `release` environment restricts deployment to tags matching `v*.*.*`, without an additional manual approval gate. Missing or unknown pins reject public releases and the CI signer refuses to write an unverifiable signature. Development builds and superuser pushes remain available. Test keys are ephemeral and are never production pins.

Control, Expose, and BBX use the same GitHub configuration names and scopes, with a different keypair in each repository:

| Name | Type and scope | Value |
| --- | --- | --- |
| `RELEASE_SIGNING_PRIVATE_KEY` | Secret in the `release` environment | Ed25519 PKCS#8 PEM private key |
| `RELEASE_SIGNING_PUBLIC_KEYS` | Repository variable | JSON map of stable key IDs to base64 raw 32-byte public keys |
| `RELEASE_SIGNING_KEY_ID` | Repository variable | Active key ID, currently `release-2026-11-17` |

Keep public configuration at repository level so unsigned builds need no environment secret access. Control additionally requires that the public map exactly match its reviewed source-controlled pins. The build-time signer's `--check-keys` mode checks that agreement and the active ID before CI builds the bundle. This variable is a consistency check, never an override of trusted keys.

Store local keys outside the checkout under `~/.ssh/github/`: `control.pem` is the private key, `control.pub` is SPKI public PEM, and `control.json` is the public JSON map. Expose uses the basename `expose`; Browser Bridge uses `bbx`. The directory must be private and private files must have mode `0600`. These are release-signing keys, not SSH login keys or certificates.

Generate a dedicated Ed25519 key on a trusted administration machine. For a new key only, with OpenSSL 3 on Linux or macOS, after confirming the destination files do not exist:

```sh
umask 077
mkdir -p "$HOME/.ssh/github"
chmod 700 "$HOME/.ssh/github"
openssl genpkey -algorithm ED25519 -out "$HOME/.ssh/github/control.pem"
openssl pkey -in "$HOME/.ssh/github/control.pem" -pubout \
  -out "$HOME/.ssh/github/control.pub"
public_key=$(openssl pkey -in "$HOME/.ssh/github/control.pem" -pubout -outform DER |
  tail -c 32 | openssl base64 -A)
printf '{"release-2026-11-17":"%s"}\n' "$public_key" > "$HOME/.ssh/github/control.json"
```

The JSON map contains only the raw public key extracted from the Ed25519 SubjectPublicKeyInfo. Review and add the same map to `internal/update/release_keys.go`, under the exact repository. Example entry, with a placeholder rather than a usable key:

```go
var releaseKeys = ReleaseKeyring{
    "koltyakov/control": {
        "release-2026-11-17": "BASE64_RAW_32_BYTE_PUBLIC_KEY",
    },
}
```

Keep the public key in source, not a downloaded asset or mutable runtime setting. `CONTROL_RELEASE_REPO` changes download routing, not trusted keys. Forks must build their own reviewed repository/key pins. Never reuse node identities, gateway keys, or user credentials for release signing.

Create and protect the GitHub `release` environment before publishing. Restrict allowed release tags, configure reviewers where available, and protect workflow/keyring changes. Configure its secret and variable:

```sh
gh secret set RELEASE_SIGNING_PRIVATE_KEY --repo koltyakov/control --env release \
  < "$HOME/.ssh/github/control.pem"
gh variable set RELEASE_SIGNING_PUBLIC_KEYS --repo koltyakov/control \
  --body "$(cat "$HOME/.ssh/github/control.json")"
gh variable set RELEASE_SIGNING_KEY_ID --repo koltyakov/control --body release-2026-11-17
```

The secret is one unencrypted Ed25519 PKCS#8 PEM `PRIVATE KEY`. Keep an encrypted offline backup and record who can restore it. Never commit the private key, upload it as an artifact, print it, or pass it as a command-line argument. Control needs no feed initialization flag. Remove superseded signing secrets and variables after the new configuration is verified; do not keep repository-scoped copies of the environment secret.

The standalone [signer](../cmd/control-sign/main.go) accepts the private key only on stdin. It validates the manifest and signing context before reading the key, checks the resulting signature against its compiled pins, and writes the public signature atomically:

```sh
go run ./cmd/control-sign --repository koltyakov/control --tag v0.2.0 \
  --key-id release-2026-11-17 < "$HOME/.ssh/github/control.pem"
go run ./cmd/control-sign --check --repository koltyakov/control --tag v0.2.0
```

Run `make release-check` before signing. Do not change manifest bytes afterward. The signer is build-time tooling, not part of the shipped `control` command. CI builds it before the secret-bearing step, passes the key through stdin, removes the secret from the signer's environment, and performs an additional public verification before publishing. All release-workflow actions are pinned to commit SHAs.

### Key rotation and recovery

For planned rotation from A to B:

1. Generate B separately and commit its public pin alongside A. Update the repository public-key variable to match both pins.
2. Publish a bridge release signed by A whose binaries trust both keys.
3. Upgrade installations to the bridge before switching the environment secret and repository key-ID variable to B.
4. Publish with B, then remove A from a later release and the public-key variable after the migration window. Retaining A indefinitely retains its authority.

An installation that missed the bridge must explicitly install that bridge version or use a trusted reinstall/development push. There is no automatic remote key discovery. Removing a key also makes its historical signatures unverifiable by the new binary.

If A is lost, restore the encrypted backup. If no trusted key remains available, use a trusted reinstall or authenticated superuser development push to distribute new pins. An optional pre-pinned offline recovery key can help with loss, but every accepted key has full signing authority.

If a key is compromised, disable publication, investigate CI and repository access, generate a new key, and distribute binaries that remove the old pin through a trusted recovery path. Do not sign the recovery only with the stolen key and assume that establishes trust. Old binaries still accept that key; a signed revocation file cannot force them to stop. A pre-pinned recovery key alone does not revoke it. If the gateway is compromised too, its development-push route is not a trustworthy recovery channel. Document the affected releases and coordinate manual recovery for installations that cannot be reached safely.

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

The gateway goes first. After a publication it stages its own executable, drains, and restarts before offering the deployment to any node, so nodes download from and commit under the new gateway and its restart never interrupts their updates. Node offers wait for that switch for at most two minutes after the coordinator selects the deployment. A gateway drain attempt that times out, such as one held by a relayed tunnel, releases them at once, and failed gateway staging releases them when the window ends; nodes then update independently while the gateway keeps retrying. Online nodes that cannot connect to the staged gateway epoch, and a gateway that cannot replace itself, skip the wait and keep nodes first. Machines appear offline for the few seconds of the gateway restart, before their own updates start. Proxy children then follow their proxy, as described below.

Nodes stage downloads while work continues. Before restarting a node, the gateway requires that node's fresh status and acknowledged idle reservation, not fleet-wide idleness. A node reserves itself only when it has no accepted running or queued tasks, synchronous operations, transfers, open tunnels, or exclusive lease. Outgoing operations count too, so a node coordinating a workflow cannot restart in the middle of delegation. Same-version nodes skip staging, restart, and unnecessary reservations.

A managed gateway proxy is the scoped exception to independent node ordering. While the proxy reports application work such as an open TCP tunnel or holds a lease, its children keep serving work. The coordinator waits for fresh proxy health showing neither before reserving idle children, draining their gateway relay/HTTP traffic, and parking their idle gateway connections. The proxy retains its zero-open-CONNECT fence and ordinary durable commit. Parked children show `update` with `waiting-proxy` and a one-minute display grace instead of `offline`. Children reconnect within 30 seconds, resume after the proxy's fresh applied/unpaused acknowledgement, and acknowledge released reservations before their own software updates. Same-version children may hold a proxy-maintenance reservation without restarting. These holds do not consume either of the two software commit slots. Unrelated nodes remain independent. See [proxy rollout coordination and compatibility](gateway-proxy.md#updates).

Gateway coordination still checks every second. Outside the installing phase, deferral reconciliation examines only existing deferred identities; a completed rollout with none skips that scan. Peer snapshots preallocate their storage, and an empty command-throttle history needs no retention index. Returning nodes, status freshness, restart grace, and maintenance deadlines retain the same checks.

Once reserved, the node waits before admitting new work. Observation remains available. Accepted work finishes normally; updates do not cancel it. A newly busy node releases its own reservation without releasing unrelated reservations. A reservation expires after two minutes without renewal, allowing the node to recover from a lost coordinator. At most two live node contexts have outstanding commit intent; excess reservations left by an older coordinator are released.

Each verified node executable reports `gatewayProtocol`, `nodeGatewayMin`, and `nodeGatewayMax` in `version --json`. The updater reports that metadata as `stagedSoftware` only after size, checksum, platform, and executable validation. The gateway binds it to the selected version and exact asset before using its range. Current binaries implement gateway epoch 1 and support node connections to epochs 0 through 1; epoch 0 denotes legacy gateways without declarations. Missing ranges retain gateway-first ordering. New nodes also check the authenticated gateway epoch before registration. Compatibility metadata is inside the authenticated executable bytes, so signed manifest bytes and deployment IDs are unchanged. A breaking gateway epoch requires compatible bridge nodes first; undeclared legacy nodes cannot authorize a switch beyond epoch 1.

The gateway persists a node's commit intent in `deployment.inFlight` before dispatch. Only a fresh selected-software report with released maintenance, or an explicit error with released maintenance, clears it. Disconnection, stale status, and display grace do not confirm success. An unresolved commit continues to block another publication and the gateway switch, even after its node becomes offline-deferred. Other eligible nodes can still progress. This intentionally requires recovery of an uncertain restart rather than silently superseding it.

Gateway maintenance is independent of local node work. It refuses new relay carriers for a 30-second drain attempt while existing carriers keep forwarding. Signed handshake negotiation enables `gateway.drain` and `gateway.resume` for both nodes and standalone clients. Outgoing session owners stop opening new streams on cached relay sessions and retire them only after stream/FIN bookkeeping drains. Incoming sides never infer safety from an empty receive queue. Legacy carriers remain blockers until closed naturally or disconnected. Relay tracking includes pending setups, both authenticated endpoint connection generations, and two-sided closure. Per-connection carrier counts are bounded to 256.

Before draining, the gateway also holds its own restart while any online node that still needs the selected software reports `downloading`, for at most five minutes after staging. Gateway-first ordering normally prevents that overlap; it remains possible for a node that starts and stages on its own, or after the gateway-first window. Nodes then finish fetching from the running gateway, and contexts sharing a machine cache reuse one download instead of failing on the restart and retrying separately. Node gateway downloads also retry 429, 502, 503, 504, stalls, and connection failures for up to 30 seconds within one attempt, holding the machine cache lock, so a brief gateway outage does not surface as an update error. Each retry keeps the bytes already received and resumes with an HTTP `Range` request; progress renews the 30-second window. The complete file is still verified against the selected size and SHA-256, and a mismatch after resuming refetches once from the beginning. A final atomic fence requires no live relay carriers, queued or in-progress packet writes, admitted HTTP mutations, or executable downloads. It then closes new HTTP, connection, and relay admission before persisting the gateway switch. Busy local tasks and established direct WebRTC streams do not block it. Drain timeout restores relay admission and retries after 30 seconds; it never cancels a stream. A persistent relayed tunnel can therefore postpone the gateway indefinitely without blocking unrelated node updates. Failed switching reopens admission. New discovery, signaling, and workflow steps can fail during the brief gateway outage; no application request or stream is replayed automatically. Replacement restarts only the gateway with its retained arguments, state, and environment.

A negative `queuedPacketBytes` diagnostic is invalid accounting, not idle capacity. Older gateways sent rename-triggered `directory.changed` notifications directly to the writer without reserving their bytes, then released those bytes after writing or cleanup. Each recipient could subtract 153 unreserved bytes and leave the restart fence blocked even with no live relays. Corrected gateways enqueue those notifications through the shared packet budgets. The fix prevents new underflows; it does not clamp counters or relax restart admission. Updating nodes or uploading another bundle cannot repair an already-running older gateway's counter. Any deployment or recovery of that gateway requires a separately authorized operational step.

A disconnected installation participant gets two minutes to reconnect. After that it is durably listed in `deployment.deferred`, not reported as successfully updated, and no longer prevents other online nodes from updating. `complete` means the available fleet and gateway have updated; deferred identities remain explicitly pending. An unresolved commit remains a separate publication barrier as described above. Registrations and credentials are unchanged. A returning node immediately participates again, and fresh confirmation of the selected software with released maintenance clears its deferral. Nodes without unresolved commits receive whichever deployment is selected when they return, not every missed intermediate version.

Nodes query authenticated `GET /v1/updates` at startup for the selected ID, manifest, and optional `releaseRepo`, verify and stage their platform binary, and report readiness. This endpoint exposes no fleet identities or rollout participants. The gateway still owns idle reservations and commit, so startup checks cannot interrupt accepted work or independently authorize a restart. A failed startup query leaves catch-up to offers over the authenticated connection; older gateways returning HTTP 404 retain that offer-based flow. A reservation survives a node restart, keeping new admissions closed until the gateway resumes that node.

Administrative update status adds `nodeProgress` and `gatewayUpdate`. Node reasons include `waiting-idle`, `waiting-gateway`, `waiting-proxy`, `reserved`, `restarting`, `applied`, `offline-pending`, and `awaiting-status`; staging/download errors remain explicit. `deployment.gatewayState` independently reports gateway progress, including `waiting-nodes`, `waiting-downloads`, `draining`, `waiting-relays`, `switching`, and `applied`. `gatewayUpdate.relayCarriers` and `activeRequests` identify current transport/request blockers without operation arguments. These global fields remain superuser-only. Fleet snapshots expose only each visible machine's own `updateState`, which the console shows beneath Updating.

The dashboard shows `update` while nodes report progress for the selected deployment or remain paused for its maintenance. Before an actual restart, the gateway persists a one-minute display grace in that machine's directory record and extends it to one minute after the restart disconnect. Reconnection alone does not clear the grace. A fresh report confirming the selected version or checksum and released maintenance clears it durably, restoring normal idle/busy observation without waiting for the minute to expire. The deadline otherwise survives gateway restart and never counts a disconnected node as online or successfully updated. After expiry, normal availability returns. Older-deployment progress is ignored for display, and same-version skips do not start a restart grace.

### Single-process runtime and update handoff

Current `gateway` and `node` entry points run the service in the foreground process, holding `runtime.lock` throughout its lifetime. There is no permanent Control supervisor or updater. Providers and executable-version validation can still start their own subprocesses. Updates remain graceful restarts, not live migration of memory or connections.

On Linux and macOS, the service finishes shutdown and closes its state before revalidating the next executable, saving `runtime-previous.json` and `runtime.json`, and replacing itself with `exec`. The PID, working directory, environment and standard streams survive. This also preserves container PID 1 and systemd/launchd process tracking. The new image reacquires the runtime lock before opening service state. A competing start can win that lock, but two service instances cannot open the same state. Failure to exec or start is reported, not automatically rolled back or retried by Control.

Windows cannot replace its process image this way. After verifying the candidate, Control starts a temporary `__update-handoff` process from the verified versioned binary. The helper opens a handle to the old process and checks access to the profile's exact SCM service or login task before reporting readiness. Only then does the old process shut down. The helper requires an explicit commit after successful cleanup; EOF, preparation failure, shutdown failure or an explicit service stop does not authorize replacement. It waits for the old process to exit and for `runtime.lock`, checks that the request and prior selection have not changed, persists the switch, and replaces `<dataDir>\runtime\control.exe` while it is not running. It then starts the SCM service, login task, or process exactly once. It exits after the replacement acknowledges its unique readiness marker following startup checks and listener binding, or reports a bounded failure. There is no automatic replay of an uncertain start.

User helpers run through a separate temporary `ControlUpdate-*` task under the same interactive user's Limited token. It has no trigger or retry policy and a three-minute execution limit. A local named pipe grants access only to that user, rejects remote clients, and authenticates a random 256-bit token before the node sends its bounded prepare/commit exchange. Preparation removes the temporary task definition while its running instance completes the handoff. This avoids Task Scheduler jobs that deny breakaway and keep a task Running while descendants exist. Registration, launch, authentication or cleanup failure leaves the node running without committing. SCM and standalone helpers do not request job breakaway. Standalone replacements inherit any enclosing job's containment, have a new PID, and return the original foreground invocation. Use SCM or login startup when the replacement must remain service-manager-owned.

SCM and new Windows login tasks directly host the stable runtime. SCM stop/shutdown controls cancel that process and wait for cleanup. The profile's service SID has start/query access to its own SCM service, not unrelated services. Login tasks run a GUI-subsystem runtime directly under the same limited interactive token, with no persistent bootstrap. That image differs from the verified original only in its PE Subsystem field. Before serving, it proves that exact byte relationship and reports the original release checksum. Ordinary `version --json` inspection still reports the inspected file's actual checksum. Sources and `runtime.json` always retain original release bytes. Firewall rules continue covering the same stable path without another UAC prompt.

GUI-source verification streams both images with two 32 KiB buffers. It hashes the complete original and compares every derived byte, permitting only the validated Subsystem change. It does not load either whole executable into the persistent User process. The previous whole-image `io.ReadAll` implementation caused a startup RAM spike even on idle profiles; declining memory after garbage collection did not remove that allocation defect.

Standalone CLI and MCP invocations retain their installed version. `control update` replaces that installation, and `make update` rebuilds the development machine's CLI; neither bypasses managed idle admission. Starting a current installed Unix CLI uses the saved `runtime.json` selection without retaining a launcher process. Windows startup registrations point directly to the selected stable copy.

The updater preserves identities, tasks, leases, keys and other state. It does not migrate incompatible state schemas or automatically roll back after startup failure. Until a node successfully starts the selected binary, or for the gateway's retained previous selection, manual recovery is possible: stop the service, restore `runtime-previous.json` as `runtime.json`, resolve or replace the gateway deployment, and restart. On Windows, refresh the stable copy with a current installed CLI's `service start` before restart. Stale readiness markers cannot acknowledge a different selection and do not prevent manual recovery.

### Migrate an existing supervisor

New Windows children automatically retire supported legacy launchers before admitting work. A helper binds the installation record, existing identity, parent executable and startup registration, verifies restart authority, and prepares the selected runtime. The child exits without writing the old supervisor's restart request, so the old launcher exits naturally. Only after that parent exits and releases its runtime lock does the helper change the startup entry and restart directly. User migration preserves the task's principal, triggers, settings and ACL; preflight checks write authority by updating its unchanged definition, accepting only the known single-logon-trigger bootstrap. A separate migration record cannot be mistaken for an old supervisor's child-restart request. Re-enrollment is checked again under the installation lock after shutdown.

This automatically migrates ordinary installing-user login tasks and SCM contexts already authorized to change their own registration, including LocalSystem companions. Older restricted LocalService services generally lack `SERVICE_CHANGE_CONFIG`. Control must not grant that right to LocalService, because it also permits changing the service account to LocalSystem. Such services keep serving under the existing wrapper and report an explicit migration error requiring one Administrator `service stop`/`service start` with a current installed CLI. Unsupported or foreign startup definitions are not rewritten. No UAC prompt, privilege escalation, unrelated-service mutation or uncertain start retry occurs.

Older login tasks created from an elevated installer can be owned by Administrators while granting their Limited user read-only access. Automatic migration correctly fails its unchanged-definition write preflight in that case. A specifically authorized privileged repair can add read/write/execute maintenance access for that installing SID on the exact owned Limited task, preserving its definition and all other ACL entries. This does not stop the profile. The next normal managed update can then retire its wrappers. Current task registration ensures that same scoped maintenance grant, without granting the User process administrative rights.

Software reports add `runtime: "supervised"` while new code still runs through the legacy entry points, and `runtime: "single"` after migration. A supervised runtime cannot acknowledge an update as applied, even when its version and checksum match. Rollout completion therefore cannot silently leave these reported supervisors behind. Older software without the field retains its prior compatibility behavior. Publish a fresh version label containing this migration code; reusing an already selected `-dirty` or release label still does not replace manifest bytes.

Software compatibility and runtime migration are separate checks. A gateway already running the selected release can coordinate node updates even if its own startup entry remains supervised. A missing migration acknowledgement must not make compatible nodes wait for that same gateway while occupying both commit slots. Existing reservation and commit rules still apply. Explicit fresh, unpaused migration errors release their commit intents without marking the runtime applied, allowing other nodes to proceed. Unknown or paused commits remain barriers. A gateway or node needing a privileged startup refresh still cannot complete the rollout.

When a fresh, unpaused node reports the exact selected version and checksum but still reports `runtime: "supervised"`, its software switch has settled separately from startup migration. The coordinator clears that software commit intent without reporting migration success, stops re-offering or recommitting the same selection, and shows `startup-refresh-required`. Paused, restarting, stale, mismatched-checksum, or unrelated-deployment reports do not settle an intent. Other nodes retain the two-slot admission limit and continue updating. If only these settled startup blockers remain after gateway completion, the deployment becomes `blocked`, not `complete`; obsolete binaries are not pruned. A new superuser publication may proceed when there are no unresolved commits or gateway switch, as for other merely pending work. Publication conflicts identify their actual unresolved node commits or gateway switch rather than saying every unfinished rollout is still installing.

Supported macOS launchd and Linux systemd profiles migrate before the new child admits work. The child binds its remembered installation and identity, exact standard startup definition and loaded job, supervisor PID/start time/account/image, and selected executable. Systemd binding also requires the correct cgroup, validated drop-ins and no pending daemon reload. With its existing file permissions and installation/CLI locks, it atomically refreshes only that installed startup executable, preserving its mode, ownership and extended attributes, including SELinux labels, and retaining the previous bytes. It does not edit or unload the job. A deliberate nonzero child exit propagates through the old supervisor, allowing the verified job's existing restart-on-failure policy to reopen the refreshed entry point once. Explicit cancellation does not request this restart. Completion still requires a fresh single-process report, not file replacement or parent exit alone. Foreign/custom jobs, missing installation records, changed bindings, and insufficient authority remain explicit blockers.

Standard vendor timeout overrides, including Fedora's `10-timeout-abort.conf`, are accepted only under the matching `/usr/lib/systemd/user/service.d` or `/usr/lib/systemd/system/service.d` directory. Each file and its directories must be root-owned and not group/other writable; symlinks, special files and files exceeding 16 KiB are rejected. At most eight overrides may contain only comments, `[Service]` and `TimeoutStopFailureMode=abort`. Execution hooks, account/environment changes, other directives and custom locations remain unsupported. The exact validated path set and content checksums are rechecked before startup replacement. No override is removed, edited or reloaded to bypass validation.

Unmanaged Unix legacy launchers, especially an old container PID 1, cannot replace their process image at a child's request. Unsupported startup needs an explicit entry-point refresh and service cycle; containers need a current image. Until refreshed, new children report supervised runtime and cannot mark migration complete. No identity or workspace replacement is needed.

### Node binary retention

After a node connects and passes its startup policy checks, its updater removes obsolete checksum-named directories under `<dataDir>/updates`. When the running version and checksum match `runtime.json` and its selected source still verifies, cleanup also removes `runtime-previous.json` and the previous staged binary. Successful node updates do not keep old binaries for rollback. Failed startups retain the previous selection for manual recovery.

Cleanup preserves the running binary, the persisted current and requested selections, and any pending staged download. On Windows, both the stable runtime copy and its current versioned source remain. Cleanup runs at successful node startup, after processing a new update offer or an eligible retry, and every minute, including on existing installations updated through a managed child rollout. It verifies the running binary's checksum only when it has something to delete, so an idle node does not rehash it every minute. Duplicate offers and retries still in backoff do not trigger cleanup. Errors are logged and retried without changing update status. Unreadable runtime records stop deletion; cleanup skips symlink entries and is confined to node update storage. Installed launchers, Windows login bootstraps, CLI update backups, task/artifact state, and work files are not pruned.

### Gateway binary retention

After the gateway and online participants acknowledge the selected software and the gateway durably marks the rollout complete, it removes obsolete uploaded binaries and staged executable directories. Cleanup also runs for an already-complete deployment after gateway restart, and repeats every 15 minutes while that deployment remains complete. It also removes interrupted download temporaries not written for an hour. Once complete and cleaned, the rollout loop checks for late-joining nodes every 10 seconds instead of every second. Failed or unfinished rollouts are not pruned. Cleanup errors are logged and retried without changing the successful deployment's phase.

The gateway retains all assets in the selected manifest for offline nodes, binaries pinned by unexpired, unrevoked, unredeemed installation invitations, and its current, previous, and requested runtime selections. The previous gateway binary is retained for local recovery, not general download. Active uploads and release downloads are protected. Unreferenced uploads younger than one hour are kept so multi-platform pushes can finish before publication; abandoned uploads are removed after that grace. Unreadable runtime records stop cleanup before deletion. Removal is confined to checksum-named entries within gateway update storage and does not follow symlinks outside it.

Publication and invitation creation temporarily pin their selected binaries and verify their full size and SHA-256 outside the repository and gateway mutexes. Pins remain until the deployment or invitation reference commits, preventing cleanup from deleting a verified binary in that interval. Duplicate identical asset references within one verification pass are hashed once. Failed verification or persistence releases the temporary pins. Pins and file timestamps never replace checksum verification, and staged executables retain their existing verification before execution.

Authenticated node downloads serve only the selected manifest. Existing installation links use their separately pinned assets until redemption, revocation, or expiry. This policy applies to gateway storage, not [node-local binary retention](#node-binary-retention), operator-created upload folders, CLI update backups, or system journals.

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
| `GET /v1/updates` | Any valid key | Selected deployment ID, manifest, optional GitHub `releaseRepo`; no fleet or participant metadata |

Binaries are limited to 128 MiB each, manifests to six platform assets and 64 KiB, and key creation requests to 4 KiB. Invalid uploads never replace a verified binary. See [gateway binary retention](#gateway-binary-retention) for automatic cleanup and preserved references.

The existing Protobuf envelope carries `update.offer`, `update.prepare`, `update.commit`, `update.resume`, and `update.status`. Negotiated proxy coordination adds `update.proxy-prepare` and `update.proxy-park`; see the [protocol reference](protocol.md#gateway-proxy). Offers include optional `releaseRepo` for exact GitHub-first delivery with gateway fallback; old nodes ignore it. Nodes accept update commands only from the reserved gateway sender. The gateway never forwards peer-supplied update commands. Node reports describe their own progress and idle state, not another node's authority.
