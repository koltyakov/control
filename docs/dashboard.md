# Dashboard and system metrics

[Contributor guide](../AGENTS.md) · [Architecture](architecture.md) · [Protocol reference](protocol.md) · [Testing](testing.md)

## Open the dashboard

Start your local node, then run:

```sh
control dashboard
```

The command uses the same `CONTROL_API` and `CONTROL_TOKEN` as the CLI and MCP server. The orchestrator's node collects snapshots from the enrolled machines. It observes work submitted by any peer, including tasks delegated by another worker.

The pool contains only machines belonging to the authenticated user's fleet. Other users' machines, names, availability, resources, and activity are excluded at directory and transport authorization, including when a caller supplies a foreign machine ID. See [users and private fleets](users.md).

The live view has four sections:

- **Machines** lists every registered machine, its availability, active count, OS/architecture, CPU utilization, RAM usage and availability, work-directory disk free space, sample age, and peer connection modes.
- **In-flight activities** shows queued/running tasks, synchronous providers, artifact operations and transfers, and open TCP tunnels. Rows include operation, state/phase, elapsed time, owner, peer, and ID. Transfers show bytes and total size; tunnels show transmitted and received bytes.
- **Recent completions** retains visibility into operations that finish between dashboard polls. It reports success, failure, or cancellation without copying results or logs.
- **System capabilities** shows OS distribution/version, CPU model and physical/logical core counts, plus disk usage for both the work and data directories.

Use `q`, Escape, or Ctrl+C to quit; `r` to request another activity snapshot; up/down or `j`/`k` to scroll vertically; left/right or `h`/`l` to scroll horizontally; Page Up/Page Down to move a screen; Home/End or `g`/`G` to move to the beginning/end. Scrolling exposes rows and columns outside the terminal viewport. Plain text and JSON also retain full identifiers.

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
| `offline` | The gateway reports no current node connection; the last-seen time remains visible |
| `unavailable` | The gateway reports the machine online, but its activity snapshot failed, timed out, was denied, or is unsupported |

The header distinguishes registered, online, and successfully observed machines. An unavailable machine has an unknown activity count, not a zero. If the whole refresh fails, the live view keeps the previous snapshot and marks it `STALE`. CPU and memory usage do not determine the machine's online status or whether it can accept more work.

Polling is bounded and concurrent, with up to eight nodes at a time. Each node request has a six-second deadline within a ten-second aggregation deadline. A failing node does not discard snapshots from healthy peers. The dashboard reads periodic snapshots rather than consuming a continuous event stream.

## Resource sampling

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

The dashboard never initiates hardware collection when it refreshes. It reads the latest cached sample and displays its age. `control machines` contains the registration-time system snapshot; use `system.info`, `node.describe`, or the dashboard for the current cache. Offline or unavailable machines may show that older registration sample, explicitly marked stale.

Collection errors are included in the system snapshot. The CPU field is absent until a usable delta is available. A collection timeout leaves the previous cached sample intact. These are host-visible metrics, not per-task consumption or container resource quotas. Docker Desktop reports the environment visible inside its Linux containers.

## Authorization and data boundaries

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

For AI clients, `control_activities` exposes the same pool snapshot and `control_system` reads or refreshes a selected node's resource sample.
