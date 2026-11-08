# Native agent-desktop adapter

[README](../README.md) · [Worker-local secrets](secrets.md) · [Protocol](protocol.md) · [Testing](testing.md)

`desktop.run` is an opt-in worker-local adapter for agent-desktop's native C library. Control launches its own executable in an isolated Go helper mode using purego. Only that subprocess loads the library and receives requests through an anonymous stdin pipe. Control stays CGO-free on Windows, macOS, and Linux, on amd64 and arm64. No C++ compiler, Python runtime, or agent-desktop CLI is required.

## Enable on a worker

Install only when a GUI task needs it, after user approval. Select the latest compatible release from the public [upstream releases](https://github.com/vercel-labs/agent-desktop/releases). Download its `agent-desktop-ffi-*` archive for the worker's OS/architecture and verify SHA-256 against `checksums.txt` from that same release. Extract the full package outside `workDir` into a directory private to the worker's OS user, retaining companion executables and the license. No GitHub login, compiler, Python runtime, or upstream CLI is required. Control supports ABI major 6 or 7 with a 96-byte `AdAction` layout; the isolated helper rejects other versions and layout mismatches at runtime. A newer release number alone does not establish compatibility. Do not bypass these checks or use credentials until native status succeeds. See [library setup](../examples/agent-desktop/README.md).

[agent-desktop v1.2.0](https://github.com/vercel-labs/agent-desktop/releases/tag/v1.2.0) retains ABI 7 and the 96-byte action layout, including native Windows ARM64 FFI assets. Control's status, snapshot, and qualified-ref action bindings remain unchanged. Control reads `ad_abi_version`, accepts only 6 or 7, and checks `ad_init` with that exact major plus `ad_action_size` before creating an adapter. Existing ABI 6 and v1.1.0 libraries remain supported. After changing the library version, take fresh snapshots before using refs. Upstream's new CLI MCP server and stdin text input do not change Control's private-pipe adapter or enable additional commands.

Configure the library's absolute path in the worker's User profile:

```json
{
  "desktop": {
    "library": "/absolute/path/to/libagent_desktop_ffi.dylib"
  }
}
```

Use `lib/libagent_desktop_ffi.so` on Linux or `lib/agent_desktop_ffi.dll` on Windows. Merge the fragment into the existing profile without replacing other settings, then restart while idle and check `desktop.run` status. Without `desktop`, the node does not advertise `desktop.run`. Request any missing OS permissions separately; installation does not grant them.

Control releases, enrollment, startup, and managed updates do not download, bundle, or install the native library. `make update` ships only the Go helper code. An existing compatible library can be reused across Control releases. Existing explicit `desktop.command` helpers remain supported using the same private-pipe protocol. Do not combine `library` with `command`, `args`, `env`, or `dir`.

Run as the logged-in desktop user, not a Hybrid System companion or Windows Session 0 service. Native platform permissions and dependencies still apply. No operation unlocks a desktop or bypasses OS access checks. Trust the helper and library with the worker's credentials and normal OS authority.

## Observe, fill by reference, verify

Discover `desktop.run` with `node.describe`, then:

```sh
control call worker desktop.run '{"command":"status"}'
control call worker desktop.run '{"command":"snapshot","app":"Example login"}'
control call worker desktop.run '{"command":"setValue","ref":"@SNAPSHOT:e1","secret":"app.username"}'
control call worker desktop.run '{"command":"setValue","ref":"@SNAPSHOT:e2","secret":"app.password"}'
control call worker desktop.run '{"command":"click","ref":"@SNAPSHOT:e3"}'
control call worker desktop.run '{"command":"snapshot","app":"Example login"}'
```

Replace the app name and refs with actual observations. Re-snapshot when UI changes invalidate refs. The C API targets the focused window of the named app; explicit window IDs, skeletons, and root-scoped traversal are not available in this ABI. Ambiguous or stale targets must be resolved through a new observation, not guessed or retried blindly.

In v1.2.0, a snapshot that exhausts its observation budget succeeds with `data.complete: false` and the partial tree, rather than failing with `TIMEOUT`. Control preserves `complete`, `truncated`, `nodes_observed`, and per-node `subtree_truncated` while masking text. Check completeness before concluding that an element is absent. A partial snapshot does not authorize replaying an action or using stale refs.

MCP `control_desktop` takes `node` and a `request` object with the same fields. Generic CLI/MCP calls and tracked `desktop.run` tasks use the same adapter. Use explicit task IDs for reconciliation and node leases for multi-command sequences. Built-in desktop invocations use one admission slot per node, separate from general I/O, and descriptions/activity snapshots report desktop usage and queues. Each admitted invocation still acquires Control's shared per-user desktop file lock across profiles and has a two-minute deadline including that lock wait. The lock retains its historical `control/rpa.lock` path under the OS user configuration directory so older and newer profiles remain serialized. This does not exclude humans, unrelated desktop software, or raw agent-desktop CLI calls.

| Command | Input |
| --- | --- |
| `status` | No additional fields. Returns masked platform, permissions, and version only |
| `snapshot` | Required `app`, optional `maxDepth` 1..32, default 10. Returns the native snapshot envelope after text masking |
| `setValue` | Required qualified `ref` and exactly one of `secret` or `text`. Headless semantic value setting only |
| `click`, `clear` | Required qualified `ref`. Headless ref action only |

Names refer to the [existing worker secret store](secrets.md#enter-credentials-outside-chat). The user enters private usernames and passwords outside chat. Missing secrets fail before starting the helper. Literal `text` is for non-secret content only, at most 4,096 UTF-8 bytes without NUL. No screenshots, clipboard commands, key synthesis, headed fallback, tracing, arbitrary CLI arguments, or application launching are exposed.

The native adapter uses `<dataDir>/desktop` as its private agent-desktop state root. It rejects state within `workDir`, including symlink-resolved paths, and uses `0700` on Unix. Windows depends on the profile's ACLs. Native snapshots persist refmaps there, so refs from ordinary CLI sessions or another profile do not belong to this adapter. Control strips inherited and configured `AGENT_DESKTOP_*` variables from the helper environment; the helper sets only its private home and creates an adapter without a tracing session or log callback.

## Credential and output handling

Only the worker resolves secret names. Resolved text goes into a bounded binary stdin request, never the helper's command arguments, environment, task specification, or an input spool file. The helper calls `ad_execute_by_ref_timeout` with `SET_VALUE` and `HEADLESS` policy, so it does not use the clipboard or synthesize keystrokes as a fallback.

All responses, including status, later snapshots, action post-state, and error envelopes, mask the invocation's configured secret values and common encodings. Private native state paths are masked too; successful status responses omit state-root and artifact metadata entirely. Raw stdout/stderr never stream into task logs, even on calls without a `secret` field. Malformed, oversized, failed, or cancelled helper output produces a fixed diagnostic without native output. Valid native command failures retain their sanitized envelope in tracked task results and fail the task. Synchronous errors return a generic diagnostic; use tracked tasks to retain partial results and native delivery details.

The helper accepts one `CDSK0001` packet: eight ASCII magic bytes, then five big-endian uint32-length-prefixed UTF-8 strings in order `command`, `app`, `ref`, resolved `text`, and absolute state root, followed by a big-endian uint32 depth and EOF. Field byte caps are 16, 4,096, 128, 4,096, and 32,768 respectively. Stdout contains one native JSON envelope of at most 2 MiB; stderr is discarded. Built-in mode runs `control __desktop LIBRARY` before profile, account, or service initialization. The library path is the mode's only argument. Input validation completes before library loading. Infrastructure failures and recovered Go panics return a fixed `NATIVE_HELPER_FAILED` envelope without input, paths, or panic values. Native crashes and hangs remain subprocess failures handled by the worker. Neither component retries side effects.

## Limits

This avoids accidental credential exposure to the coordinating AI context; it is not an execution sandbox or protection from a compromised worker. The helper, native library, and target application necessarily hold credential values in memory. Native refmaps can retain observed private text outside Control's task storage. Protect that private directory and backups. Control does not promise secure memory erasure, encrypt the store, restrict credentials to approved applications, or mask arbitrary transformations. Rotated or deleted secret values are not available to mask later observations. Keep these credentials away from unrestricted providers, raw CLI observations, screenshots, and public logs.

If the application cannot accept headless value setting, stop for manual login or an explicitly approved alternative. Never retrieve the value through a shell or automatically repeat a failed action. Cancellation and node restarts do not undo UI effects.

## Use agent-desktop on a worker

The upstream CLI is optional and separate from `desktop.library`. For broader GUI operations, discover the requested User worker with `node.describe`, then check the CLI and load its version-matched instructions on that same worker:

```sh
control exec worker -- agent-desktop version
control exec worker -- agent-desktop skills get desktop
control exec worker -- agent-desktop skills get platform
control exec worker -- agent-desktop status
```

MCP uses `control_call` with `node: "worker"`, `method: "exec.run"`, and `params: {"command":"agent-desktop","args":["version"]}`. Change the argument array for the other commands. Read the returned skills' `data.content` before acting. Keep all subsequent CLI commands routed to that worker through Control with structured arguments and fresh snapshot-qualified refs. CLI refs cannot be used by the native adapter, or vice versa. Never replay uncertain effects. Physical input with `--headed` requires explicit user permission, not an automatic fallback after a semantic failure.

`version` establishes presence, not desktop readiness. `status` reports platform and permissions. Missing executables, denied execution, unavailable desktop sessions, and missing OS grants are blockers, not permission to switch to System. Ask before installing the CLI or granting permissions. Use an already configured native adapter when suitable. Control does not automatically install either dependency. The CLI's embedded skills need no separate harness installation; upstream's optional harness skill is available with `npx skills add https://github.com/vercel-labs/agent-desktop --skill agent-desktop`.

For login, use native `setValue` with worker-local secret names for both private usernames and passwords. Ordinary CLI text commands do not resolve Control secrets, and `exec.run` output is not secret-masked. Never read the store or construct shell, environment, clipboard, script, or prompt credential substitutions. While credentials may be visible, use masked native observations, not raw CLI snapshots, field reads, screenshots, clipboard reads, or traces. Verify non-sensitive application state before resuming raw observations. If protected input is unavailable or unsupported, stop for manual login or approved setup.

## Migrate from RPA

`rpa.run`, MCP `control_rpa`, and the supplied Python/PyAutoGUI helper are removed. The `rpa` configuration field is rejected whenever present, even as `null` or alongside `desktop`. No automatic conversion or execution fallback occurs.

1. Remove the top-level `rpa` field from each affected node profile before starting the new version. Configure an approved native FFI library with `desktop.library` as described above, then restart while idle and verify native status.
2. Replace caller/tool references, access rules, runbook requirements, and future task specifications explicitly. Use `desktop.run` with its command object, or `control_desktop` with `node` and `request`. Obtain fresh refs from the adapter's snapshots; old RPA selectors are not native refs.
3. Reconcile retained RPA task IDs without replaying them. Existing task history is not rewritten. Restart still interrupts unfinished tasks rather than resubmitting them under another capability. Remove a dedicated RPA virtual environment/helper installation only after confirming nothing else uses it. Control's native adapter needs no Python runtime.

The replacement is intentionally narrower:

| Removed RPA feature | Current behavior |
| --- | --- |
| Exact selectors from desktop-wide inspection | App-scoped native snapshots with qualified refs, not compatible selector syntax |
| Secret-reference `setValue` and keystroke `type` | Native headless `setValue` only; no secret typing or physical-input fallback |
| Screenshot PNG artifacts | No native screenshot command or automatic screenshot-artifact import |
| Focus, pointer movement, coordinate clicks, drag, scroll, key chords and waits | Not exposed by `desktop.run`; use supported upstream CLI operations separately with required approval, or manual interaction |
| 1..100-action batches and ordered partial results | One native command per invocation; use leased tracked tasks for exclusive multi-command sequences on one node |
| Text masking and desktop serialization | Native responses remain masked and raw logs suppressed; the same per-user lock and one-slot desktop pool remain |

Broader upstream CLI operations through `exec.run` are not protected equivalents. They have no Control secret resolution, output masking, automatic screenshot-artifact conversion, or participation in the native desktop lock/pool. Coordinate one writer explicitly and follow the installed CLI skills. Do not expand permissions or expose credentials to bridge a missing feature.
