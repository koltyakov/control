# Persistent TCP tunnels

[README](../README.md) · [Protocol](protocol.md#orchestrator-sessions-and-forwards) · [Dashboard](dashboard.md) · [Installation](installation.md)

`control tunnel start` retains a forward or reverse listener in a separate user-level client service on the orchestrator host. It returns after committing the definition, without occupying the invoking terminal. The service is not a fleet node and does not enroll the host.

## Start, inspect, and dispose

Log in with an account key first. Use a fixed listen port so restoration binds the same address:

```sh
control tunnel start tj-vm '[::1]:5173' --reverse --listen 127.0.0.1:5173 --id frontend
control tunnel start tj-vm api.westus3.qa.traderjoes.cloud:443 --listen 127.0.0.1:15174 --id qa-api
control tunnel start tj-vm qil.westus3.qa.traderjoes.cloud:443 --listen 127.0.0.1:15175 --id qa-content
control tunnel list
control tunnel dispose frontend
control tunnel dispose qa-api
control tunnel dispose qa-content
```

The reverse frontend listener is on `tj-vm`; its connections reach Vite on this orchestrator. The ordinary QA listeners are on this orchestrator; the destination machine dials their QA hosts. Use `127.0.0.1:5173` instead of `[::1]:5173` if Vite listens on IPv4.

TCP forwarding does not rewrite HTTP headers or provide TLS termination. The example's local HTTP-to-HTTPS proxy remains a separate application. Its TLS SNI and Host header still need the upstream QA hostname, and that proxy needs its own lifetime management if it must survive a restart.

An explicit ID makes uncertain starts reconcilable. The CLI prints the ID before requesting creation. Repeating an existing ID with the same specification returns the retained definition without replacing it or renewing its TTL. A changed specification fails. When a request fails, list the tunnels before retrying; do not create a replacement with a new ID automatically. Generated IDs are used when `--id` is omitted.

`start` returns JSON with `state: "starting"` after durable acceptance. It does not guarantee that the port has already bound or that the upstream service is available. `list` reports `starting`, `active`, or `retrying`, plus `activeConnections`, `lastError`, the pinned `nodeId`, and the absolute expiry. Port conflicts and denied permissions remain visible and retry with backoff. No dummy upstream connection probes readiness. Starting initially requires an online, enabled, policy-acknowledged, compatible target.

`dispose ID`, also available as `stop ID`, removes the definition durably before closing its listener and active sockets. It is idempotent if absent. A disposed tunnel cannot return on restart. Disposal and listing use the local service and still work while the gateway is unreachable. They do not stop Vite, the QA endpoint, or any other destination service.

## Lifetime and recovery

Without `--ttl`, a definition remains until disposed. A TTL is measured once from creation:

```sh
control tunnel start worker db.internal:5432 --listen 127.0.0.1:15432 --id database --ttl 2h
```

TTLs accept whole seconds from 1s through 8760h; zero means no expiry. Reconnection, destination restart, client restart, and service restart never renew expiry. Expired definitions cannot bind a listener, even if the host was shut down when they expired. The service removes expired definitions and closes their sockets.

The service retries listener establishment with backoff from one to 30 seconds. A reverse listener reconnects after its peer stream fails or reaches the receiving node's 24-hour stream limit. A forward listener stays available locally and opens a fresh peer stream for each new socket. Gateway sessions reconnect through the existing transport implementation. Interrupted TCP connections are closed, never replayed or resumed; applications must establish new connections themselves. A restored reverse listener can briefly be unavailable while the previous bind is released.

Targets are pinned to their immutable machine IDs. Renaming a machine preserves its tunnels. Unregistering a machine does not redirect a retained definition to a replacement that uses the old name. Authorization and admission are checked normally on each peer operation; persistent tunnels do not grant ambient worker authority or bypass disabling, access rules, or credential revocation.

## User service and private state

The first start installs separate user startup:

| Platform | Startup |
| --- | --- |
| macOS | Profile-scoped `com.koltyakov.control.tunnels.*` LaunchAgent, with crash restart |
| Linux | Profile-scoped `control-tunnels-*.service` under `systemd --user`, enabled at user-manager startup |
| Windows | Profile-scoped `ControlTunnels-*` login scheduled task under the user's limited interactive token, with failure retries |

Restoration after a host reboot begins when that user's startup manager runs. Windows requires login. Linux needs linger if the service must run before login or after logout, as described under [startup contexts](installation.md#choose-a-startup-context). Default startup fails visibly if the user service manager is unavailable; it does not silently promise reboot recovery from a detached process. It does not alter the node's startup, firewall, or elevation.

For containers or deliberate process-only use, `CONTROL_TUNNEL_SERVICE_MODE=process` launches a detached service. It survives CLI exits, but has no OS login or crash supervision. Calling `tunnel list` or `start` restarts a stopped service and restores its retained definitions. Do not use process-only mode when automatic reboot recovery is required.

State lives under `tunnels/<login scope>/` in the Control configuration directory, separate from node state and workspaces. The scope includes the gateway URL and selected login credential. Concurrent CLI/MCP processes using that login share the service; other credentials or gateways use separate services. Rotating a credential does not silently migrate its old definitions or substitute another account's authority. The old service cannot perform newly authorized operations with a revoked credential.

When restarting a stopped service, the client waits for its previous owner to release the runtime lock before launching a replacement. Shutdown keeps the endpoint file until owned forwards have stopped. Closing a reverse listener still requires the close to reach the worker, so its remote port can take a short time to be released.

`service.json` contains the selected account credential and ICE configuration; `tunnels.json` contains desired definitions and absolute expiries. Both are atomically written private files with mode `0600` on Unix. Windows uses the configuration directory's inherited ACLs. Keep the directory outside worker-accessible workspaces and protect it as you protect the saved login. A service lock prevents duplicate runtimes; a startup lock serializes competing launches. The local management API binds only `127.0.0.1`, authenticates with a random private endpoint token, refuses redirects, bounds requests, and exposes no generic execution or credential-read operation. Inspect `service.log` for startup failures.

At most 32 retained tunnels and 128 simultaneous sockets per tunnel are supported. User-service entries remain when the last definition is disposed, but an empty service opens no account peer sessions. Keep the executable at the registered path, normally the installed CLI path, for restart recovery.

## Dashboard and MCP

The machine table shows live `Tunnels` after `D/R`, as `↑2 ↓1`. Up counts active forward TCP connections; down counts reverse listeners, including idle listeners. These peer-observed counts cover all owners and orchestrator hosts, including foreground and detached processes, not only this service. An idle local forward listener has no worker activity; its connected sockets appear as `tcp.accept`, then move to Recent when closed.

The separate `P.Tun.` column means permanent tunnels and counts this dashboard host and selected login's retained forward/reverse definitions. It includes definitions waiting to retry, excludes expired definitions, and remains available for offline targets. Details and plain-text output always include it. JSON contains live `tunnels: {forward, reverse}` and local `retainedTunnels: {forward, reverse}`, without tunnel addresses or credentials. A missing or unreadable local tunnel store is shown as unknown where appropriate. Dashboard polling reads the atomic store without starting the tunnel service.

MCP exposes `control_tunnel_start`, `control_tunnel_list`, and `control_tunnel_dispose`. Start requires `id`, `node`, `address`, and a fixed `listen`, with optional `reverse` and `ttlSeconds`. These use the same host service and durable state as the CLI. Explicit local API routing is unsupported for persistent tunnels; use an account login.

The original foreground `control tunnel NODE HOST:PORT` and process-owned MCP `control_forward_start/list/stop` retain their existing lifetimes. Use them for temporary sessions; use the new tunnel commands/tools when the definition must outlive the calling process.
