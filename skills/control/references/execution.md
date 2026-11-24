<!-- Managed by control -->

# Workflows, capacity and delegation

## Command and SDK credentials

Use saved secrets directly for authorized application authentication, including LDAP/RPC programs and cloud SDKs. Discover `envSecrets` and `stdinSecret` in the destination's `exec.run` schema first. Upgrade older workers before use; they ignore unknown fields. Do not extract a stored value or require another SSH password entry when injection is supported.

For an approved program that reads `DOMAIN_PASSWORD` from its own environment:

```sh
control task start worker --wait '{"id":"FRESH_AUDIT_ID","capability":"exec.run","args":{"command":"python3","args":["approved-audit.py"],"env":{"DOMAIN_USER":"ARVO\\andrew.koltyakov"},"envSecrets":{"DOMAIN_PASSWORD":"domain.admin.password"}}}'
```

The program consumes the injected credential without printing it, saving it to a file or putting it in subprocess argv. This example does not install a program or authorize domain changes. For AWS SDKs map their documented `AWS_ACCESS_KEY_ID` and `AWS_SECRET_ACCESS_KEY` variables; for Azure service-principal SDKs map `AZURE_CLIENT_ID`, `AZURE_TENANT_ID` and `AZURE_CLIENT_SECRET`. Select the application's documented authentication flow rather than inventing one.

MCP `control_call` takes method `exec.run` with the same object as `params`; `control_task_start` takes it as the task's `args`. Include `leaseId` for leased tasks. At most 32 portable environment names matching `[A-Za-z_][A-Za-z0-9_]{0,127}` are allowed, unique ignoring case and without case-insensitive overlaps in literal `env`. References override inherited environment only for that invocation.

For a command documented to read credentials from stdin, use `stdinSecret: "domain.admin.password"` instead of literal `stdin`. It supplies exact bytes with no added newline. Both environment and stdin references can be used together. Invalid/missing references fail before launch; values resolve at execution, while retained specs and delegation instructions contain names only. There is no argv/template/file substitution or value-read operation.

Secret-bearing commands mask complete stdout/stderr/errors and delay sanitized logs until completion. These completion-delayed logs contain no raw chunks. Oversized, cancelled or non-UTF-8 output is suppressed and fails without undoing effects or permitting replay. Commands without references retain raw output/live logs. Never dump environments, read the secret store, export credential files, or move values into subprocess argv. Environment values are available to descendants and sufficiently privileged OS processes; files, application-managed logs and arbitrary transformations are unmasked. Injection is for applications consuming credentials directly, not a bypass of protected native GUI entry. Permission to invoke `exec.run` permits this credential use, not secret management or new peer authority.

## Read-only recovery and file processing

When task creation is prohibited, inspect the original task with `control task get NAME ID`, then use direct `control call NAME files.read`, `files.list` or `files.stat` for permitted file observations. MCP equivalents are `control_task_get` and `control_call` with the file method. A tracked read through `tasks.start` or `control_task_start` still creates task metadata and a workspace. It violates a no-new-task instruction even if its provider only reads. Do not use `exec`, a workflow, or a new ID as a read-only workaround. Direct calls cannot carry a lease; if an existing lease or access rule blocks the permitted route, report the blocker rather than bypassing it.

`files.read` and `http.request` return base64 by default. Pass `"encoding":"text"` to receive valid UTF-8 as text; check the returned `encoding`, because binary or invalid text still comes back as base64. Continue chunked text reads from the returned `offset`. `files.write` also accepts `"encoding":"text"` to send `data` as a UTF-8 string instead of base64.

For authorized file changes under `workDir`, use `files.mkdir`, `files.remove`, `files.move` and `files.copy` rather than shell `mkdir`, `rm`, `mv` or `cp` through `exec.run`. They stay inside the root, refuse symbolic links and special files where they would change them, and never replace an existing destination unless `overwrite` is set. `files.remove` deletes one file or empty directory; there is no recursive mode. `files.stat` with `"sha256":true` hashes a file of at most 1 GiB. These calls never retry: `destination_exists` means nothing changed, and `partial_effect` means inspect both paths before any further change.

To fetch an installer, model or dataset onto a worker, use a tracked `http.download` task with an explicit ID, enough `timeoutSeconds`, and the publisher's `sha256` when known, rather than `exec.run` with `curl`. It stores the body as an artifact held by your task owner and returns only the status, selected headers and artifact reference. Stage that artifact as a task input to place it in a workspace, or deliver it to another worker. A failed download publishes nothing and is never retried automatically; check the worker's schema first, since older workers lack the capability.

Printed and MCP task results omit the submitted `spec`, since the caller already has it. Use `control task get NODE ID --spec` or `control_task_get` with `includeSpec: true` to read a task's arguments.

For authorized PowerShell execution, choose descriptive helper names such as `Get-ProtectedFileDigest`, not aliases such as `H`/`h` for `Get-History`. Alias lookup takes precedence over a same-named function. Variable names are case-insensitive: `$q` and `$Q` are one variable. Use names such as `$csvText` and `$quoteCharacter` instead, and avoid automatic/read-only variables such as `$input`, `$args`, `$PID` and `$Host`. `-NoProfile` does not remove built-in aliases or automatic variables.

