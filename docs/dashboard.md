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

The gateway URL comes from `CONTROL_GATEWAY`, the selected `--config` / `CONTROL_CONFIG` node profile, or the saved administration profile. Credentials use explicit global `--token`, then `CONTROL_USER_KEY`, `CONTROL_SUPERUSER_KEY`, `CONTROL_TOKEN`, a matching saved administration key, or the matching node profile token. Saved credentials are never sent to a different gateway. An unconfigured gateway produces a configuration error instead of defaulting to a loopback address. The gateway must support `GET /v1/status`.

With an account key, the dashboard reads fresh machine-health reports from the gateway even without a local node. Nodes publish active-work counts, lease state, and their cached resource sample every five seconds. These reports allow `idle`/`busy`, numeric `Work`, and current CPU/RAM values after the first usable sample.

When a local node is available, the command uses the same `CONTROL_API` and local token as the CLI and MCP server to collect detailed peer snapshots. This observes work submitted by any peer, including tasks delegated by another worker. A failed peer request preserves fresh gateway health. Activity records, recent completions, and connection counts still require authorized peer observation.

The pool contains only machines belonging to the authenticated user's fleet. Other users' machines, names, availability, resources, and activity are excluded at directory and transport authorization, including when a caller supplies a foreign machine ID. See [users and private fleets](users.md).

The live view shows the gateway first, followed by fleet sections:

- **Gateway** shows the connected URL, running version, and uptime on one line, followed by CPU, RAM, and disk on one line. RAM and disk bars show used capacity and percentages; wider terminals also show used/total bytes. Narrow terminals shorten the bars before hiding them. Fill updates directly from cached samples without animation. Light gray changes to yellow at 80% used and red at 95%; unknown capacity shows `?`. Details mode expands fleet tables and machine hardware, without adding gateway rows.
- **Machines** lists every registered machine, its availability, `Seen` age, active count, running version, OS, CPU utilization, RAM used/total, work-directory disk free space, and peer connection modes when observed directly. `Seen` shows `now` for online machines, a compact elapsed duration such as `30s`, `5m`, `2h`, or `3d` for offline machines, and `-` when unknown. Architecture and exact last-seen timestamps remain in JSON. Available RAM remains in JSON, without a duplicate table column. Compact views may shorten versions; details mode shows them in full.
- **Activity** shows queued/running tasks, synchronous providers, artifact operations and transfers, and open TCP tunnels. Rows include operation, state/phase, elapsed time, owner, peer, and ID. Transfers show bytes and total size; tunnels show transmitted and received bytes.
- **Recent** retains visibility into operations that finish between dashboard polls. It reports success, failure, or cancellation without copying results or logs.
- **Hardware**, visible in details mode, shows OS distribution/version, CPU model and physical/logical core counts, plus disk usage for both the work and data directories.

The compact view sizes tables to the terminal. It hides lower-priority columns such as IDs, owner/peer details, and connections before CPU and RAM usage. Node names, operations, and states remain visible, with long values shortened as needed. Headers use short labels: `Work` is the active record count, `Op` is the operation, and `D/R` is the direct/relay session count. Empty activity and hardware sections are omitted. Sample times remain in JSON output but are omitted from the display.

Use `d` to toggle full details, including every column, full identifiers, and hardware information. Use `q`, Escape, or Ctrl+C to quit; `r` to request another snapshot; up/down or `j`/`k` to scroll vertically; left/right or `h`/`l` to scroll wide details; Page Up/Page Down to move a screen; Home/End or `g`/`G` to move to the beginning/end. Plain text and JSON retain full identifiers.

Headings and machine states use static color, disabled by `NO_COLOR` or `TERM=dumb`. Polling keeps the previous view visible with no spinner, blinking text, or flashing refresh indicator. A failed refresh adds a steady `[STALE]` tag beside `Up` and puts the error in the existing footer. It does not add a row or shift the tables. The tag clears after a successful refresh.

