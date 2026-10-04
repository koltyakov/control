---
name: control
description: Use Control to inspect registered machines, run tools on named Windows, Linux, or macOS peers, monitor remote tasks, and transfer artifacts directly between machines. Use when the user asks to do work on another machine in their Control pool or check its availability and resources.
---

<!-- Managed by control -->

# Work with the Control pool

Use the installed `control` CLI or the corresponding Control MCP tools. The saved local configuration supplies the gateway connection and credentials. Do not print configuration secrets or installation links in logs.

Commands operate only within the authenticated user's private fleet. Machine names can repeat across users. Never switch credentials to reach another user's machine or interpret an unknown foreign ID as permission to enroll it.

## Discover before executing

1. Run `control machines` to resolve the requested machine name and check availability.
2. Run `control call NAME node.describe` to inspect supported operations and schemas.
3. Read `control system NAME` for cached resources. Use `--refresh` only when a new sample is needed.

An offline or unavailable node is not an idle node. Report connection failures rather than choosing a different execution machine without the user's instruction.

## Submit durable work

For a command, use `control exec NAME -- COMMAND ARG...`. The CLI prints a task ID before submission and waits for its result. Use structured argument arrays; do not assume the remote machine has a Unix shell.

For agents, custom providers, input artifacts, or declared outputs, create a task specification and submit it:

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

## Setup and administration

Use `control service status` to check the local node. `control service start` starts its saved configuration. A fleet owner can create a machine installation command with `control machines add NAME --platform OS/ARCH --ttl 15m`. Treat the resulting URL as a short-lived bearer secret and share it only with the intended installer. Common keys cannot manage invitations. User provisioning and global software updates require the gateway superuser.

Prefer MCP tools when already available. The MCP server is self-contained and does not need this skill to expose its tool schemas.
