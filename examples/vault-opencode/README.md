# OpenCode V2 adapter for Control Vault

This optional example is an OpenCode V2 server plugin. It gives each OpenCode session its own private `control mcp --vault-process` stdio child. A [Vault](../../docs/vault.md) approval then covers one conversation or one subagent rather than every chat in the editor.

It targets the published V2 plugin contract (`@opencode/plugin` 2.4.1): `Plugin.define`-compatible `{ id, setup }`, `ctx.tool.transform`, the tool execution context, `ctx.session.get` and `ctx.event.subscribe`. It has no runtime dependencies. `@opencode/plugin` is imported for types only, and the MCP client is a small bounded stdio implementation in `src/stdio-mcp.ts`.

## What it adds

A single tool, `control_vault_call`:

| Input | Effect |
| --- | --- |
| `{ "describe": true }` | Lists this session's Vault tools and the server instructions. Invokes nothing. |
| `{ "describe": true, "tool": "control_nodes" }` | Returns that tool's description and input schema. Invokes nothing. |
| `{ "tool": "control_nodes", "arguments": { ... } }` | Calls that one Control MCP tool through this session's Vault process. |

Any other input field is rejected, including `sessionID`. The tool name must be one advertised by this session's own child. Arguments are passed to that tool unchanged and never affect routing.

## Session routing

