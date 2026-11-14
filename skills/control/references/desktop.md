<!-- Managed by control -->

# Desktop interaction and setup

## GUI work, only when needed

Only when a task requires GUI interaction, describe the named User worker. Prefer `desktop.run` through `control_desktop` for private, masked snapshots and `setValue` with `secret`. Never replay uncertain actions.

Inspect `data.complete`, `truncated`, `nodes_observed`, and `subtree_truncated`. A partial observation cannot prove absence or authorize action replay.

If desktop support is missing, suggest installation and ask for explicit user permission. After approval, select the latest compatible release from [public upstream releases](https://github.com/vercel-labs/agent-desktop/releases). Download its OS/architecture `agent-desktop-ffi-*` archive, verify SHA-256 against `checksums.txt` from the same release, and extract with companions outside `workDir` into private storage. Require ABI major 6 or 7 and a 96-byte `AdAction`; reject incompatible libraries. Set absolute `desktop.library` in the User profile, preserve other settings, restart while idle, and check status before credential use. Control does not bundle/install FFI. OS grants require separate approval. See [native setup](https://github.com/koltyakov/control/blob/main/docs/desktop.md).

For an unconfigured [agent-desktop CLI](https://github.com/vercel-labs/agent-desktop), run `version`, `skills get desktop`, `skills get platform`, and `status` through worker `exec.run` with structured arguments. Read `data.content`; skip these checks and skill loads for non-GUI tasks. See the [GUI guide](https://github.com/koltyakov/control/blob/main/docs/desktop.md). `--headed` needs explicit user permission. `exec.run` output is not secret-masked: stop for manual login when protected input is unavailable, never retrieve credentials through a shell.

## Complete macOS permission setup

Treat permission readiness as part of an approved desktop installation, not an optional follow-up. Installation approval alone does not authorize OS grants. Before setup, explain which permissions the requested workflow needs and ask for approval to help enable the missing ones. Accessibility supports tree inspection and actions; Screen Recording is needed for OS screenshots and screenshot-backed workflows. For a full desktop setup, discuss both up front. Request Automation only when a required operation reports that it needs it; `automation: unknown` alone is not a blocker.

1. Check permissions through the named User node's configured backends: native `desktop.run` status and CLI `permissions` when configured. A successful command or installed binary does not prove the required permissions are granted. Do not use a Terminal or SSH check as evidence that the node service has access.
2. Identify the actual running User node executable from its process or startup definition, without printing configuration secrets. macOS can attribute helper access to the launching Control service. For a standalone launchd service, the grant may belong to the `control` binary rather than `agent-desktop`, its dylib, Terminal, Brave, or the orchestrator. Do not assume an installation path or substitute the System companion. If the attribution is unclear, inspect narrowly scoped TCC attribution logs for the current permission check.
3. With permission-change approval, open System Settings > Privacy & Security > Accessibility or Screen Recording as needed. Check the exact executable, not just a matching display name. Some upstream permission guides fall back to `agent-desktop` when there is no ancestor `.app` bundle; do not follow that suggestion blindly. A checked `agent-desktop` entry does not establish a grant for Control.
4. Automate supported Settings steps after approval: inspect the current pane, add the verified executable through the file picker, and enable its entry. Inspect the focused sheet/dialog rather than acting on the obscured Settings window. Use fresh masked observations after each mutation and preserve other entries. Obtain separate explicit approval before physical `--headed` input. If Accessibility is not yet usable, give the user the exact binary path and minimal manual bootstrap steps instead of attempting inaccessible UI automation.
5. Leave passwords, Touch ID, and other local authentication to the user. Never retrieve credentials, write the TCC database, reset unrelated grants, or switch to root/System to force access. If targeting fails or delivery is uncertain, stop automatic retries and give a short manual handoff with the verified path. After the user completes it, recheck rather than asking them to grant the same permission again.
6. Verify required grants through every configured desktop backend in fresh invocations. A `denied` or `unknown` preflight means access is not established; some upstream versions label any unsuccessful Screen Recording preflight `denied` even when no grant exists. It does not by itself prove the user refused consent. Restart the User node only if macOS requires it or fresh checks still indicate a cached state, after reconciling active work and releasing the lease. Do not claim permission-ready until required grants are verified; report optional missing permissions separately.

Do not take screenshots or collect screenshot traces merely to test permissions. If an authorized capture is part of the task, use a non-sensitive target only after the grant is verified. A missing Screen Recording grant does not prevent Accessibility-only inspection or prove a browser bridge is disconnected.

## Leased desktop checks

If the sequence holds a lease, use a leased inline task for each required fallback check, with a fresh ID for each new command:

```sh
control task start worker --wait '{"id":"FRESH_TASK_ID","capability":"exec.run","leaseId":"CURRENT_LEASE_ID","timeoutSeconds":120,"args":{"command":"agent-desktop","args":["version"]}}'
```

Replace the argument array for the remaining checks. Do not use a direct `exec.run` call or `control exec` under the lease. These raw CLI checks still lack desktop masking and the native lock; use them only for permitted non-credential work and obtain fresh native refs before protected input.

## Full agent-desktop commands

When the worker's `desktop.run` schema advertises `command: "cli"`, use `control_desktop` or `control call NAME desktop.run '{"command":"cli","args":["list-apps"]}'` for the full installed upstream command surface, not raw `exec.run`. This requires an absolute `desktop.cli` executable path; installing only the native library does not enable it. Installation still needs approval. Route `version`, `skills get desktop`, `skills get platform`, and `status` through this interface first, then read `data.content`. Use the installed command help rather than inventing flags. Discover exact app/window identities and system-tray surfaces before concluding that a hidden app requires manual opening.

Structured `args` are forwarded without a shell or command whitelist. Optional `stdin` carries non-secret text where upstream supports it; batch JSON is positional argv, not an invented stdin mode. CLI and native calls share the desktop slot, per-user lock, private default state and text redaction. Default-state refs can cross those two configured backends on the same profile; external CLI or explicit-session refs must not be mixed. Native `setValue` with `secret` remains the credential path. Never put credentials in CLI args, stdin, batch JSON or traces. Full features include upstream snapshots, discovery, actions, windows, screenshots, notifications, clipboard, waits, sessions, tracing and batches, subject to upstream platform support. `--headed`, clipboard use and sensitive captures need appropriate user authorization; a broad capability does not supply it. File bytes are not redacted. Use tracked tasks with declared workspace outputs for screenshots and exports. Inspect every batch entry; Control fails batches with failed/unstarted entries but retains upstream partial results. One-shot CLI invocations are cancellable and bounded; persistent stdio MCP serving uses the existing MCP-server configuration. Never replay uncertain effects.

## Cursor overlay for visible UI

For user-visible UI actions, prefer a presentation-only cursor overlay with `--size 0.75` so the user can follow the work. Skip the overlay for background-only accessibility-tree reads or semantic work that does not expose UI, unless the user requests it. Headless semantic actions can still expose UI; they do not need `--headed` to show an overlay.

When the installed CLI and platform support it, use a suitable existing session or create one with `session start --no-trace` before taking snapshots. Run `cursor-overlay enable --size 0.75` in that session and pass the returned `--session ID` explicitly to every overlay, snapshot and action command. Control strips inherited session variables. Keep native credential input sessionless with fresh native refs; never move credentials into CLI commands for visual feedback. Disable the overlay when returning to background-only work, and end only sessions you created when finished.

Check `data.rendered`: `false` means settings were saved but no cursor was drawn, as on unsupported Linux Wayland sessions. Missing overlay support does not block otherwise permitted UI work. Do not install software, change display servers, grant OS permissions, enable tracing or add `--headed` merely to show a cursor. Follow the installed platform guidance and user preferences.
