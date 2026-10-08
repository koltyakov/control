# Dashboard and system metrics

[Contributor guide](../AGENTS.md) · [Architecture](architecture.md) · [Protocol reference](protocol.md) · [Testing](testing.md)

## Open the dashboard

With a saved host profile, run:

```sh
control dashboard
```

The command reads gateway status directly, so the gateway's URL, running version, uptime, and resource metrics remain visible without a local node. With no saved profile:

```sh
control login --gateway https://control.example.com
control dashboard
```

Uptime separates units with spaces, such as `2m 54s`. Once it reaches an hour, it shows only hours and minutes, such as `2h 54m`.

The gateway URL comes from `CONTROL_GATEWAY`, the selected `--config` / `CONTROL_CONFIG` node profile, or the saved administration profile. Credentials use explicit global `--token`, then `CONTROL_USER_KEY`, `CONTROL_SUPERUSER_KEY`, `CONTROL_TOKEN`, a matching saved administration key, or the matching node profile token. Saved credentials are never sent to a different gateway. An unconfigured gateway produces a configuration error instead of defaulting to a loopback address. The gateway must support `GET /v1/status`.

With an account key, the dashboard reads fresh machine-health reports from the gateway even without a local node. Nodes publish active-work counts, lease state, and their cached resource sample every five seconds. These reports allow `idle`/`busy`, numeric `Work`, and current CPU/RAM values after the first usable sample.

An account client can collect detailed peer snapshots under its own authority, without borrowing worker permissions. This observes work submitted by any owner, including delegated tasks. Explicit local-API routing cannot independently request peer activity beyond discovery. A failed peer request preserves fresh gateway health. Activity records, recent completions, and connection counts still require authorized peer observation.

The pool contains only machines belonging to the authenticated user's fleet. Other users' machines, names, availability, resources, and activity are excluded at directory and transport authorization, including when a caller supplies a foreign machine ID. See [users and private fleets](users.md).

CLI/MCP processes on the orchestrator are authenticated clients, not fleet machines. Their live or disconnected sessions never add dashboard rows or machine counts. Gateway startup migrates the previous implementation's capability-free CLI registrations out of the machine catalog without changing task ownership. See [standalone clients](installation.md#standalone-cli-and-mcp).

Machines sort alphabetically by name, ignoring case, regardless of online state. Names that differ only by case use their original spelling as a deterministic tie-breaker. Displayed names and routing remain case-sensitive.

The live view shows the gateway first, followed by fleet sections:

- **Gateway** shows the connected URL, running version, and uptime on one line, followed by CPU, RAM, and disk on one line. RAM and disk bars show used capacity and percentages; wider terminals also show used/total bytes. Narrow terminals shorten the bars before hiding them. Fill updates directly from cached samples without animation. Light gray changes to yellow at 80% used and red at 95%; unknown capacity shows `?`. Details mode expands fleet tables and machine hardware, without adding gateway rows.
- **Machines** lists every registered machine, its availability, `Seen` age, active count, running version, OS, CPU utilization, RAM used/total, work-directory disk free space, and peer connection modes when observed directly. `Seen` shows `now` for online machines, a compact elapsed duration such as `30s`, `5m`, `2h`, or `3d` for offline machines, and `-` when unknown. Architecture and exact last-seen timestamps remain in JSON. Available RAM remains in JSON, without a duplicate table column. Compact gateway and machine versions show only the numeric release, with `*` for any suffix, such as `v0.1.0*` for `v0.1.0-3-gabc1234-dirty`. Details mode and plain-text output show full versions; JSON retains the original metadata.
- **Activity** shows queued/running tasks, synchronous providers, artifact operations and transfers, and open TCP tunnels. Rows include operation, state/phase, elapsed time, owner, peer, and ID. Transfers show bytes and total size; tunnels show transmitted and received bytes.
- **Recent**, hidden by default, can be toggled with `R`. It reports success, failure, or cancellation for operations that finish between dashboard polls, without copying results or logs.
- **Warnings**, visible only when machine observation fails, separates diagnostics from the machine table. Compact messages use plain wording such as `Activity request timed out`, highlight the machine name in yellow, and stay on one line. Long messages are shortened with an ellipsis. Details mode and plain-text output retain the original diagnostic; JSON is unchanged.
- **Hardware**, visible in details mode, shows OS distribution/version, CPU model and physical/logical core counts, plus disk usage for both the work and data directories.

