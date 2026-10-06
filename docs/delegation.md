# Orchestrator authority and worker delegation

[README](../README.md) · [Architecture](architecture.md) · [Protocol](protocol.md) · [Installation](installation.md)

Fleet membership permits discovery, not execution. An account-authenticated CLI or MCP client initiates work. An enrolled node, its local API token, an installation key, or an unbound common key cannot independently execute commands on another worker. Static `allow` rules do not override this boundary.

The gateway assigns `executionAuthority` to live client sessions authenticated with an account key, or the superuser for its legacy fleet. Machines cannot claim it through enrollment metadata or labels. The destination checks this authority before dispatch, on both WebRTC and relay, including existing sessions and streaming methods. Its `allow` rules still restrict account clients. `@orchestrator` in `allow` selects account-authenticated clients; full stable owner IDs allow narrower restrictions.

## Run work through another worker

From a logged-in orchestrator:

```sh
control call first peers.call '{"target":"second","method":"exec.run","params":{"command":"hostname"}}'
```

CLI and MCP first ask `second` for an instruction-bound grant naming `first` as its subject. They then instruct `first`, carrying the grant in that invocation's peer envelope. `first` connects directly to `second`, which checks the authenticated worker identity, grant lifetime, method, and arguments. An unrelated `control exec second` from `first` has no grant and fails.

For durable work, submit `peers.call` as a task, or use a `workflow.run` task. A forwarded `tasks.start` returns its child task handle rather than waiting. Workflow preparation assigns stable child IDs and grants each step's task lifecycle. Child tasks belong to the original orchestrator owner, not the forwarding worker. Repeating a parent submission with the same explicit ID retains its child IDs and idempotent acceptance.

Workflow grants permit only the planned task specification. Dynamic inputs must carry a source-signed artifact grant naming the planned upstream task, output index, and recipient. Output grants can name only artifacts produced by that step and recipients selected in the workflow. Artifact bytes still flow between their producers and consumers.

Grants travel in invocation contexts, including the contexts of durable tasks. They are not stored as worker-wide permissions or inserted into provider environments. An arbitrary script or AI CLI invoking `control` on a worker does not inherit permission to invent peer commands. Use `peers.call` or a planned workflow for that work. Nested delegated workflows and delegated grant-management operations are rejected. `peers.call` accepts JSON instructions, not raw streaming methods.

## Inspect and revoke access

```sh
control call second access.list
control call second access.revoke '{"id":"GRANT_ID"}'
```

Only the original account client owner can list or revoke its grants. Concurrent CLI/MCP processes sharing the saved owner identity can manage them. Another client owner cannot revoke them merely because it has the same fleet account key. `access.revoke` is idempotent for an absent grant.

Revocation rejects further calls, cancels queued and running tasks admitted under the grant, and closes associated transfers, TCP sockets, reverse listeners, and log streams. Artifact grants derived from a delegated task share that task grant's cancellation lifetime. The cancellation is cooperative and uses the existing provider process-tree cleanup. It cannot undo completed commands, written files, or external effects.

There is no fleet-wide grant transaction or single workflow-wide revoke operation. Inspect and revoke the grants at the relevant destinations. Cancelling a workflow also attempts to cancel its current child task without replaying any work.

## One-hour idle expiry

Every destination owns its grant records. A grant expires after one hour without an authorized invocation or stream traffic. Discovery and transport keepalives do not renew it. An accepted task keeps its grant active while queued or running; task completion starts a fresh idle period. Idle streams do not keep a grant alive just by remaining open. Expiry is checked on admission and once per second for existing streams.

Synchronous instructions are single-use, so a worker cannot replay a command during that hour. Task grants permit idempotent submission and owner-scoped status, logs, and cancellation for one explicit task ID. Artifact downloads permit offset-based resumption. Artifact tokens retain their separately requested absolute TTL in addition to the idle access check.

At most 4,096 grants are retained per destination. Records are in memory and disappear on node restart. Unfinished tasks retain the existing interrupted-on-restart behavior; restart never restores a grant or replays a command. Accepted tasks can outlive the submitting client disconnecting.

## Upgrade and credential boundaries

Upgrade the gateway and every execution node. `GET /v1/auth` advertises `delegation`; compatible nodes advertise `instructionDelegation`. Default CLI/MCP account routing refuses older gateways and targets before execution. Older receivers retain their old trust rules until upgraded, so a mixed fleet does not provide this protection everywhere.

Default remote CLI/MCP routing now uses the account client's stable owner even when a local node is running. It no longer inherits that node's authority. Keep using named targets. `--api` and `CONTROL_API` explicitly select the local API, which can execute work on its own node and discover peers but cannot independently execute peer work. Previous tasks owned by a node remain owned by it; inspect them through that worker's local API. They are not reassigned to the new client owner.

Existing restrictive host profiles need an explicit rule for the requesting client owner, or `"@orchestrator": ["*"]`, to accept account-client execution on that host. Newly generated host profiles include the account-client rule. Do not put account or superuser keys in worker configuration, environments, or files accessible to worker commands. Installation keys remain machine-bound.

This protects the Control execution API. It does not sandbox providers, block SSH or other external tools, protect an orchestrator's stolen account credentials, or make a fully compromised OS trustworthy. Someone who controls a worker's private key can attempt the exact instructions still authorized for that worker, but cannot turn those grants into unrelated commands at an uncompromised destination.
