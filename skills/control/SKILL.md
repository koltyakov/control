---
name: control
description: Use Control to inspect registered machines, run tools on named Windows, Linux, or macOS peers, monitor remote tasks, and transfer artifacts directly between machines. Use when the user asks to do work on another machine in their Control pool or check its availability and resources.
---

<!-- Managed by control -->

# Work with the Control pool

## Terminology

An orchestrator coordinates work, a worker executes requested work, and the gateway handles enrollment, discovery, signaling, encrypted relay fallback, and fleet administration. Client names the CLI/MCP component; node names the enrolled execution service. Orchestrator and worker are operation roles, not fixed machine types: a node can perform both, and an orchestrator can use a standalone client without a local node. Reserve agent for AI software, including optional `agent.run` providers, not Control services.

Use the installed `control` CLI or the corresponding Control MCP tools. Default remote calls use the saved account login and a client session even when a local node is running. Worker/common credentials permit discovery, not independent peer execution. `--api` or `CONTROL_API` explicitly selects the local API and does not confer orchestrator authority. A client is not a fleet machine and must never enroll itself. Require delegation support on the gateway and execution nodes; report an upgrade error rather than using older trust rules. Use named execution targets. Concurrent account clients share the persistent task owner but use independent connections. Do not print configuration secrets or installation links in logs.

Commands operate only within the authenticated user's private fleet. Machine names can repeat across users. Never switch credentials to reach another user's machine or interpret an unknown foreign ID as permission to enroll it.

## Discover before executing

1. Run `control machines` to resolve the requested machine name and check availability.
2. Run `control call NAME node.describe` to inspect supported operations and schemas.
3. Read `control system NAME` for cached resources. Use `--refresh` only when a new sample is needed.

An offline or unavailable node is not an idle node. Do not schedule work on machines with `disabled` or `controlPending` set. `control_select` excludes those machines. Report connection failures rather than choosing a different execution machine without the user's instruction.

## Wake a known LAN machine

When the user asks to wake a machine, use `control wake NAME` or MCP `control_wake` with `node`. The target must be offline and have retained `wakeInterfaces` and verified `lanPeers`. Updated nodes automatically report interfaces and learn directional LAN relationships through signed broadcast proofs while online together. No LAN label or manual MAC entry is needed. Matching private ranges alone are not evidence. Control prefers this host when a registered NIC matches a verified sender, otherwise selects an online worker. Use `--from WORKER` or MCP `from` to choose a sender, or `--from local` to require this host. The former `--lan` argument is no longer used. An older machine already offline must come online once before discovery can learn its metadata and relationships.

Wake requires account-client routing and `network.wake` permission on a worker sender. It sends UDP magic packets only, does not configure firmware or NIC wake settings, and does not confirm power-on. Check the directory later for online status. Do not automatically wake machines as a side effect of execution failure, retry uncertain wake requests, or switch senders after failure.

## Test peer connections on demand

Use `control speedtest NAME` to measure this orchestrator's connection to a worker, or `control speedtest DESTINATION --from SOURCE` for direct worker-to-worker traffic. MCP exposes `control_speedtest` with `node`, optional `from`, `bytes`, and `samples`. Results include warmed RTT, jitter, sequential throughput in both directions, and the measured WebRTC/relay mode. Upload means source to destination; download is the reverse. Worker payloads do not pass through the orchestrator.

Tests require account-client routing, upgraded workers, `connection.open` permission at the destination, and `connection.test` at a selected source. Discover `node.describe.connectionTest` first. Explicit `--api` or `CONTROL_API` is unsupported. Defaults are 16 MiB per direction and ten samples; CLI `--size` accepts 1..256 MiB, `--samples` 1..100, and `--timeout` 1s..2m. Tests consume bandwidth, run only on demand, leave no files, and are not durable tasks. Never rerun a failed test automatically or substitute another machine.

Use the reported `route` to distinguish `direct`, `turn`, `gateway-relay`, and `unknown`. `transport: "webrtc"` can use TURN and does not alone prove a direct connection. Session and dashboard route counts are observations, not new connection tests.

## Submit durable work

For a command, use `control exec NAME -- COMMAND ARG...`. The CLI prints a task ID before submission and waits for its result. For long work, use `--detach --id TASK_ID --timeout 4h` before `--` to return after durable acceptance. Follow logs with `control task logs NAME TASK_ID --follow --offset 0`. Stopping a follower or wait does not cancel the task; use `control task cancel`. Use structured argument arrays; do not assume the remote machine has a Unix shell.

For AI agents, custom providers, input artifacts, or declared outputs, create a task specification and submit it:

```sh
control task start worker @task.json
control task wait worker TASK_ID
control task logs worker TASK_ID
```

Choose the task ID before submitting. If the connection fails during submission, query that same ID before retrying. Reusing an ID with the same specification reconciles acceptance while its task record or tombstone remains. A task with `pruned: true` retains its original final state but no result or logs. Full records default to seven days and 10,000 terminal tasks; tombstones are capped at 50,000 and startup discards entries older than 180 days. Once a tombstone is removed, that ID can execute again. Never treat an old missing ID as proof that work did not happen. A failed or interrupted command may already have changed external state; do not automatically submit a replacement task. With MCP, wait with `control_task_get` and `waitSeconds` rather than polling repeatedly. For independent `workflow.run` steps, `maxParallel` (up to 16) runs ready steps concurrently; a failed step stops further steps from starting but lets already-running steps finish.