The compact view sizes tables to the terminal. The `Node` column fits the longest displayed name, with a minimum width of 8 and a maximum of 18 terminal cells. Machine states reserve 8 cells; longer states such as `unavailable` are shortened with an ellipsis, with full values available in details mode. Leased machines show `reserved`, with their active count still visible in `Work`. Details mode and plain-text output retain `idle/leased` or `busy/leased` and show the lease owner and expiry when observed directly. The compact view omits that owner/expiry line. Other machine columns reserve space for their formatted values, keeping them aligned between refreshes. Tables hide columns strictly from right to left until the remaining prefix fits. Extremely narrow terminals shorten the remaining node column. Headers use short labels: `Work` is the active record count, `Op` is the operation, and `D/R` is the direct/relay session count. Empty activity and hardware sections are omitted. Sample times remain in JSON output but are omitted from the display.

The machine table order is `Node`, `State`, `Seen`, `Work`, `D/R`, `Tunnels`, `CPU`, `RAM`, `Disk free`, `P.Tun.`, `OS`, and `Version`. `Version` disappears first when space is limited, followed by `OS` and then the other columns from the right. `Tunnels` displays `↑N ↓N` for live forward connections and reverse listeners observed on that node, across all owners and orchestrator hosts. Forward connections are `tcp.open` or `tcp.accept` activities; reverse listeners are `tcp.listen` activities and count once regardless of their socket count. Recent completions do not count. Missing peer observation shows `-`, not zero. Idle local forward listeners cannot be observed on the worker.

The `P.Tun.` column means permanent tunnels. Always available in details and plain-text output, it separately counts this host login's retained forward/reverse definitions. It includes definitions awaiting recovery, excludes expired definitions, and remains available for offline targets. JSON keeps live counts in `tunnels` and local definition counts in `retainedTunnels`. See [persistent tunnels](tunnels.md).

Use `d` to toggle full details, including every column, full identifiers, and hardware information. Use `q`, Escape, or Ctrl+C to quit; `r` to request another snapshot; up/down or `j`/`k` to scroll vertically; left/right or `h`/`l` to scroll wide details; Page Up/Page Down to move a screen; Home/End or `g`/`G` to move to the beginning/end. The mouse wheel scrolls vertically in the main view. Escape cancels an active drag instead of quitting. Plain text and JSON retain full identifiers.

Drag with the left mouse button to select visible dashboard text, including in dialogs. Releasing the button automatically copies that selection to the local clipboard. The displayed screen stays fixed while dragging, even if polling finishes, then resumes on release. Selection uses terminal cell coordinates, preserves whole Unicode graphemes, and strips color codes and trailing line padding from copied text. A click without a drag does not copy anything. Clipboard operations are serialized and bounded to five seconds; new drags are ignored while a copy is pending. The footer reports success or failure, and `c` copies the last selection again or retries a failed copy. Any other key or a new selection clears the retained text. Resizing cancels an active drag.

Clipboard access uses the host's clipboard tool. Linux needs `wl-copy`, `xclip`, or `xsel` in a desktop session. With mouse reporting enabled, Shift-drag uses native terminal selection where the terminal supports that override. In VS Code on Windows/Linux, use Ctrl+Shift+C for native selections; Ctrl+C still quits when delivered to the dashboard. Use `control dashboard --once` for static output with ordinary terminal selection. The dashboard copies only the displayed selection, never a full snapshot or hidden columns.

Headings and machine states use static color, disabled by `NO_COLOR` or `TERM=dumb`. The `offline` state is red. Polling keeps the previous view visible with no spinner, blinking text, or flashing refresh indicator. A failed refresh adds a steady `[STALE]` tag beside `Up` and puts a concise yellow error in the existing footer, such as `Gateway refresh timed out`. Press `d` for the original diagnostic. It does not add a row or shift the tables. The tag clears after a successful refresh.

Press `a` to add a machine when logged in with a fleet-management key. Enter its name, or use `auto` or a blank name for the target machine's hostname. Select macOS, Windows, or Linux, then choose User context or System context before creating and copying the installation command. Escape returns to the previous step without creating an invitation.

User context is the wizard default. It runs with the installing user's files and credentials, but can go offline after logout. Linux user services need linger to remain online outside a login session. System context starts at boot and survives logout. Windows requires Administrator PowerShell and runs as LocalService. Linux/macOS commands use `sudo bash` and install a root system service without desktop access. The invitation records the chosen context, which takes precedence over `CONTROL_SERVICE_MODE`. The installer detects amd64 or arm64; invitations expire after 15 minutes by default. The gateway needs a published bundle with both architectures for that OS and support for the startup-context field. Automatic naming is resolved and signed on the target. See [startup contexts](installation.md#choose-a-startup-context).