- **Identity comes from OpenCode.** The executor routes by `context.sessionID`, which OpenCode supplies for every tool call. The model cannot pick a session or a process. Before starting a child, the plugin confirms the session with `ctx.session.get` and records its `parentID`. A malformed or unknown session ID fails closed.
- **One child per live session.** Each root session, each subagent (child) session and each sibling gets a separate process and a separate broker connection. Concurrent first calls from one session share the process being started. No path routes one session's call to another session's process.
- **No identity claims to the child.** The command is always `control mcp --vault-process`, with nothing appended. Neither argv nor the environment carries a session ID, label, name or token. The frontend reports its own fixed caller label, `MCP stdio connection (all callers on this connection)`. With this adapter, the only caller on that connection is one OpenCode session.
- **The host must not hold Control credentials.** Before every call, and again just before each spawn, the plugin checks its own (the OpenCode server's) environment for nonempty `CONTROL_TOKEN`, `CONTROL_USER_KEY` or `CONTROL_SUPERUSER_KEY`. These are the credential variables that Control's frontend treats as ambient. If one is set, the call fails and no process starts; the error names the variable but never shows its value. An OpenCode process holding a fleet credential could use it without Vault, so hiding it from the child would protect nothing. Matching is case-insensitive. The same variables, even empty ones, are always removed from the child's environment. Credential files are checked by the frontend itself (login files and accessible node configurations in `CONTROL_HOME`, `CONTROL_CONFIG`), and it refuses to start if it finds any.

## Approving a session

A fleet call from an unapproved session returns `approval_required` with a short request code. Vault sends nothing to a worker in that case. Because the code comes back through the calling session's own child, it appears only in that chat. Its progress updates and errors stay there too.

1. In the chat you intend to authorize, ask the agent for the request code from its last `control_vault_call` result.
2. In your own terminal, run `control vault grant CODE`. Check that the code matches the one in that chat. The caller label in the prompt is reported by the caller and proves nothing; do not approve because of a name.
3. Tell the agent to continue. Calling again is safe only because the earlier call was refused before dispatch.

Approve only codes that came from `control_vault_call`. If the agent also started `control mcp --vault-process` some other way, such as from a shell tool, that is a separate connection with its own code. Each subagent session needs its own approval.

## Lifecycle

| Event | Effect on the session's child |
| --- | --- |
| `session.deleted` | Closed, along with the known children of that session. The session ID is refused afterwards. |
| `session.archived`, `session.moved` | Closed. A later call starts a new, unapproved process. |
| `session.revert.committed` | Closed, because the conversation that led to an approval may have been removed. |
| Idle for `idleMinutes` with no call in flight | Closed, **only if** `idleMinutes` is set above 0. Off by default. |
| Plugin unload or reload | Every child closed. The pool refuses new calls. |
| Event stream lost | Every child closed. Calls are refused until the subscription resumes, so a missed deletion cannot leave an approved process reachable. |
| Child exits or breaks the protocol | Removed. Its in-flight call reports an uncertain outcome. |

Closing a child ends stdin, then escalates to SIGTERM and SIGKILL. According to the [Vault documentation](../../docs/vault.md), a grant ends when its caller connection is lost, so closing a child also ends that child's approval. Nothing carries a grant to a replacement process. When a session's next call starts a replacement, the result begins with a notice saying that earlier approvals did not carry over.

By default, a child lives as long as its session: until deletion, archive, move, revert, plugin unload or event-stream loss. The adapter adds no inactivity timeout. A session that makes no Control calls for a while therefore keeps its approval until the grant's own deadline (24 hours by default) or revocation. It also keeps any foreground forward bound to its connection, just as a normal `control mcp --vault-process` process would. Set `idleMinutes` only if you want inactivity to end approvals and those forwards early.

At most `maxSessions` children run at once. A new session beyond that limit fails with an error; the plugin never evicts another session's process.

## Reset, resume, fork and replay

OpenCode V2 does not tell plugins when an ongoing conversation should lose authority. It also has no notion of a process handle that persists across a server restart. The adapter therefore fails closed:

- **Server restart or resume.** Pool state lives only in memory. Children are closed when the plugin unloads. If the OpenCode server dies instead, their stdin closes and the stdio frontend exits. A resumed session keeps its ID but gets a new, unapproved process on its next call. No notice appears in that case, because the earlier history is gone. The `approval_required` result explains the situation.
- **Fork.** A fork is a new session ID with copied history, so it gets its own unapproved process. A request code copied into the fork's history is stale. Do not grant it.
- **Revert.** A committed revert closes the process, as listed above. A revert that is only staged changes nothing.
- **Interrupt or stop.** The aborted call sends MCP `notifications/cancelled` and reports an uncertain outcome. The process and its approval remain.
- **No replay.** The adapter never retries or replays a call, including after cancellation, timeout, a crash or a replacement process. Errors say the outcome is uncertain. Reconcile the original operation before issuing new work.
- **Compaction** does not change session identity and does not affect the process.

## Isolation claims

The adapter isolates sessions from each other for calls routed through this plugin. One session's model cannot use another session's approved connection. A subagent cannot use its parent's connection, nor the parent its subagent's.

It does not defend against a hostile host. OpenCode supplies the session ID, so a modified OpenCode, another server plugin in the same process, or code that can read or debug that process could misuse any child. Those threats are outside Vault's boundary as well (see [Security boundary](../../docs/vault.md#security-boundary)). An approved session can also do anything its Vault grant allows, through any tool path it has.

## Install

Requirements: OpenCode V2 and a Vault-enabled `control` frontend (`control vault status` reports it enabled). The environment OpenCode's server runs in must not contain `CONTROL_TOKEN`, `CONTROL_USER_KEY` or `CONTROL_SUPERUSER_KEY`; if it does, every call is refused. Don't register `control mcp --vault-process` as an ordinary OpenCode MCP server, because one stdio connection would then be shared by every session. If you have a plain `control mcp` server entry, disable it; Vault refuses that mode anyway.

Load the plugin directory, choosing one of these:

```sh
# Project-local discovery, without editing any config file:
mkdir -p .opencode/plugins
ln -s /path/to/control/examples/vault-opencode .opencode/plugins/control-vault
```

```jsonc
// opencode.jsonc (project) or ~/.config/opencode/opencode.jsonc (global)
{
  "$schema": "https://opencode.ai/config.json",
  "plugins": [
    {
      "package": "/path/to/control/examples/vault-opencode",
      "options": { "controlPath": "/usr/local/bin/control", "maxSessions": 8 },
    },
  ],
}
```

OpenCode reloads plugins when watched config changes; a change inside a symlinked directory may need `/reload`. To confirm that `control.vault` loaded, run `opencode api get /api/plugin` from the project directory, or check the server log for load errors. `opencode plugin add` doesn't accept local paths, so configure the directory directly.

| Option | Default | Meaning |
| --- | --- | --- |
| `controlPath` | `control` (from `PATH`) | Trusted Control binary. The arguments are always `mcp --vault-process`. |
| `maxSessions` | `8` (1–64) | Concurrent children for this plugin instance. |
| `idleMinutes` | `0` (off; 0–1440) | When above 0, close a child after this long without calls. That ends its approval and foreground forwards early. |
| `startupTimeoutSeconds` | `60` | Limit for the frontend to start and answer `initialize`. The frontend contacts the broker lazily, on the first fleet call, so this timeout doesn't cover broker availability. |

Unknown options make the plugin fail to load. OpenCode may load one plugin instance per location, and limits apply per instance.

## Test

The tests use Bun's built-in runner and no network. They start real private child processes from `test/fake-control.ts`. That fixture is a stdio MCP server that refuses any arguments other than `mcp --vault-process`, and refuses to run if any of the three credential variables reaches it, even empty. Its only state is in-process memory, so any sharing between sessions would show up as a shared PID or shared approval state. `test/plugin.test.ts` drives the real plugin entrypoint through a mocked OpenCode context. It doesn't touch your OpenCode configuration or create sessions.

```sh
cd examples/vault-opencode
bun test
```

To type-check against the published plugin types, install the pinned development dependencies first:

```sh
npm install --ignore-scripts && npm run typecheck
```

The tests cover:

- separate processes and approval state for two root sessions, for a parent and its subagents, and for siblings;
- rejection of a model-supplied `sessionID`;
- fixed argv, and credential variables removed from the child;
- refusal before any session check or spawn when the host holds a nonempty credential variable, including for sessions that already have a child;
- termination on `session.deleted` (including subagents), revert, unload and event-stream loss;
- no idle cleanup by default (an approved child survives 48 simulated idle hours), and opt-in idle cleanup;
- the process bound;
- reporting a crash as uncertain, with no replay;
- cancellation;
- approval codes reaching only the requesting session;
- `describe` returning tool information without invoking a tool.

`test/plugin.test.ts` uses a shebang fixture and needs a POSIX system.

### Smoke test with a real binary

```sh
CONTROL_VAULT_SMOKE_BIN=/path/to/control bun test test/smoke.test.ts
```

This runs the frontend with an empty temporary `CONTROL_HOME`. It checks that an inactive frontend refuses, that the adapter reports the failure, and that no process keeps running. The smoke test applies the same host-credential preflight as the plugin. If your shell exports a Control credential variable, the test reports the refusal and never runs the binary. Adding `CONTROL_VAULT_SMOKE_LIVE=1` also runs a check against your activated Vault. That check lists tools, then makes one fleet call from each of two sessions and expects two different `approval_required` codes. It issues no grant and sends nothing to a worker. It leaves up to two pending requests, which expire after five minutes.