Use `control dashboard --once` for pool-wide activity. Observation does not grant permission to read or cancel another owner's work.

## Delegate through another worker

Use `control call FIRST peers.call '{"target":"SECOND","method":"exec.run","params":{"command":"hostname"}}'`, or MCP `control_delegate` with `node`, `target`, `method`, and `params`. The account client prepares a destination-owned grant for that instruction before asking the first worker to execute it. Planned `workflow.run` tasks and artifact deliveries prepare their grants too. Arbitrary scripts or AI CLIs running on a worker do not inherit these grants and cannot invent peer commands.

Grants expire after one hour idle and are lost on destination restart. Accepted tasks keep their grant active; stream bytes refresh idle expiry, not discovery or keepalives. Inspect with `control call DESTINATION access.list`, and revoke with `control call DESTINATION access.revoke '{"id":"GRANT_ID"}'`, or the corresponding MCP access tools. Revocation cancels associated tasks and streams but cannot undo side effects. Only the original account-client owner can manage its grants. Synchronous instructions are single-use; do not replay an uncertain command. Use explicit task IDs for reconcilable work.

## Move results directly

Export files from their source with `control artifact export SOURCE PATH`, then request recipient-bound access with `control call SOURCE artifacts.grant '{"id":"ARTIFACT_ID","target":"DESTINATION"}'`. Pass the granted reference, including `grant`, as a task input so the destination can fetch it directly. Planned workflow inputs prepare their signed grants automatically. Use `control artifact deliver SOURCE ARTIFACT_ID DESTINATION` for peer-to-peer delivery. Download through the host only when the user needs a local copy:

```sh
control artifact get worker ARTIFACT_ID ./result.bin
```

## Paste a clipboard

Only transfer a clipboard when the user requests it. Use `control clipboard paste NAME` or MCP `control_clipboard_paste` with `node`. Text replaces the remote OS clipboard; copied regular files stream into the node's workspace when the call is made. `--dir incoming` chooses an existing directory relative to `workDir`. With `--reverse` or MCP `reverse: true`, the remote clipboard is the source and `dir` is an existing local directory. Results report only basenames and byte counts, never clipboard text. Do not inspect clipboard text through generic calls or use clipboard transfer to retrieve secrets.

Both desktops need clipboard access for text; a file destination needs only filesystem access. Discover the `clipboard` protocol field in `node.describe`, and require `clipboard.paste` permission or `clipboard.open` for reverse. There is no background synchronization or native Finder/Explorer paste hook. Directories, symlinks, images, and rich formats are unsupported. Source files are not deleted, existing destinations are not overwritten, and completed files can remain after a later failure. An uncertain paste may already have succeeded; inspect destination metadata rather than retrying automatically. Use artifacts for durable, resumable transfers.

## Forward ports

Use `control tunnel NAME HOST:PORT --listen 127.0.0.1:PORT` for a foreground TCP forward. MCP `control_forward_start` accepts `node`, `address`, and optional `listen` and returns an ID and local address immediately. It survives tool-call completion until `control_forward_stop` or MCP process exit. Use `control_forward_list` for active connections and errors, and `control_session` or CLI `control session` for process identity, traffic lanes, and stream counts. Upgraded peers share a WebRTC carrier across independent control, bulk, and interactive channels, stream followed task logs, and preserve TCP write-side EOF. Older peers retain separate connections, log polling, or full-close semantics. Forwards require `tcp.open` permission on the worker, do not create fleet registrations, and do not stop remote services when closed. Default to loopback; use a non-loopback listener only when the user asks to expose it. Failed sockets and log streams are not automatically replayed. Resume log following explicitly by byte offset.

For a dev server running on this orchestrator, use `control tunnel NAME 127.0.0.1:3000 --reverse --listen 127.0.0.1:3000`, then open `http://localhost:3000` on the remote machine. MCP `control_forward_start` uses `reverse: true`, with `address` as the orchestrator-local destination and `listen` as the remote bind address. HTTP and WebSocket hot reload work over TCP. This requires remote `tcp.listen` permission and updated receiver/API support, not enrollment of a standalone client. The remote listener closes when stopped, disconnected, or the requesting process exits. The dev server stays running.

## Retain tunnels across client exits

Use `control tunnel start NAME HOST:PORT --listen 127.0.0.1:PORT --id TUNNEL_ID`, with `--reverse` for a remote listener targeting this orchestrator. It returns after durable acceptance without blocking the terminal. MCP `control_tunnel_start` requires `id`, `node`, `address`, and fixed `listen`, with optional `reverse` and `ttlSeconds`. The separate host user service retains definitions after CLI/MCP exits and restores listeners after disconnects or service/destination restarts. It is not an enrolled node. An interrupted TCP socket is never replayed; applications must make new connections.

