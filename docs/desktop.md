# agent-desktop through Control

[README](../README.md) · [Worker-local secrets](secrets.md) · [Protocol](protocol.md) · [Testing](testing.md)

`desktop.run` operates the full installed agent-desktop CLI and also offers protected native credential input. Both backends share Control's desktop admission slot, per-user lock, private default state, task ownership, cancellation and text redaction. The node advertises only the configured backends in its capability schema. Control remains CGO-free on Windows, macOS and Linux.

## Full upstream command surface

Upgrade the execution node and the orchestrator's CLI/MCP installation before using the new request shape. Older `control_desktop` clients validate only the native subset and reject CLI requests even after the worker is upgraded. Managed fleet updates do not replace standalone CLI/MCP installations.

Configure `desktop.cli` with the absolute path of the native agent-desktop executable, retaining its upstream companion files. Install only after approval, using a compatible public [upstream release](https://github.com/vercel-labs/agent-desktop/releases) for the worker's OS/architecture and verifying its archive against that release's `checksums.txt`. Use private User storage outside `workDir`, preserve other profile settings, and restart while idle. Control does not bundle or automatically install this dependency. A library-only installation provides the native commands below, not the full CLI. The CLI and library can be installed together:

```json
{
  "desktop": {
    "cli": "/absolute/path/to/agent-desktop",
    "library": "/absolute/path/to/libagent_desktop_ffi.dylib"
  }
}
```

Use the actual platform paths, including `agent-desktop.exe` and the DLL on Windows. `cli` alone is supported when protected native input is not needed. Do not point `desktop.command` at the upstream CLI; that legacy field names a private-pipe native helper.

```sh
control call worker desktop.run '{"command":"cli","args":["version"]}'
control call worker desktop.run '{"command":"cli","args":["skills","get","desktop"]}'
control call worker desktop.run '{"command":"cli","args":["skills","get","platform"]}'
control call worker desktop.run '{"command":"cli","args":["status"]}'
control call worker desktop.run '{"command":"cli","args":["list-apps"]}'
control call worker desktop.run '{"command":"cli","args":["list-windows"]}'
control call worker desktop.run '{"command":"cli","args":["snapshot","--surface","system-tray"]}'
```

Read the returned skills' `data.content` and installed command help before acting. Use observed app identities and window IDs, not guessed process names. For a hidden tray app, inspect the supported shell surface before asking the person to open it. Inspect completeness metadata before concluding a control is absent. Physical `--headed` input, clipboard operations, sensitive captures and OS permission changes still require the appropriate user approval.

`command: "cli"` forwards its structured `args` verbatim to the configured executable, without a shell or a Control-maintained upstream command whitelist. It supports all installed upstream features, including app/window/display discovery and management, system surfaces, snapshots and drill-down, find/get/is, ref actions, keyboard/mouse, screenshots, notifications, clipboard, waits, sessions, traces, cursor overlay, skills and batches. New upstream commands do not need new Control bindings. Platform limitations and upstream refusals remain in effect; forwarding a reserved held-key command does not make upstream implement it.

Optional `stdin` carries non-secret text for upstream commands that accept it. Upstream batch JSON remains a positional argument; Control does not add stdin support to commands that lack it. Arguments and stdin are retained in task specifications: never put credentials there. Use native `setValue` with worker-local secret names instead. Both configured backends use the same `AGENT_DESKTOP_HOME` default namespace, so fresh default-session CLI refs can be used for native protected input on the same profile with compatible versions. Inherited `AGENT_DESKTOP_*` variables are removed; only Control's private home is set. Explicit CLI sessions retain their upstream semantics and must not be mixed with sessionless native refs. Raw external CLI invocations use a different state namespace unless configured otherwise and do not share Control's lock.

The CLI runs once per invocation under the caller/task deadline, with cancellation terminating its process tree. Session commands persist upstream state normally, but `desktop.run` is not a persistent stdio/MCP stream. Use the existing [worker MCP configuration](protocol.md#ai-cli-and-mcp-configuration) to host `agent-desktop mcp`. That separate MCP route does not inherit desktop.run's native credential or desktop-lock contract.

CLI responses retain upstream JSON envelopes and add `process: {exitCode, stderr}` with redacted stderr. Non-JSON text such as help is wrapped in `data.stdout` on successful exit or `error.stdout` on failure. A nonzero process exit or upstream `ok: false` fails execution, preserving sanitized output for tracked tasks. Upstream batches can report outer `ok: true` despite failed or unstarted entries; Control also fails those invocations while preserving the original envelope and partial results so dependent workflows do not continue as if every entry succeeded. Raw output never streams to task logs. Oversized output, cancellation and startup failure return fixed diagnostics without raw text. No command is retried automatically.

Limits are 256 arguments, 1 MiB of combined decoded UTF-8 argv/stdin bytes, no NUL, and 2 MiB each of captured stdout/stderr. Existing serialized task-argument bounds and lower OS process-argument limits still apply. Use stdin only where the installed upstream command supports it. Native calls retain their two-minute deadline; CLI calls use their caller/task deadline so upstream waits and batches are not limited to two minutes. Large files belong in task outputs, not JSON/base64 results:

```json
{
  "id": "requested-desktop-capture",
  "capability": "desktop.run",
  "args": { "command": "cli", "args": ["screenshot", "screen.png", "--screen", "0"] },
  "outputs": ["screen.png"],
  "timeoutSeconds": 120
}
```

Run this example only for a user-authorized non-sensitive capture. The CLI runs in the task workspace; declared outputs use normal owner-bound artifact publication and direct transfers. Synchronous calls do not automatically import files. Text redaction does not mask image/file bytes, sanitize trace exports, or prevent a trusted upstream process from writing private data to disk. Do not capture credentials or publish sensitive traces. Use leases for multi-command desktop sequences and reconcile task IDs after uncertain outcomes.

## Cursor overlay for visible UI

The default MCP and installed skill instructions prefer a presentation-only cursor overlay at `--size 0.75` for user-visible UI actions, so the user can follow the work. Background-only accessibility-tree reads or semantic work that does not expose UI do not need an overlay unless the user requests it. A headless semantic action can still change visible UI; the overlay does not require physical input or `--headed`. This is agent guidance, not an automatic runtime setting.

Use the installed CLI/platform guidance and command help. When supported, reuse a suitable upstream session or create one without tracing before taking snapshots:

```sh
control call worker desktop.run '{"command":"cli","args":["session","start","--no-trace"]}'
```

Read the returned `data.session_id`, then substitute it for `SESSION_ID` in subsequent requests:

```sh
control call worker desktop.run '{"command":"cli","args":["--session","SESSION_ID","cursor-overlay","enable","--size","0.75"]}'
```

Pass `--session ID` explicitly to every overlay, snapshot and action command, including commands submitted as tracked tasks. Control strips inherited session variables. Session-scoped CLI refs cannot be used by sessionless native credential input. Keep credentials on the protected native path and obtain fresh native refs; do not move credential input into the CLI for visual feedback.

Check `data.rendered` rather than equating an enabled setting with a visible cursor. In upstream v1.2.0, Linux X11 supports rendering while Wayland saves the setting with `rendered: false`. Missing overlay support does not block otherwise permitted UI work. Do not install dependencies, change display servers, grant OS permissions, enable tracing or switch to `--headed` merely to make the cursor visible.

Disable the overlay when returning to background-only work. End sessions you created when finished, but do not end a user-owned session. Respect user preferences and the installed platform's limits. Native-only profiles do not expose the cursor-overlay command.

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

While holding a lease, synchronous capability calls are rejected. Submit each request inline with the current lease ID and wait in the same invocation, rather than creating a request file and separately waiting:

```sh
control task start worker --wait '{"id":"FRESH_TASK_ID","capability":"desktop.run","leaseId":"CURRENT_LEASE_ID","timeoutSeconds":120,"args":{"command":"status"}}'
```

Use a fresh ID for each new command, inspect the result, and obtain fresh masked observations after mutations before choosing the next action. Load version-matched CLI skills once per worker/backend/version in a session, but check current readiness for each sequence. Group only independent read-only checks where supported. A wait failure does not cancel or retry the task; reconcile the original ID and live state before proceeding. Fallback raw `exec.run` checks under the lease also require inline task specifications with that `leaseId`; `control exec` has no lease option. Their output is still unmasked, so credential safeguards below remain unchanged.

| Command | Input |
| --- | --- |
| `status` | No additional fields. Returns masked platform, permissions, and version only |
| `snapshot` | Required `app`, optional `maxDepth` 1..32, default 10. Returns the native snapshot envelope after text masking |
| `setValue` | Required qualified `ref` and exactly one of `secret` or `text`. Headless semantic value setting only |
| `click`, `clear` | Required qualified `ref`. Headless ref action only |

Names refer to the [existing worker secret store](secrets.md#enter-credentials-outside-chat). The user enters private usernames and passwords outside chat. Missing secrets fail before starting the helper. Literal `text` is for non-secret content only, at most 4,096 UTF-8 bytes without NUL. No screenshots, clipboard commands, key synthesis, headed fallback, tracing, arbitrary CLI arguments, or application launching are exposed.

The native adapter uses `<dataDir>/desktop` as its private agent-desktop state root, shared with the configured CLI's default state. It rejects state within `workDir`, including symlink-resolved paths, and uses `0700` on Unix. Windows depends on the profile's ACLs. Refs from ordinary external CLI sessions or another profile do not belong to this adapter. Control strips inherited and configured `AGENT_DESKTOP_*` variables from the helper environment; the helper sets only its private home and creates an adapter without a tracing session or log callback.

## Credential and output handling

Only the worker resolves secret names. Resolved text goes into a bounded binary stdin request, never the helper's command arguments, environment, task specification, or an input spool file. The helper calls `ad_execute_by_ref_timeout` with `SET_VALUE` and `HEADLESS` policy, so it does not use the clipboard or synthesize keystrokes as a fallback.

All responses, including status, later snapshots, action post-state, and error envelopes, mask the invocation's configured secret values and common encodings. Private native state paths are masked too; successful status responses omit state-root and artifact metadata entirely. Raw stdout/stderr never stream into task logs, even on calls without a `secret` field. Malformed, oversized, failed, or cancelled helper output produces a fixed diagnostic without native output. Valid native command failures retain their sanitized envelope in tracked task results and fail the task. Synchronous errors return a generic diagnostic; use tracked tasks to retain partial results and native delivery details.

The helper accepts one `CDSK0001` packet: eight ASCII magic bytes, then five big-endian uint32-length-prefixed UTF-8 strings in order `command`, `app`, `ref`, resolved `text`, and absolute state root, followed by a big-endian uint32 depth and EOF. Field byte caps are 16, 4,096, 128, 4,096, and 32,768 respectively. Stdout contains one native JSON envelope of at most 2 MiB; stderr is discarded. Built-in mode runs `control __desktop LIBRARY` before profile, account, or service initialization. The library path is the mode's only argument. Input validation completes before library loading. Infrastructure failures and recovered Go panics return a fixed `NATIVE_HELPER_FAILED` envelope without input, paths, or panic values. Native crashes and hangs remain subprocess failures handled by the worker. Neither component retries side effects.

## Limits

This avoids accidental credential exposure to the coordinating AI context; it is not an execution sandbox or protection from a compromised worker. The helper, native library, and target application necessarily hold credential values in memory. Native refmaps can retain observed private text outside Control's task storage. Protect that private directory and backups. Control does not promise secure memory erasure, encrypt the store, restrict credentials to approved applications, or mask arbitrary transformations. Rotated or deleted secret values are not available to mask later observations. Keep these credentials away from unrestricted providers, raw CLI observations, screenshots, and public logs.

If the application cannot accept headless value setting, stop for manual login or an explicitly approved alternative. Never retrieve the value through a shell or automatically repeat a failed action. Cancellation and node restarts do not undo UI effects.

## Use agent-desktop on a worker

Prefer the [full desktop.run CLI route](#full-upstream-command-surface) when configured. The upstream CLI is separate from `desktop.library`; a managed Control update does not install it. For an existing CLI not configured as `desktop.cli`, ordinary execution remains available. Discover the requested User worker with `node.describe`, then check the CLI and load its version-matched instructions on that same worker:

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

Broader upstream CLI operations through ordinary `exec.run` have no Control secret resolution, output masking, automatic screenshot-artifact conversion, or participation in the desktop lock/pool. Use configured `desktop.cli` through `desktop.run` for serialized, text-masked full control and declared task outputs for files. Credential values still belong only in native secret-reference input, never CLI args/stdin. Do not expand permissions or expose credentials to bridge a missing feature.
