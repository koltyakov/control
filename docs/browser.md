# Remote browser through Control

[README](../README.md) · [Protocol](protocol.md#browser-automation) · [Tunnels](tunnels.md) · [Desktop](desktop.md)

`browser.run` operates an installed [agent-browser](https://github.com/vercel-labs/agent-browser) CLI on a worker. `control browser view` forwards its web dashboard to a loopback URL on the orchestrator machine. The worker runs the website and retains its network access, cookies and downloaded files. The local browser receives viewport frames and sends input, without loading the internal website itself.

Control does not implement a browser engine, streaming codec or public browser proxy. It reuses agent-browser's bundled dashboard and WebSocket streaming through existing encrypted peer TCP forwarding. The gateway does not serve browser sessions or authenticate viewers with console cookies.

## Setup

Install agent-browser and its companions on the intended worker only after approval. This adapter requires stable **agent-browser 0.41.2 or newer**, with native namespace/configuration support and the bundled dashboard. It checks the configured binary's version before each invocation and refuses older or unrecognized builds before running the requested command. Use an official upstream release/package for its OS and architecture and verify available release checksums. Configure an absolute native executable path in the existing node profile, preserving other settings:

```json
{
  "browser": {
    "cli": "/absolute/path/to/agent-browser"
  }
}
```

Use `agent-browser.exe` on Windows, not an npm `.cmd` shim. The executable must run without a shell. Restart the node while idle and rediscover its capabilities. Control does not bundle, download or automatically install agent-browser, Chrome or other engines. Managed Control updates do not update these dependencies.

For web tasks, load installed-version guidance first:

```sh
control browser run worker -- --version
control browser run worker -- skills get core
```

The default engine is Chrome for Testing. Because Control gives the CLI a private home, a Chrome download under the worker's ordinary home is not necessarily discoverable. After explicit installation approval, run:

```sh
control browser run worker -- install
```

Alternatively, set optional absolute `browser.config` to an operator-owned upstream JSON configuration with an `executablePath` pointing to an existing compatible Chrome. That file selects trusted upstream settings instead of the empty default configuration. Consult installed upstream help for engine, proxy and policy configuration; Control does not silently change engines on failure. Use Chrome for visual streaming. Lightpanda does not provide equivalent graphical rendering.

Run as the intended unprivileged User context. Missing dependencies or permissions do not authorize switching to System, disabling the browser sandbox, or granting OS permissions. A headless website does not need a visible desktop, but security-key prompts or native dialogs may need additional interaction.

## Open the worker browser locally

Start a named headless session and explicitly start its owner's dashboard on a chosen worker port:

```sh
control browser run worker --session work -- open https://internal.example.com
control browser run worker -- dashboard start --port 4848
control browser view worker --port 4848
```

Open the printed `http://127.0.0.1:PORT` URL on the orchestrator. Keep the last command running. It binds a loopback ephemeral port by default; use `--listen 127.0.0.1:14848` for a fixed local URL. IPv6 loopback listeners are also supported. Public/wildcard listeners and hostname listen addresses are refused.

The dashboard serves its own assets and proxies session WebSockets internally, so forwarding individual session ports or raw Chrome CDP is unnecessary. The worker dashboard port and the orchestrator listen port may differ. With default upstream origin settings, local loopback dashboard origins are permitted; do not add public allowed origins just for a Control loopback forward.

`view` forwards an **already started** dashboard. It does not start a browser/dashboard, test readiness, select a session, or verify which application owns the chosen port. Inspect upstream start results and ensure the worker port is free before starting a new dashboard. Choose a different worker port for each concurrently running owner dashboard; namespaces do not isolate TCP port numbers. A failed start or blank view is not permission to terminate another process or automatically repeat browser actions.

Ctrl+C closes the local forward and its sockets, not the remote dashboard or browser. For continued automation from another terminal, retain the same account-client owner and session:

```sh
control browser run worker --session work -- snapshot -i
```

Refs belong to that browser session and current observation. Refresh after navigation and after human input; do not reuse stale refs or replay a command whose effects are uncertain.

## Human takeover and 2FA

Pause AI mutations before the human uses the streamed page. The human can enter credentials and one-time codes directly, or approve an expected phone notification. After explicit hand-back, take a fresh snapshot and verify non-sensitive login state before continuing. Control does not enforce an automatic human/AI input lock across dashboard input and CLI calls; coordinate ownership explicitly.

Do not request passwords or verification codes in chat, place them in task arguments/stdin, or retrieve them with shell, clipboard, environment or file operations. This integration does not resolve Control secret references and does not inherit `desktop.run`'s protected private-pipe credential entry. Text redaction only masks configured worker secret values; it cannot discover arbitrary one-time codes.

Viewport input does not forward a local security key, passkey authenticator, OS dialog, camera, microphone or desktop. Support depends on the target site's flow and worker environment. Headless streaming is not a general remote desktop.

## MCP workflow

MCP clients do not need the Control skill installed. Discover `browser.run` with `control_describe` on the intended User worker before calling `control_browser`. Its `request` has structured `args`, an optional `session` and optional non-secret `stdin`. Use the same worker and stable owner throughout the sequence.

| Tool | Arguments |
| --- | --- |
| `control_browser` | `{"node":"worker","request":{"args":["--version"]}}` |
| `control_browser` | `{"node":"worker","request":{"args":["skills","get","core"]}}` |
| `control_browser` | `{"node":"worker","request":{"session":"work","args":["open","https://internal.example.com"]}}` |
| `control_browser` | `{"node":"worker","request":{"session":"work","args":["snapshot","-i"]}}` |

Read the returned installed-version guidance before acting. Load it once per worker, backend and version per session, and reload if those change. Refresh refs after navigation or human input. Installation still needs approval; missing browser support does not authorize installation or a System-context fallback. Default state is private to the profile/owner, not the worker's ordinary Chrome profile. Commands share `maxTasks` and serialize per owner, not against dashboard input. Explicit upstream overrides are not a sandbox.

For [manual login/2FA](#human-takeover-and-2fa), pause AI mutations and explicitly start the dashboard before forwarding it:

| Tool | Arguments |
| --- | --- |
| `control_browser` | `{"node":"worker","request":{"args":["dashboard","start","--port","4848"]}}` |
| `control_browser_view` | `{"node":"worker","port":4848}` |

Choose a different worker port for each concurrent owner dashboard. Inspect the start result before opening the view. The view does not start services or verify readiness and gives interactive access to every session in that namespace. Its loopback URL is on the MCP server host, not necessarily the AI client's machine. Frames, input and upstream activity are not redacted; never send passwords or one-time codes through `control_browser`. Security keys/passkeys are not forwarded. Resume automation only after explicit hand-back and a fresh snapshot.

Stop the local view with `control_forward_stop` and its returned forward `id`. This does not stop remote processes. Close only sessions you created with `control_browser` request `{"session":"work","args":["close"]}`, and stop the dashboard when no longer needed with `{"args":["dashboard","stop"]}`, using the same worker and owner/profile. Daemons can survive calls and Control restarts; a completed task is not browser shutdown. See [state, privacy and cleanup](#state-privacy-and-cleanup) for retention and lifecycle limits. Never replay uncertain effects.

## CLI, MCP and tasks

The capability accepts structured arguments, optional session and optional non-secret stdin:

```sh
control call worker browser.run '{"session":"work","args":["snapshot","-i"]}'
control task start worker --wait '{"id":"browser-capture-001","capability":"browser.run","args":{"session":"work","args":["screenshot","page.png"]},"outputs":["page.png"]}'
```

Every upstream command is forwarded without a shell or Control command whitelist. Control requests JSON output by default. Session names in the structured `session` field are 1..32 ASCII letters, digits, underscores or hyphens, starting with a letter or digit. Omitted session uses `default`. Explicit upstream flags can override default configuration, session, profile or namespace; this is not a sandbox or an owner-isolated execution environment for unrestricted code.

MCP `control_browser` takes `node` and the same `request` object. `control_browser_view` takes `node`, optional worker `port` and optional loopback `listen`, returning `url` and the ordinary forward `id`. Its URL is on the MCP server host, which must be the machine running the human's browser or otherwise reachable through an explicitly authorized route. The view survives tool-call completion until `control_forward_stop` or MCP/client exit. Inspect `control_forward_list` for connection errors. No request starts or retries a login automatically.

Browser commands share `maxTasks` execution admission. An owner-specific file lock serializes CLI commands, including those in tracked tasks. Independent owners have different default namespaces. The lock does not exclude humans, dashboard operations, external processes, or an explicit upstream namespace override. Existing authorization, delegation, task ownership, leases, maintenance admission, cancellation and idempotent task submission apply. Use leased tasks with `leaseId` for every command in a leased sequence; the direct convenience CLI cannot carry a lease.

Each CLI invocation uses the caller/task deadline and existing process-tree cancellation. Cancellation cannot undo effects or guarantee termination of an already detached upstream daemon. There is no automatic replay. Use explicit task IDs for retained failures and artifacts. Save file outputs in the task workspace and declare them; large artifacts use existing direct delivery instead of embedding bytes in orchestration messages.

For direct `screenshot [selector] PATH` commands, Control resolves a relative output path against the current invocation's workspace before forwarding it. This also works when the session's daemon started in another directory. Absolute paths are unchanged. Batch commands and ambiguous or unfamiliar flag syntax remain untouched; use absolute worker paths in those cases. Other file-producing commands retain upstream path behavior.

## State, privacy and cleanup

Defaults are private to the profile and stable caller owner. Control uses `<dataDir>/browser/cHASH` as the upstream home and an opaque profile/owner namespace. It replaces inherited `AGENT_BROWSER_*`, home and XDG home settings, and supplies an explicit empty configuration unless `browser.config` is set. This prevents ambient sessions/configuration from silently selecting another browser or enabling tracing. Chromium, session state and upstream auth vault files under that home are separate from the worker's ordinary browser home.

Runtime socket/PID/stream files use the OS user's cache directory under `control/b`, then upstream namespace directories, keeping Unix-domain socket paths shorter than paths under arbitrary task workspaces. Both private roots must be outside `workDir`, and symlink directory leaves are rejected. Unix directories are mode `0700`; Windows depends on the worker profile's ACLs. Long user-cache paths or explicit long upstream session names can still exceed upstream Unix socket limits.

Decoded argv/stdin are limited to 256 arguments and 1 MiB combined UTF-8 bytes, with NUL refused. Platform process limits may be lower. Each output stream is captured up to 2 MiB. Raw stdout/stderr never enter task logs. The result is `{success,data,process:{exitCode,stderr}}`; `data` retains the upstream JSON value or wraps text as `{stdout}`. Nonzero exit, an upstream `success:false`, or failed array/batch entries fail Control execution while tracked tasks retain sanitized partial results. Oversized, binary or launch-failure diagnostics are suppressed rather than publishing partial raw output.

Failed convenience CLI/MCP calls include the exit code and up to 4 KiB of the already-sanitized result in the error, with an explicit truncation notice when needed. They still fail and never replay the command. Use a tracked task when the complete sanitized failure result is needed.

Text results mask configured worker secret values and private home/runtime roots. **Live frames, input, screenshots, downloads, upstream console/activity feeds, traces, cookies and saved state are not redacted.** Do not enable sensitive recording or export profiles as ordinary task outputs. The local dashboard grants interactive authority over all sessions in that namespace, including session creation, rather than read-only access to one page. Keep both ends on loopback and use authorized Control forwarding. Do not expose raw CDP or dashboard ports publicly. Other processes running as the same OS user, or providers with unrestricted execution, remain trusted.

The stream uses JPEG frames over WebSocket, not a video codec or WebRTC media track. Upstream quality, dimensions, FPS limits and acknowledgement pacing can reduce bandwidth and stale-frame delay. Control carries those bytes on its existing reliable interactive lane with direct WebRTC or encrypted relay fallback; it does not promise video-grade latency or bandwidth reservations.

When finished, close only sessions you created and stop the dashboard when no longer needed:

```sh
control browser run worker --session work -- close
control browser run worker -- dashboard stop
```

These commands must use the same owner/profile as startup. Upstream sessions and dashboard processes are externally managed, can outlive a CLI call or Control restart, and are not active Control tasks between invocations. An active forwarded connection holds normal tunnel admission; an idle external browser does not reserve Control against managed updates. Upstream idle expiry may close a headless session, but is not a substitute for explicit cleanup. Closing a browser does not delete its saved state or downloaded files; manage retention separately with appropriate authorization.

## Verification

Core tests use a native Go subprocess stand-in and a synthetic dashboard, never installed browsers or real credentials. They check request bounds, configuration opt-in, default owner isolation, inherited-setting removal, text redaction, empty raw logs, failures, cancellation, artifact publication, task reconciliation, CLI/MCP routing and loopback-only views. Peer fixtures carry dashboard HTTP and duplex WebSocket frames/input over both loopback WebRTC and forced relay.

Actual agent-browser/Chrome launch, dashboard rendering, human input, target-site authentication and native Windows/macOS/Linux behavior require an installed-version smoke test using the workflow above. Synthetic frames and cross-builds do not establish browser-engine compatibility or live 2FA support.