The wizard uses the local OS clipboard. Linux needs `wl-copy`, `xclip`, or `xsel` in a desktop session. If copying fails, the command remains visible; `c` retries copying the same invitation. While the command is displayed, the dashboard checks that invitation's status in the authenticated fleet registry on its regular refresh cycle. After redemption, it shows that it is waiting for the machine to come online. The dialog closes only when a successful machine-list refresh started after redemption shows the redeemed identity online. Automatic names and replacement enrollments use the invitation ID and redeemed machine ID, so an existing same-name machine or an older snapshot cannot close the dialog early. Expiry, revocation, or a failed status request keeps the dialog open. Enter or Escape also closes the result. Invitation creation is never retried automatically after an uncertain response.

Press `m` to manage a registered machine with your account key. Select it with arrows and Enter, then press `r` to rename, `e` to enable, `d` to disable, or `u` to unregister. The rename form starts with its current alias; edit it or press Ctrl+U to clear it, then Enter to save. Escape goes back without submitting. Renaming changes routing and display names, including for offline machines, without restarting the node or interrupting work. Disable blocks new work while accepted work finishes. Unregister removes the registration immediately and prevents that identity from rejoining, including for connected older nodes. Current installations cancel work and uninstall startup and their unshared executable when they next contact the gateway, retaining configuration and work files. Older nodes may need local cleanup. Changes are requested once and never retried automatically after a lost response. See [registration management](installation.md#manage-registrations).

```sh
control dashboard --node worker,consumer --interval 5s --recent 10
control dashboard --once
control dashboard --json
```

`--interval` defaults to two seconds between activity snapshots, with a minimum of 250 milliseconds. `--timeout` defaults to 15 seconds. `--recent` selects 0..64 recent records per machine and defaults to five. `--node` accepts comma-separated names or stable IDs.

`--once` prints a plain-text snapshot, including recent completions. `--json` prints one structured snapshot, also retaining recent records. The live view hides recent completions until you press uppercase `R`; `d` does not change their visibility. Redirecting output or using non-terminal input also selects a one-shot view. These modes are suitable for scripts and do not enter the terminal's alternate screen.

## Availability is separate from observability

| Display | Meaning |
| --- | --- |
| `idle` | Registered, online, and responding, with no observed active work |
| `busy` | Registered, online, and responding, with active work |
| `reserved` | Execution capacity has an active or still-busy exclusive lease; details and plain text show `/leased` after `idle` or `busy` |
| `online` | Connected to the gateway, but no fresh owner health or peer observation is available; `Work` shows `-` |
| `disabled` | Registered but excluded from scheduling and new execution; it can remain connected for monitoring |
| `disabling` / `enabling` | The owner changed policy and the connected node has not acknowledged it yet |
| `offline` | The gateway reports no current node connection; the last-seen time remains visible |
| `update` | A managed update is in progress, or the node is within its one-minute restart display grace |
| `unavailable` | The gateway reports the machine online, but its activity snapshot failed, timed out, was denied, or is unsupported |

The header distinguishes registered, online, and successfully observed machines. The gateway is separate from those fleet counts. An unavailable machine has an unknown activity count, not a zero. If the gateway refresh fails, the live view keeps the previous snapshot and marks it `STALE`. Failure to observe peer activity does not discard fresh gateway status. CPU and memory usage do not determine the machine's online status or whether it can accept more work.

Managed-update restarts show `update` instead of `offline` for a one-minute disconnect grace. The grace survives node reconnection and gateway restart, but ends early when the node confirms the selected version or checksum and reports that it has left maintenance. Normal `idle` or `busy` observation then returns immediately. Nodes still paused for the rollout remain `update`; machines that have not returned show `offline` after grace expires. Progress from an older deployment cannot keep a machine in `update`. The actual online flag, last-seen time, routing, and fleet counts still reflect connection presence. Peer observations cannot replace an active gateway `update` status.

Owner reports expire after 20 seconds without receipt and disappear on disconnect or gateway restart. An older node or gateway may provide only registration-time metrics. Update both to enable health reports. Fresh reports use JSON status `summary`; authorized peer observations use `ready`. Both display `idle` or `busy`. The first CPU sample is a baseline, so CPU becomes available after the next collection, normally within 15 seconds of startup.

Polling is bounded and concurrent, with up to eight nodes at a time. Each node request has a six-second deadline within a ten-second aggregation deadline. A failing node does not discard snapshots from healthy peers. The dashboard reads periodic snapshots rather than consuming a continuous event stream.

## Resource sampling

The gateway samples its host on startup and every 15 seconds. Status requests only read that cache. CPU usage requires two samples; the initial reading is unknown. Gateway metrics describe the whole host-visible environment, not just the Control process. The disk entry covers the gateway state filesystem, with the private path replaced by `state`. Raw collection errors are replaced by generic messages. Sampling timestamps remain available through JSON. These metrics are visible to any authenticated fleet member; other fleets' registrations and activity are excluded.

Each node collects system information locally using cross-platform Go APIs:

- OS, architecture, distribution/version, kernel version, and uptime.
- CPU model, physical and logical CPU counts, and utilization averaged across logical CPUs.
- Total and used RAM, plus available RAM. Available RAM includes memory the OS can reclaim, rather than only completely unused pages.
- Total, used, and free space on the filesystems containing `workDir` and `dataDir`. Those paths may refer to the same filesystem; their capacities should not be summed.

Collection happens on startup, periodically, after execution/agent tasks or other tasks lasting over a second finish, and on an explicit refresh request. Completion triggers are coalesced and do not delay task completion. CPU utilization is calculated between samples; the first sample establishes the baseline and shows unknown usage rather than a false zero.

```json
{
  "metricsIntervalSeconds": 15
}
```

Omitting the setting or using zero selects the 15-second default. Values from 1 to 3600 select a periodic interval in seconds. `-1` disables periodic sampling; startup, completion-triggered, and requested sampling still work.

Read the cache or request a new sample:

```sh
control system worker
control system worker --refresh
control call worker system.info '{"refresh":true}'
```

The dashboard never initiates hardware collection when it refreshes. It reads the latest cached sample. `control machines` contains the registration-time system snapshot; use `system.info`, `node.describe`, or the dashboard for the current cache. Offline machines show `-` for CPU usage, RAM usage, and disk free space. Details mode retains their static hardware and disk capacity, but hides used/free disk readings and sample errors. Unavailable online machines may show the older registration sample. JSON retains cached metrics and their sampling timestamp, including for offline machines.

Collection errors are included in the system snapshot. The CPU field is absent until a usable delta is available. A collection timeout leaves the previous cached sample intact. These are host-visible metrics, not per-task consumption or container resource quotas. Docker Desktop reports the environment visible inside its Linux containers.

## Authorization and data boundaries

Gateway machine-health summaries are available only to the owning account key, or the superuser for its legacy fleet. They expose aggregate work count, lease presence, and cached resources. They contain no task records, operation names, owner IDs, arguments, credentials, or private filesystem paths. Common node keys permit discovery only. Owner health grants no execution or task-control access.

`activities.list` is a read-only, node-wide metadata operation. A caller allowed to use it can observe work from all owners on that node. It does not grant permission to read another owner's task results/logs, cancel their tasks, invoke providers, or access files.

Account clients can observe peers when their access rules permit it. Workers need instruction-bound delegation for observation beyond discovery. For a restricted node, add permissions for your orchestrator:

```json
{
  "allow": {
    "ORCHESTRATOR_STABLE_ID": ["activities.list", "system.info", "node.describe"]
  }
}
```

The account client implements `activities.pool` by requesting `activities.list` from peers under its own authority and each peer's access rules. The node's local API also exposes aggregation, but its worker identity cannot independently observe peers beyond discovery. The aggregation method is not a peer RPC.

Activity records contain operational metadata and progress. They do not include command arguments, prompts, environment values, credentials, results, or log contents. Observational requests are excluded from activity tracking so the dashboard does not list its own polling traffic.

Sender and receiver transfers/tunnels can both appear because they are separate activities on separate nodes. Tasks can also have child transfer rows associated through `taskId`. The in-flight total counts these activities, not just distinct jobs.

Ordinary forwards create worker activity only while a TCP socket is connected. An unused local listener belongs to the CLI/MCP process, not the worker, so it does not make that worker busy. Reverse forwards keep a `tcp.listen` activity visible for the entire remote listener lifetime, including idle periods and after individual sockets close. Its traffic counters accumulate across those sockets. Account-login dashboards collect these records without a local node or node API token, subject to `activities.list` permission.

## Retention and limits

Tasks retain their existing persisted lifecycle. Active non-task records and the last 64 non-task completions per node are held in memory and disappear on restart. Recent task completions are selected from persisted task records. This is an operational view, not a durable audit log.

Each node tracks up to 2,048 non-task activities in addition to its accepted tasks. Additional active records are counted as omitted. Aggregated snapshots include at most 4,096 active records and 512 recent records; the dashboard reports omitted active detail instead of silently claiming complete coverage. Filter by node to inspect a busy subset.

For AI clients, `control_activities` exposes the peer activity snapshot under the requesting account client's authority, and `control_system` reads or refreshes a selected node's resource sample. Direct gateway status is available through authenticated `GET /v1/status`; it does not require MCP or a running local node.