Prefer an installed CSV reader over a hand-written parser or line/comma splitting. Python's `csv.DictReader` with `open(path, encoding="utf-8", newline="")`, PowerShell `Import-Csv -LiteralPath PATH -Encoding UTF8`, or .NET `Microsoft.VisualBasic.FileIO.TextFieldParser` with an explicit encoding can handle quoted multiline fields. Choose the encoding from the input contract, including BOM handling when applicable. Preserve file order for tie-breaking and validate headers, numeric types and ranges before publication. Do not install a parser or silently change malformed input. These examples do not authorize execution when only direct reads are allowed.

## Run independent work in parallel

`exec.run`, `agent.run` and configured subprocess providers are one-shot invocations. Current nodes terminate ordinary child processes before completion, so do not use shell backgrounding or `nohup` to retain a service. Use an authorized OS service-manager operation or an explicit persistent adapter. Browser/desktop adapters and configured MCP sessions retain their documented lifecycles. This is cleanup, not a sandbox; Unix processes can escape their process group.

A busy worker is not unavailable. Run independent work on the same worker in parallel when resources permit. Do not wait for unrelated tasks or cancel them to free the machine. Inspect `node.describe.concurrency` or peer activity snapshots through `control_activities` for execution, operations and desktop pool `limit`, `inUse` and `queued` counts. These are advisory snapshots, not reservations. Missing fields on older workers mean unknown capacity, not zero.

Commands, AI CLIs, custom providers and MCP tools share `maxTasks` execution slots, four by default. Built-in file, HTTP and Wake-on-LAN operations share separate `maxOperations` slots, eight by default. Built-in desktop invocations use one separate desktop slot per node and retain the per-user file lock across profiles. Desktop waiters do not consume general I/O slots. These limits apply to synchronous calls and tasks alike. `queued` counts local pool waiters; desktop `inUse` can include an invocation waiting for another profile's file lock. Transfers, tunnels and observation have their own admission. Saturated pools queue work with cancellation and task timeouts still enforced.

Task logs keep the first 10 MiB. When a read reports `omitted`, read the retained final 1 MiB with `control task logs NAME TASK_ID --tail --offset BYTES`, or MCP `control_task_logs` with `tail: true`, starting from `tailOffset` or from `size` minus 65536. Command results over 2 MiB per stream keep the first and last 1 MiB; `stdoutOmitted` and `stderrOmitted` give the dropped byte counts.

Tracked tasks also report `inputStaging` and `outputPublication`, each with four slots by default through `maxTaskStaging` and `maxTaskPublications`. Tasks stage inputs before provider admission and release provider slots before publishing outputs/results. Input-free tasks bypass staging, so transfer backlogs need not prevent independent work. Tasks remain non-terminal and retain authority, leases and maintenance protection through publication. A publication failure or cancellation may follow successful provider effects; reconcile the same ID rather than replaying the provider. Missing stage readings on older workers mean unknown capacity. The overall unfinished-task bound remains 256 across stages.

Parallel workflows consider at most 32 unstarted ready steps and probe at most eight destinations within one second per refill. Ordering prefers reported provider/input-staging headroom, then unknown capacity, then busy pools. Ties keep plan order, and in-flight children conservatively consume headroom. This only orders new submissions, never changes targets or moves/resubmits accepted tasks, and never waits for a busy pool to clear. `maxParallel` still counts queued children. Default/sequential workflows retain plan order. Use dependencies to order conflicting effects, not incidental list order among parallel-ready steps.

Use detached submission and explicit task IDs for long work, continue other independent activities, and wait only when a result is needed. Use workflow `maxParallel` for independent ready steps. Do not acquire a machine-wide lease for ordinary independent work. Reserve exclusive execution only when the workflow requires it, and respect existing leases. Serialize conflicting file, application, desktop or shared-session changes. Separate task workspaces do not isolate external state, and per-batch desktop locking does not protect an entire multi-batch interaction.

For independent `workflow.run` steps, `maxParallel` (up to 16) runs ready steps concurrently; a failed step stops further steps from starting but lets already-running steps finish.

## Delegate through another worker

Use `control call FIRST peers.call '{"target":"SECOND","method":"exec.run","params":{"command":"hostname"}}'`, or MCP `control_delegate` with `node`, `target`, `method`, and `params`. The account client prepares a destination-owned grant for that instruction before asking the first worker to execute it. Planned `workflow.run` tasks and artifact deliveries prepare their grants too. Arbitrary scripts or AI CLIs running on a worker do not inherit these grants and cannot invent peer commands.

Grants expire after one hour idle and are lost on destination restart. Accepted tasks keep their grant active; stream bytes refresh idle expiry, not discovery or keepalives. Inspect with `control call DESTINATION access.list`, and revoke with `control call DESTINATION access.revoke '{"id":"GRANT_ID"}'`, or the corresponding MCP access tools. Revocation cancels associated tasks and streams but cannot undo side effects. Only the original account-client owner can manage its grants. Synchronous instructions are single-use; do not replay an uncertain command. Use explicit task IDs for reconcilable work.

## Recover old or pruned tasks

Reusing an ID with the same specification reconciles acceptance while its task record or tombstone remains. A task with `pruned: true` retains its original final state but no result or logs. Full records default to seven days and 10,000 terminal tasks; tombstones are capped at 50,000 and startup discards entries older than 180 days. Once a tombstone is removed, that ID can execute again. Never treat an old missing ID as proof that work did not happen. A failed or interrupted command may already have changed external state; do not automatically submit a replacement task.