Press `a` to add a machine when logged in with a fleet-management key. Enter its name, select macOS, Windows, or Linux, then press Enter to create and copy the installation command. Run it on the target machine in Administrator PowerShell for Windows or a terminal with Bash for macOS/Linux. The installer detects amd64 or arm64; the invitation uses the default 15-minute lifetime. The gateway needs a published bundle with both architectures for that OS. See [installation](installation.md#add-a-machine).

The wizard uses the local OS clipboard. Linux needs `wl-copy`, `xclip`, or `xsel` in a desktop session. If copying fails, the command remains visible; `c` retries copying the same invitation. Escape closes the result. Invitation creation is never retried automatically after an uncertain response.

Press `m` to manage a registered machine with your account key. Select it with arrows and Enter, then press `e` to enable, `d` to disable, or `u` to unregister. Disable blocks new work while accepted work finishes. Unregister removes the registration immediately and prevents that identity from rejoining, including for connected older agents. Lifecycle-capable agents cancel work and stop when they next contact the gateway; older agents require a local stop or a new installer if still running. Changes are requested once and never retried automatically after a lost response. See [registration management](installation.md#manage-registrations).

```sh
control dashboard --node worker,consumer --interval 5s --recent 10
control dashboard --once
control dashboard --json
```

`--interval` defaults to two seconds between activity snapshots, with a minimum of 250 milliseconds. `--timeout` defaults to 15 seconds. `--recent` selects 0..64 recent records per machine and defaults to five. `--node` accepts comma-separated names or stable IDs.

`--once` prints a plain-text snapshot. `--json` prints one structured snapshot. Redirecting output or using non-terminal input also selects a one-shot view. These modes are suitable for scripts and do not enter the terminal's alternate screen.

## Availability is separate from observability

| Display | Meaning |
| --- | --- |
| `idle` | Registered, online, and responding, with no observed active work |
| `busy` | Registered, online, and responding, with active work |
| `/leased` | Execution capacity has an active or still-busy lease |
| `online` | Connected to the gateway, but no fresh owner health or peer observation is available; `Work` shows `-` |
| `disabled` | Registered but excluded from scheduling and new execution; it can remain connected for monitoring |
| `disabling` / `enabling` | The owner changed policy and the connected agent has not acknowledged it yet |
| `offline` | The gateway reports no current node connection; the last-seen time remains visible |
| `unavailable` | The gateway reports the machine online, but its activity snapshot failed, timed out, was denied, or is unsupported |

The header distinguishes registered, online, and successfully observed machines. The gateway is separate from those fleet counts. An unavailable machine has an unknown activity count, not a zero. If the gateway refresh fails, the live view keeps the previous snapshot and marks it `STALE`. Failure to observe peer activity does not discard fresh gateway status. CPU and memory usage do not determine the machine's online status or whether it can accept more work.

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

The dashboard never initiates hardware collection when it refreshes. It reads the latest cached sample. `control machines` contains the registration-time system snapshot; use `system.info`, `node.describe`, or the dashboard for the current cache. Offline or unavailable machines may show that older registration sample; JSON retains the sampling timestamp.

Collection errors are included in the system snapshot. The CPU field is absent until a usable delta is available. A collection timeout leaves the previous cached sample intact. These are host-visible metrics, not per-task consumption or container resource quotas. Docker Desktop reports the environment visible inside its Linux containers.

## Authorization and data boundaries

Gateway machine-health summaries are available only to the owning account key, or the superuser for its legacy fleet. They expose aggregate work count, lease presence, and cached resources. They contain no task records, operation names, owner IDs, arguments, credentials, or private filesystem paths. Common node keys continue to use peer observation permissions. Owner health grants no execution or task-control access.

`activities.list` is a read-only, node-wide metadata operation. A caller allowed to use it can observe work from all owners on that node. It does not grant permission to read another owner's task results/logs, cancel their tasks, invoke providers, or access files.

The default trusted pool permits observation. For a restricted node, add permissions for your orchestrator:

```json
{
  "allow": {
    "ORCHESTRATOR_STABLE_ID": ["activities.list", "system.info", "node.describe"]
  }
}
```

`activities.pool` is available only through a node's local API. It uses that node's identity to request `activities.list` from peers, enforcing each peer's access rules. A remote caller cannot ask another node to aggregate with that node's authority.

Activity records contain operational metadata and progress. They do not include command arguments, prompts, environment values, credentials, results, or log contents. Observational requests are excluded from activity tracking so the dashboard does not list its own polling traffic.

Sender and receiver transfers/tunnels can both appear because they are separate activities on separate nodes. Tasks can also have child transfer rows associated through `taskId`. The in-flight total counts these activities, not just distinct jobs.

## Retention and limits

Tasks retain their existing persisted lifecycle. Active non-task records and the last 64 non-task completions per node are held in memory and disappear on restart. Recent task completions are selected from persisted task records. This is an operational view, not a durable audit log.

Each node tracks up to 2,048 non-task activities in addition to its accepted tasks. Additional active records are counted as omitted. Aggregated snapshots include at most 4,096 active records and 512 recent records; the dashboard reports omitted active detail instead of silently claiming complete coverage. Filter by node to inspect a busy subset.

For AI clients, `control_activities` exposes the peer activity snapshot through the local node and `control_system` reads or refreshes a selected node's resource sample. Direct gateway status is available through authenticated `GET /v1/status`; it does not require MCP or a running local node.
