# Native agent-desktop adapter

[GUI automation](rpa.md) · [Worker-local secrets](secrets.md) · [Protocol](protocol.md) · [Testing](testing.md)

`desktop.run` is an opt-in worker-local adapter for agent-desktop's native C library. Control launches its own executable in an isolated Go helper mode using purego. Only that subprocess loads the library and receives requests through an anonymous stdin pipe. Control stays CGO-free on Windows, macOS, and Linux, on amd64 and arm64. No C++ compiler, Python runtime, or agent-desktop CLI is required.

## Enable on a worker

Install only when a GUI task needs it, after user approval. Select the latest compatible release from the public [upstream releases](https://github.com/vercel-labs/agent-desktop/releases). Download its `agent-desktop-ffi-*` archive for the worker's OS/architecture and verify SHA-256 against `checksums.txt` from that same release. Extract the full package outside `workDir` into a directory private to the worker's OS user, retaining companion executables and the license. No GitHub login, compiler, Python runtime, or upstream CLI is required. Control requires ABI major 6 and a 96-byte `AdAction` layout; the isolated helper rejects mismatches at runtime. A newer release number alone does not establish compatibility. Do not bypass these checks or use credentials until native status succeeds. See [library setup](../examples/agent-desktop/README.md).

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

MCP `control_desktop` takes `node` and a `request` object with the same fields. Generic CLI/MCP calls and tracked `desktop.run` tasks use the same adapter. Use explicit task IDs for reconciliation and node leases for multi-command sequences. Each invocation shares Control's per-user RPA lock and has a two-minute deadline including lock wait. This does not exclude humans, unrelated desktop software, or raw agent-desktop CLI calls.

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
