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

TCP forwarding does not rewrite HTTP headers or provide TLS termination. If you need an HTTP-to-HTTPS proxy, run it as a separate application. Its TLS SNI and Host header still need the upstream QA hostname, and that proxy needs its own lifetime management if it must survive a restart.

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

The service's ephemeral management port excludes retained local forward ports, including wildcard listener addresses. A restarting service cannot take a forward's fixed port before that forward has rebound. Ports held by unrelated processes still report a conflict and retry normally.

`service.json` contains the selected account credential and ICE configuration; `tunnels.json` contains desired definitions and absolute expiries. Both are atomically written private files with mode `0600` on Unix. Windows uses the configuration directory's inherited ACLs. Keep the directory outside worker-accessible workspaces and protect it as you protect the saved login. A service lock prevents duplicate runtimes; a startup lock serializes competing launches. The local management API binds only `127.0.0.1`, authenticates with a random private endpoint token, refuses redirects, bounds requests, and exposes no generic execution or credential-read operation. Inspect `service.log` for startup failures. It is rotated at 16 MiB, keeping one previous file as `service.log.1`.

At most 32 retained tunnels and 128 simultaneous sockets per tunnel are supported. All retained tunnels share one account client and gateway connection, so they count as a single client against the account's client limit. User-service entries remain when the last definition is disposed, but an empty service opens no account peer sessions. Keep the executable at the registered path, normally the installed CLI path, for restart recovery.

## Dashboard and MCP

For an agent-browser dashboard already running on a worker, `control browser view NODE --port 4848` returns a local loopback URL and keeps its ordinary forward alive until Ctrl+C. MCP `control_browser_view` returns the URL and forward ID until `control_forward_stop` or process exit. It carries HTTP and WebSocket streams without exposing per-session ports, but does not launch the dashboard or verify readiness. See [remote browser workflow and privacy limits](browser.md).

The machine table shows live `Tunnels` after `D/T/R`, as `↑2 ↓1`. Up counts active forward TCP connections; down counts reverse listeners, including idle listeners. These peer-observed counts cover all owners and orchestrator hosts, including foreground and detached processes, not only this service. An idle local forward listener has no worker activity; its connected sockets appear as `tcp.accept`, then move to Recent when closed.

The separate `P.Tun.` column means permanent tunnels and counts this dashboard host and selected login's retained forward/reverse definitions. It includes definitions waiting to retry, excludes expired definitions, and remains available for offline targets. Details and plain-text output always include it. JSON contains live `tunnels: {forward, reverse}` and local `retainedTunnels: {forward, reverse}`, without tunnel addresses or credentials. A missing or unreadable local tunnel store is shown as unknown where appropriate. Dashboard polling reads the atomic store without starting the tunnel service.

MCP exposes `control_tunnel_start`, `control_tunnel_list`, and `control_tunnel_dispose`. Start requires `id`, `node`, `address`, and a fixed `listen`, with optional `reverse` and `ttlSeconds`. These use the same host service and durable state as the CLI. Explicit local API routing is unsupported for persistent tunnels; use an account login.

The original foreground `control tunnel NODE HOST:PORT` and process-owned MCP `control_forward_start/list/stop` retain their existing lifetimes. Use them for temporary sessions; use the new tunnel commands/tools when the definition must outlive the calling process.

## SOCKS proxies

To reach several hosts on a worker's network through one local port, run a foreground SOCKS5 proxy instead of one tunnel per destination:

```sh
control tunnel worker --socks --listen 127.0.0.1:1080 --max-connections 256
curl --socks5-hostname 127.0.0.1:1080 https://api.internal/health
```

MCP `control_forward_start` takes `socks: true` without `address`. Each SOCKS `CONNECT` opens its own `tcp.open` stream through the worker, which resolves domain names with its own DNS and network. Use `socks5h://` or `--socks5-hostname` so clients send names unresolved. Only "no authentication" and `CONNECT` are supported. A SOCKS listener must be on loopback unless `--allow-non-loopback` is given; a non-loopback listener is an unauthenticated open proxy into everything the worker can reach. SOCKS mode is refused for instruction-only destinations such as Hybrid System companions, and for `--reverse`. Retained `control tunnel start` definitions do not support SOCKS mode in this version. See [SOCKS forwarding details](protocol.md#orchestrator-sessions-and-forwards).

SOCKS defaults to 256 connections per listener. Set `--max-connections N` or MCP `maxConnections` to 1–384; zero or omission uses the default. Connections share the same interactive peer session. Upgrade both ends for more than 128 streams on that session; with explicit local API routing, upgrade the local node too. When slots are busy, a SOCKS request can wait briefly and receives general failure (`0x01`) if admission still fails. MCP `control_forward_list` and `control_session` show the per-listener limit and cumulative `capacityRejections`; a foreground CLI prints nonzero rejections when it exits.

## HTTPS proxies over HTTP/2

Use `control tunnel worker --https-proxy` or MCP `control_forward_start` with `httpsProxy: true` instead of `socks`. The listener is loopback only. Apps explicitly configured to use its URL share an HTTP/2 connection to the listener. HTTPS sites use `CONNECT`, with no website TLS interception. Plain `http://` requests also go through the worker, but are still unencrypted between the worker and the site. The proxy opens an authorized `tcp.open` stream for each `CONNECT` or plain HTTP request. It does not accept HTTP/1.1 proxy clients. Use SOCKS5 for clients that support it, or a fixed TCP forward for an app pointed at one destination.

```sh
control tunnel tallinn --https-proxy --listen 127.0.0.1:8443
```

Use the printed proxy URL and public key pin, or read `proxyUrl` and `proxySpki` from MCP. Each start has a fresh, in-memory proxy certificate, so reuse neither the old pin nor a system-wide certificate override.

For Chrome on macOS, set `PROXY_PIN` to the printed base64 pin and choose a separate profile directory:

```sh
open -n -a "Google Chrome" --args \
  --user-data-dir="$HOME/Library/Application Support/Control/Chrome-Tallinn" \
  --proxy-server=https://127.0.0.1:8443 \
  --ignore-certificate-errors-spki-list="$PROXY_PIN" \
  --force-webrtc-ip-handling-policy=disable_non_proxied_udp
```

The pin permits only certificates matching the proxy key in this launch. Don't use the unscoped `--ignore-certificate-errors` flag. The profile and proxy flags leave system proxy settings and ordinary Chrome launches unchanged; the listener is not exclusive to Chrome. Quit the separate instance before relaunching it with a new pin.

For curl builds with HTTP/2 proxy support, `--proxy-http2 --proxytunnel` selects the same channel without Chrome. `--proxy-insecure` below skips the normal CA check for the self-signed proxy only; the proxy pin is still checked and origin TLS is verified normally.

```sh
curl --proxy-http2 --proxytunnel --proxy https://127.0.0.1:8443 \
  --proxy-insecure --proxy-pinnedpubkey "sha256//$PROXY_PIN" https://example.com/
```

HTTPS uses the same 256 default and 1–384 configurable destination limit as SOCKS. A busy listener briefly queues up to 32 requests before returning HTTP 503 and counting capacity rejections. Closing the forward closes its local listener and all active streams. Older workers can serve this mode with their existing `tcp.open` support, within their peer stream limit.
