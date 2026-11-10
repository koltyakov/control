<!-- Managed by control -->

# Workflows, capacity and delegation

## Read-only recovery and file processing

When task creation is prohibited, inspect the original task with `control task get NAME ID`, then use direct `control call NAME files.read` or `files.list` for permitted file observations. MCP equivalents are `control_task_get` and `control_call` with the file method. A tracked read through `tasks.start` or `control_task_start` still creates task metadata and a workspace. It violates a no-new-task instruction even if its provider only reads. Do not use `exec`, a workflow, or a new ID as a read-only workaround. Direct calls cannot carry a lease; if an existing lease or access rule blocks the permitted route, report the blocker rather than bypassing it.

For authorized PowerShell execution, choose descriptive helper names such as `Get-ProtectedFileDigest`, not aliases such as `H`/`h` for `Get-History`. Alias lookup takes precedence over a same-named function. Variable names are case-insensitive: `$q` and `$Q` are one variable. Use names such as `$csvText` and `$quoteCharacter` instead, and avoid automatic/read-only variables such as `$input`, `$args`, `$PID` and `$Host`. `-NoProfile` does not remove built-in aliases or automatic variables.

Prefer an installed CSV reader over a hand-written parser or line/comma splitting. Python's `csv.DictReader` with `open(path, encoding="utf-8", newline="")`, PowerShell `Import-Csv -LiteralPath PATH -Encoding UTF8`, or .NET `Microsoft.VisualBasic.FileIO.TextFieldParser` with an explicit encoding can handle quoted multiline fields. Choose the encoding from the input contract, including BOM handling when applicable. Preserve file order for tie-breaking and validate headers, numeric types and ranges before publication. Do not install a parser or silently change malformed input. These examples do not authorize execution when only direct reads are allowed.

## Run independent work in parallel

A busy worker is not unavailable. Run independent work on the same worker in parallel when resources permit. Do not wait for unrelated tasks or cancel them to free the machine. Inspect `node.describe.concurrency` or peer activity snapshots through `control_activities` for execution, operations and desktop pool `limit`, `inUse` and `queued` counts. These are advisory snapshots, not reservations. Missing fields on older workers mean unknown capacity, not zero.

Commands, AI CLIs, custom providers and MCP tools share `maxTasks` execution slots, four by default. Built-in file, HTTP and Wake-on-LAN operations share separate `maxOperations` slots, eight by default. Built-in desktop invocations use one separate desktop slot per node and retain the per-user file lock across profiles. Desktop waiters do not consume general I/O slots. These limits apply to synchronous calls and tasks alike. `queued` counts local pool waiters; desktop `inUse` can include an invocation waiting for another profile's file lock. Transfers, tunnels and observation have their own admission. Saturated pools queue work with cancellation and task timeouts still enforced.

Tracked tasks also report `inputStaging` and `outputPublication`, each with four slots by default through `maxTaskStaging` and `maxTaskPublications`. Tasks stage inputs before provider admission and release provider slots before publishing outputs/results. Input-free tasks bypass staging, so transfer backlogs need not prevent independent work. Tasks remain non-terminal and retain authority, leases and maintenance protection through publication. A publication failure or cancellation may follow successful provider effects; reconcile the same ID rather than replaying the provider. Missing stage readings on older workers mean unknown capacity. The overall unfinished-task bound remains 256 across stages.

Parallel workflows consider at most 32 unstarted ready steps and probe at most eight destinations within one second per refill. Ordering prefers reported provider/input-staging headroom, then unknown capacity, then busy pools. Ties keep plan order, and in-flight children conservatively consume headroom. This only orders new submissions, never changes targets or moves/resubmits accepted tasks, and never waits for a busy pool to clear. `maxParallel` still counts queued children. Default/sequential workflows retain plan order. Use dependencies to order conflicting effects, not incidental list order among parallel-ready steps.

Use detached submission and explicit task IDs for long work, continue other independent activities, and wait only when a result is needed. Use workflow `maxParallel` for independent ready steps. Do not acquire a machine-wide lease for ordinary independent work. Reserve exclusive execution only when the workflow requires it, and respect existing leases. Serialize conflicting file, application, desktop or shared-session changes. Separate task workspaces do not isolate external state, and per-batch desktop locking does not protect an entire multi-batch interaction.

For independent `workflow.run` steps, `maxParallel` (up to 16) runs ready steps concurrently; a failed step stops further steps from starting but lets already-running steps finish.

## Delegate through another worker

Use `control call FIRST peers.call '{"target":"SECOND","method":"exec.run","params":{"command":"hostname"}}'`, or MCP `control_delegate` with `node`, `target`, `method`, and `params`. The account client prepares a destination-owned grant for that instruction before asking the first worker to execute it. Planned `workflow.run` tasks and artifact deliveries prepare their grants too. Arbitrary scripts or AI CLIs running on a worker do not inherit these grants and cannot invent peer commands.

Grants expire after one hour idle and are lost on destination restart. Accepted tasks keep their grant active; stream bytes refresh idle expiry, not discovery or keepalives. Inspect with `control call DESTINATION access.list`, and revoke with `control call DESTINATION access.revoke '{"id":"GRANT_ID"}'`, or the corresponding MCP access tools. Revocation cancels associated tasks and streams but cannot undo side effects. Only the original account-client owner can manage its grants. Synchronous instructions are single-use; do not replay an uncertain command. Use explicit task IDs for reconcilable work.

## Recover old or pruned tasks

Reusing an ID with the same specification reconciles acceptance while its task record or tombstone remains. A task with `pruned: true` retains its original final state but no result or logs. Full records default to seven days and 10,000 terminal tasks; tombstones are capped at 50,000 and startup discards entries older than 180 days. Once a tombstone is removed, that ID can execute again. Never treat an old missing ID as proof that work did not happen. A failed or interrupted command may already have changed external state; do not automatically submit a replacement task.
