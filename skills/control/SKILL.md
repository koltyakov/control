---
name: control
description: Use Control to inspect registered machines, run tools on named Windows, Linux, or macOS peers, monitor remote tasks, and transfer artifacts directly between machines. Use when the user asks to do work on another machine in their Control pool or check its availability and resources.
---

<!-- Managed by control -->

# Work with the Control pool

## Terminology

An orchestrator coordinates work, a worker executes requested work, and the gateway handles enrollment, discovery, signaling, encrypted relay fallback, and fleet administration. Client names the CLI/MCP component; node names the enrolled execution service. Orchestrator and worker are operation roles, not fixed machine types: a node can perform both, and an orchestrator can use a standalone client without a local node. Reserve agent for AI software, including optional `agent.run` providers, not Control services.

Use the installed `control` CLI or the corresponding Control MCP tools. The saved login or local configuration supplies the gateway connection and credentials. Remote CLI/MCP calls work without a local service using an authenticated client session, unless `--api` or `CONTROL_API` explicitly selects an API. A client is not a fleet machine and must never enroll itself or appear in machine discovery. Standalone mode requires client-session and concurrent-owner support on the gateway and target nodes; report an upgrade error rather than falling back to enrollment. Use named targets in standalone mode. Concurrent standalone processes share the persistent task owner but use independent transport connections. Do not print configuration secrets or installation links in logs.

Commands operate only within the authenticated user's private fleet. Machine names can repeat across users. Never switch credentials to reach another user's machine or interpret an unknown foreign ID as permission to enroll it.

## Discover before executing

1. Run `control machines` to resolve the requested machine name and check availability.
2. Run `control call NAME node.describe` to inspect supported operations and schemas.
3. Read `control system NAME` for cached resources. Use `--refresh` only when a new sample is needed.

An offline or unavailable node is not an idle node. Do not schedule work on machines with `disabled` or `controlPending` set. `control_select` excludes those machines. Report connection failures rather than choosing a different execution machine without the user's instruction.

## Submit durable work

For a command, use `control exec NAME -- COMMAND ARG...`. The CLI prints a task ID before submission and waits for its result. For long work, use `--detach --id TASK_ID --timeout 4h` before `--` to return after durable acceptance. Follow logs with `control task logs NAME TASK_ID --follow --offset 0`. Stopping a follower or wait does not cancel the task; use `control task cancel`. Use structured argument arrays; do not assume the remote machine has a Unix shell.

For AI agents, custom providers, input artifacts, or declared outputs, create a task specification and submit it:

```sh
control task start worker @task.json
control task wait worker TASK_ID
control task logs worker TASK_ID
```

Choose the task ID before submitting. If the connection fails during submission, query that same ID before retrying. Reusing an ID with the same specification reconciles acceptance. A failed or interrupted command may already have changed external state; do not automatically submit a replacement task.

Use `control dashboard --once` for pool-wide activity. Observation does not grant permission to read or cancel another owner's work.

## Move results directly

Export files from their source with `control artifact export SOURCE PATH`. Pass the returned artifact reference as a task input to let the destination fetch it directly. Use `control artifact deliver SOURCE ARTIFACT_ID DESTINATION` for peer-to-peer delivery. Download through the host only when the user needs a local copy:

```sh
control artifact get worker ARTIFACT_ID ./result.bin
```

## Forward ports on the orchestrator

Use `control tunnel NAME HOST:PORT --listen 127.0.0.1:PORT` for a foreground TCP forward. MCP `control_forward_start` accepts `node`, `address`, and optional `listen` and returns an ID and local address immediately. It survives tool-call completion until `control_forward_stop` or MCP process exit. Use `control_forward_list` for active connections and errors, and `control_session` or CLI `control session` for process identity, traffic lanes, and stream counts. Upgraded peers share a WebRTC carrier across independent control, bulk, and interactive channels, stream followed task logs, and preserve TCP write-side EOF. Older peers retain separate connections, log polling, or full-close semantics. Forwards require `tcp.open` permission on the worker, do not create fleet registrations, and do not stop remote services when closed. Default to loopback; use a non-loopback listener only when the user asks to expose it. Failed sockets and log streams are not automatically replayed. Resume log following explicitly by byte offset.

## Manipulate a worker's GUI

Discover `rpa.run` before using MCP `control_rpa` or `control call NAME rpa.run`. It requires an explicitly configured helper in the logged-in user's desktop session. Inspect accessibility elements first, then use exact selectors for native focus, value setting, or activation. Selectors must match exactly one element; do not guess platform-native role names or silently replace a failed selector with a coordinate click. Use explicitly chosen pointer/keyboard actions only after inspecting the current screen. Screenshots return PNG artifact references; download or deliver them through the existing artifact operations. Account for reported screenshot scaling before using image coordinates.

Batches are serialized per OS user. For a sequence of batches on one node, acquire a node lease and submit tracked `rpa.run` tasks with its `leaseId`. Local users and unrelated automation can still change the desktop. Failed actions may already have changed application state. Never automatically replay a batch after an error or uncertain response; inspect the current GUI and reconcile tracked submissions by task ID. Linux coordinate actions require X11; report missing dependencies or OS permissions rather than bypassing them.

## Setup and administration

Use `control machines add auto --platform OS`, or omit the name, to use the target machine's hostname. This name is resolved by the target installer, not by the orchestrator. Hostname collisions fail without replacing another machine; choose an explicit name rather than automatically retrying or adding a suffix. Automatic naming requires compatible gateway and installer binaries.

Use `control service status` to check the local node. `control service start` starts its saved configuration. A fleet owner can create an installation command with `control machines add NAME --platform macos|windows|linux`. The installer detects architecture and uses the default invitation lifetime. Treat the URL as a short-lived bearer secret and share it only with the intended installer. Account keys can use `control machines disable|enable|unregister NAME`; unregister removes even offline registrations and retires their identities. Lifecycle-capable nodes stop on their next gateway contact; older offline nodes require a local stop if still running. Use these lifecycle commands only when the user requests the change. Common keys cannot manage invitations or machine policy. User provisioning and global software updates require the gateway superuser.

Prefer MCP tools when already available. The MCP server is self-contained and does not need this skill to expose its tool schemas.