Inspect with `control tunnel list` or `control_tunnel_list`; `starting` is accepted, `active` means bound, and `retrying` reports setup errors. Use `control tunnel dispose ID` or `control_tunnel_dispose` to durably remove a definition and close its sockets. Without TTL, it remains until disposed. CLI `--ttl 2h` or MCP `ttlSeconds` sets an absolute expiry that does not reset on reconnect. Reusing the same ID/specification reconciles without renewing TTL; changed specifications fail. After an uncertain start, list before retrying and never invent a replacement ID automatically.

Automatic host restart recovery requires the user's service manager and may require login, or Linux linger. Service installation fails rather than silently falling back when that manager is unavailable. Explicit `CONTROL_TUNNEL_SERVICE_MODE=process` has no automatic reboot/crash recovery. Persistent tunnels require account routing, not explicit local API routing. They are scoped to this host's gateway/login credential and pin immutable destination IDs. Dashboard `Tunnels` shows live `↑` forward TCP connections and `↓` reverse listeners from authorized node-wide observation. The separate `P.Tun.` column means permanent tunnels and counts this host login's retained definitions, including retrying ones but excluding expired ones. Keep listeners on loopback unless the user requests network exposure. Any HTTP/TLS proxy or dev server remains a separate application with its own lifetime.

## Manipulate a worker's GUI

Discover `rpa.run` before using MCP `control_rpa` or `control call NAME rpa.run`. It requires an explicitly configured helper in the logged-in user's desktop session. Inspect accessibility elements first, then use exact selectors for native focus, value setting, or activation. Selectors must match exactly one element; do not guess platform-native role names or silently replace a failed selector with a coordinate click. Use explicitly chosen pointer/keyboard actions only after inspecting the current screen. Screenshots return PNG artifact references; download or deliver them through the existing artifact operations. Account for reported screenshot scaling before using image coordinates.

Batches are serialized per OS user. For a sequence of batches on one node, acquire a node lease and submit tracked `rpa.run` tasks with its `leaseId`. Local users and unrelated automation can still change the desktop. Failed actions may already have changed application state. Never automatically replay a batch after an error or uncertain response; inspect the current GUI and reconcile tracked submissions by task ID. Linux coordinate actions require X11; report missing dependencies or OS permissions rather than bypassing them.

## Setup and administration

For credentials, use worker-local secret names supplied by the user. GUI `setValue`/`type` accepts `secret` instead of `text`; HTTP `headerSecrets` maps headers to `{secret, prefix}`. Ask the user to enter missing credentials themselves with `control secrets set NAME` on the worker, outside chat. Never request, retrieve, print, or paste credential values into prompts or tool inputs. There is no secret-value read operation. Secret-bearing HTTP requests do not follow redirects. Text results mask configured values, but screenshots and unrestricted execution can expose them. Do not reveal passwords or capture credential-bearing screens.

Use `control machines add auto --platform OS`, or omit the name, to use the target machine's hostname. This name is resolved by the target installer, not by the orchestrator. Hostname collisions fail without replacing another machine; choose an explicit name rather than automatically retrying or adding a suffix. Automatic naming requires compatible gateway and installer binaries.

Choose `machines add --service user|system` explicitly when startup context matters. User context uses the installing user's files and credentials but may stop at logout; system context starts at boot as root on Unix or LocalService on Windows and has no interactive desktop. Linux user startup can use administrator-configured linger without changing identity. Direct Windows `setup` defaults to a system service, unlike invitation scripts' user-login default.

Rename only when requested, with `control machines rename OLD NEW`. The shared client also migrates this project's `.control-scenarios/OLD/` folder and supported Markdown/JSON routing references, not arbitrary files or other hosts. A pending invitation can reserve the destination name. Use `--force` on rename or invitation creation only after explicit approval to retract that invitation's URL. Force never takes over another enrolled identity. After an uncertain response, inspect the directory and invitation registry instead of replaying the mutation.

Use `control service status` to check the local node. `control service start` starts its saved configuration. A fleet owner can create an installation command with `control machines add NAME --platform macos|windows|linux`. The installer detects architecture and uses the default invitation lifetime. Treat the URL as a short-lived bearer secret and share it only with the intended installer. Account keys can use `control machines disable|enable|unregister NAME`; unregister removes even offline registrations and retires their identities. Current installations cancel work and uninstall startup and their unshared executable on their next gateway contact, retaining configuration and work files. Older installations may only stop or require local cleanup; see the repository's installation documentation before promising remote uninstall. Use these lifecycle commands only when the user requests the change. Common keys cannot manage invitations or machine policy. User provisioning and global software updates require the gateway superuser.

Prefer MCP tools when already available. The MCP server is self-contained and does not need this skill to expose its tool schemas.

Windows worker installers request UAC only for executable-scoped firewall setup; user-login nodes remain non-elevated. For an existing profile, run the installed CLI's `service firewall` locally when the user requests that change. Use its absolute path on Windows because `control` can resolve to Windows Control Panel. Rules cover WebRTC UDP and outbound gateway TCP, not inbound API access. Updated Windows supervisors use a stable verified runtime path so managed updates retain firewall coverage; older launchers need an installed-CLI update and restart.
