# Install through a worker proxy

Some machines cannot reach the gateway directly. They may sit on an isolated LAN, behind a VPN that only another machine joins, or in a subnet with no internet route. A worker that reaches both networks can host an HTTP CONNECT proxy: either separately managed software, or a [Control-managed listener](#managed-listener) that starts and stops with its dependents. Control can then install and run the restricted machine, called the child, through that proxy.

The child keeps its canonical gateway URL, identity, and credentials. Scripts and binaries still come from the gateway and stream through the proxy; the proxy worker stores and prepares nothing per invitation. Gateway TLS is verified end to end by the child. Peer WebRTC sessions, provider requests, and child process environments are unaffected.

This release supports one explicitly selected proxy endpoint per child. Control does not install, configure, start or stop external proxy software, change firewalls, switch routes automatically, chain proxies, or replay application work. The optional managed listener is Control's own fixed-gateway CONNECT server, not a general proxy.

## Requirements

- An HTTPS gateway URL (`CONTROL_PUBLIC_URL` or the address the host supplies). Proxied invitations and profiles reject plain HTTP gateways because TLS verification is mandatory.
- A CONNECT proxy on the proxy worker: either tinyproxy, Squid or similar software maintained separately from Control, or a [managed listener](#managed-listener). An external proxy must allow CONNECT to the gateway's host and port and accept connections from the child's network. Use a stable LAN or VPN address; Control never infers it from the worker's own gateway connection.
- Gateway, proxy worker, published installers, and the orchestrator's CLI or dashboard running a version with gateway proxy support. See [rollout](#rollout).

Proxy endpoints are `http://HOST:PORT` only, without credentials, path, query, or fragment. IPv6 literals use brackets, such as `http://[fd00::5]:3128`. Zoned link-local addresses are rejected because their zone depends on the child's interface naming. Authenticated proxies are not supported.

## Advertise endpoints on the proxy worker

Add the addresses children can reach to the worker's profile and restart its node:

```json
{
  "proxyEndpoints": [
    { "url": "http://10.20.0.5:3128", "interface": "office LAN" },
    { "url": "http://[fd00:20::5]:3128", "interface": "site VPN" }
  ]
}
```

Only explicitly configured endpoints are advertised; Control never publishes every interface. A worker advertises at most eight. A node that uses `gatewayProxy` itself cannot advertise endpoints. Endpoints are part of the signed registration and are visible only within the worker's fleet.

## Managed listener

Set `managed` and an explicit source allowlist on an endpoint to let Control run the proxy itself:

```json
{
  "proxyEndpoints": [
    { "url": "http://10.20.0.5:3128", "interface": "office LAN", "managed": true, "sources": ["10.20.0.0/24"] }
  ]
}
```

The listener:

- binds only the endpoint's own address, which must be an IP literal in a private or VPN range (`10.0.0.0/8`, `172.16.0.0/12`, `192.168.0.0/16`, `100.64.0.0/10`, or `fc00::/7`). Hostnames, wildcard, loopback, link-local and public addresses are refused, as is the local API's address. It is a separate socket from the privileged local API.
- accepts only TCP sources inside `sources`, which takes 1–16 CIDRs or addresses inside those ranges. The worker's own readiness check from the listener address is also allowed. Unlisted sources are closed without a response. `sources` is local configuration and is not advertised.
- answers only `CONNECT` to the canonical gateway host and port from the worker's `gateway` setting. Other hosts, ports, IP spellings, absolute URIs and plain HTTP requests are refused before any upstream connection. It pipes bytes after `200`, so the child still verifies gateway TLS end to end. It has no credentials, cache, logging of payloads, or idle-byte timeout.
- bounds the CONNECT request to 8 KiB and ten seconds, upstream dials to ten seconds, and admitted connections to 256.

The gateway decides when the listener runs, from durable state in its fleet:

| Dependency | Ends when |
| --- | --- |
| Pending invitation selecting the endpoint, including a pending Hybrid companion | It is redeemed, revoked, or expires |
| Enrolled child redeemed through the endpoint, online or offline | It is unregistered, or its identity re-enrolls through another route |
| Unregistered child | It acknowledges its signed retirement, or the owner releases it |

The listener runs while any dependency remains, and is shared by every dependent of the endpoint. Child dependencies are stored in their own gateway table with the child's identity. Clearing invitation history, gateway restarts, child disconnects, and re-registration do not change them. Expiry needs no mutation: it takes effect on the worker's next state read.

The worker reads its own desired listeners with a signed request, on the existing policy hint and its 30-second fallback poll. It persists the last decision before changing listeners and restores it on restart before reaching the gateway. A failed read or state-file write keeps the current listeners and is retried on the next poll; an outage never means "no dependencies". An unreadable or corrupt saved decision prevents startup rather than silently dropping dependencies. When the last dependency ends, the listener closes its listening socket. Tunnels it already admitted continue until either side closes them or the node stops. Control only ever starts and stops this listener; it never stops external proxy software or other processes.

### Readiness and activation

A managed endpoint reports one of these states in addition to the [general ones](#readiness):

| State | Meaning |
| --- | --- |
| `standby` | No dependency; the listener is off. The worker freshly verified the direct gateway path, TLS and `/healthz` the listener would use. Selectable. |
| `ready` | The listener is running and a check through it to the gateway succeeded. |
| `unavailable` | The check failed, or the listener could not bind its address. |

Creating an invitation for a `standby` endpoint is how the first child bootstraps it: the committed invitation becomes the dependency, and the gateway hints the worker. No binary or script is copied to the worker. The response's `proxyActivation` is `active` only when the endpoint was already `ready` and another dependency already kept it running, otherwise `pending`. The CLI then waits up to 90 seconds for an online, enabled worker's own `ready` check dated at or after invitation creation and prints a warning if it is not confirmed. The dashboard and console show the pending state. None of them retry the creation. Each listener change discards the previous managed observation and checks again immediately.

`control machines proxies`, the dashboard, and the console show `managed` endpoints and their dependents as invitations, children, and retiring children. Status reads use a separately locked committed projection, so dependency display does not wait behind gateway database writes. Invitation expiry is evaluated when reading the projection.

### Unregister a child

Unregistering a child revokes its credential, but its listener stays up so the child can still read its signed retirement policy through it. The child persists retirement, then repeats the signed read with that exact revision as an acknowledgement. Only this acknowledgement releases the dependency: disconnection, directory removal, or delivery of the response alone do not. A failed acknowledgement is retried twice, and once more on every later start of the retired node. Inspect-only reads and other identities' proofs cannot release it.

A child unregistered while offline keeps the listener running until it returns. If it never will, release the wait explicitly:

```sh
control machines proxies release CHILD_ID
```

Only the child's fleet owner can release it, and only after unregistration. The release is audited.

### Updates

Admitted managed connections, including pending CONNECT requests, are owned work. A managed node update or startup change reserves maintenance only when none exist, atomically fencing new connections; it never closes an admitted tunnel. Consequently, a proxy worker postpones its own updates while any child holds a gateway connection through it. Stop or move children to apply the update. The fence is lifted if the update does not proceed.

## Readiness

Every minute, independently of dashboard polling, the worker checks each endpoint by:

1. Establishing CONNECT through the endpoint to its own gateway.
2. Completing gateway TLS verification.
3. Fetching the gateway's `/healthz` response.

Each check is bounded to ten seconds and reuses no pooled connection. Results travel in the existing owner health report with a bounded, sanitized failure reason such as `proxy connection failed`, `proxy refused CONNECT: forbidden`, or `gateway TLS verification failed`. The gateway dates every check on its own clock.

Owners see one state per advertised endpoint:

| State | Meaning |
| --- | --- |
| `ready` | The latest check succeeded within the last three minutes. |
| `unavailable` | The latest check failed; the reason is shown. |
| `stale` | The latest observation is older than three minutes. |
| `offline` | The proxy worker is not connected. |
| `unchecked` | No check has been reported since the worker connected. |

Only `ready` endpoints, and managed endpoints on `standby`, of online, enabled workers can be selected. The gateway never probes endpoints itself: that would not prove the child can reach them and would turn the gateway into an arbitrary URL fetcher. Readiness proves proxy-to-gateway access only. It does not prove that the child can reach the endpoint or passes the proxy's source allowlist.

List proxy workers and their readiness:

```sh
control machines proxies
```

## Create a proxied invitation

```sh
control machines add render-01 --platform windows --via office-bridge
control machines add render-01 --platform linux --via office-bridge --proxy-url http://10.20.0.5:3128
```

`--via` accepts the worker's name or stable ID; the CLI sends the stable ID, so renaming the worker never changes a stored selection. When the worker advertises several endpoints, `--proxy-url` is required and the command fails clearly otherwise. Other `machines add` options, including `--service user|system|hybrid` and `auto` naming, combine normally.

In `control dashboard`, press `a` and choose **Connection → Through worker** after the startup context. The wizard lists ready workers, shows non-ready ones with their state and failure, and requires an explicit address when a worker advertises several. The web console's **Add machine** dialog offers the same Connection, Worker, and Address fields. All three produce the same request and command.

The gateway validates, under its fleet lock, that the worker belongs to the requesting fleet, is online and enabled, advertises the endpoint, and has a fresh `ready` observation, or `standby` for a managed endpoint. Invalid or foreign selections fail before any invitation state is written. This API never accepts an arbitrary proxy URL without a worker. The invitation stores a snapshot of the worker's stable ID and the endpoint URL in its existing transactional record, so it survives gateway restarts and later address changes never rewrite it.

The creation response, invitation registry, and dashboards show the selection. Clients verify that the gateway echoed the requested worker and endpoint. An unconfirmed or uncertain response never triggers creating another invitation; check `control machines invites` first.

## Install the child

Run the printed command on the child exactly as for a direct invitation. It fetches the gateway's HTTPS URL through the selected endpoint:

```sh
curl -fsSL --proxy 'http://10.20.0.5:3128' --noproxy '' --proto '=https' --proto-redir '=https' 'https://control.example.com/install/TICKET' | bash
```

```powershell
irm -Proxy 'http://10.20.0.5:3128' -MaximumRedirection 0 -ErrorAction Stop 'https://control.example.com/install/TICKET'|iex
```

The generated script embeds the selection and passes it explicitly to every later stage: architecture-specific binary download, invitation inspection and preflight, signed policy inspection, redemption, service-start readiness, and Hybrid companion scripts and enrollment. Unix scripts use `--proxy URL --noproxy ''`, which overrides inherited `NO_PROXY`; PowerShell passes `-Proxy` on every download. Nothing depends on environment inheritance through `sudo`, UAC, or limited-token helpers.

Proxied curl downloads permit HTTPS only, including redirects. Proxied PowerShell downloads refuse redirects entirely. This prevents a gateway redirect from exposing the invitation ticket or downloaded script over plain HTTP. Windows firewall setup permits the selected proxy's TCP port for both the installed CLI and managed runtime, including Hybrid profiles.

After verifying the pinned binary's checksum, the script runs the hidden `__installer-features` command and requires its explicit `gatewayProxy` capability before preflight or any installation change. A binary without it stops the installation; publish a compatible release and create a new invitation. The enrollment itself requires `control enroll --gateway-proxy URL`, so older binaries reject the unknown flag instead of enrolling directly. The signed redemption proof echoes the endpoint; the gateway refuses a proxied ticket redeemed without it, before consuming the ticket.

The installer saves the route in pending enrollment, then commits it to the node profile as `gatewayProxy` before service startup:

```json
{
  "gateway": "https://control.example.com",
  "gatewayProxy": { "url": "http://10.20.0.5:3128", "worker": "9f2c…" }
}
```

Retries of the same pending enrollment reuse its saved route and identity, even if run without the flag; a different `--gateway-proxy` is refused. Runtime connections depend only on the saved profile, never on the invitation ticket, so ticket expiry does not disconnect an installed child.

### Manual enrollment

A verified, pre-provisioned binary can enroll through a proxy without a proxied invitation:

```sh
control enroll --url https://control.example.com/install/TICKET --gateway-proxy http://10.20.0.5:3128
```

A proxied invitation requires its exact endpoint. `--gateway-proxy direct` removes a saved proxy. Without the flag, re-enrollment preserves the existing profile's route; select a replacement explicitly.

## Runtime behavior

With `gatewayProxy` configured, every gateway request uses it: authentication, the WebSocket connection and relay, directory and peer lookups, placement reads, signed policy and retirement reads, update selection, and managed binary downloads. Gateway traffic:

- uses the proxy regardless of inherited `HTTP_PROXY`, `HTTPS_PROXY`, or `NO_PROXY`;
- never falls back to a direct connection;
- rejects plain HTTP requests and HTTPS-to-HTTP redirects before sending credentials or tickets to the proxy;
- leaves global HTTP defaults and provider/child environments unchanged;
- streams managed updates from the gateway only, skipping direct GitHub release downloads. The proxy keeps no binary cache.

The network monitor watches the local route to the proxy rather than the public gateway, whose name the child may not resolve. A proxy-only outage disconnects the gateway connection and its relayed sessions, which reconnect with the usual backoff when the proxy returns. Established direct WebRTC sessions continue. Accepted tasks keep running; interrupted streams fail and are never replayed.

`control login` reuses the selected node profile's route only when logging in to that profile's canonical gateway. An explicit login to another gateway does not inherit the proxy. Persistent tunnel services also save their selected route and restore it on restart. Older tunnel-service profiles that saved an empty route cannot recover a lost proxy address automatically; explicitly reconfigure those profiles before restarting them.

## Address changes and maintenance

A proxy endpoint should keep a stable address. If it changes, Control does not rewrite existing invitations or installed profiles. Create a replacement invitation, or change the child profile's `gatewayProxy` explicitly and restart its node. Renaming the proxy worker changes nothing: selections use its stable ID.

Maintain external proxy software separately from Control node updates. Restarting the proxy disconnects its children's gateway traffic until it returns. Managed updates of the proxy worker's node do not affect an external proxy process. A managed listener stops with its node, so its updates wait for idle tunnels as described in [updates](#updates).

## Rollout

1. Deploy gateway support. Managed listeners need a gateway advertising `managedGatewayProxy` in `GET /v1/auth`; its database migrates to schema version 8, which older gateways refuse to open.
2. Publish compatible installer binaries with `make update` or a release.
3. Upgrade workers that advertise proxy endpoints. A managed worker keeps its listeners unchanged and reports no managed readiness while its gateway lacks support.
4. Upgrade CLI and dashboard clients.
5. Create proxied invitations.

Children of a managed endpoint must run a version that acknowledges retirement; older children keep their dependency until the owner releases it. Mixed versions fail closed. Clients refuse to submit a proxied invitation unless the gateway advertises `gatewayProxy` in `GET /v1/auth`, and reject responses that do not echo the selection. Older installers fail the capability check or the enrollment flag before redemption, without consuming the ticket. Direct invitations, scripts, and redemption proofs are unchanged. New registration fields are signed only for gateways that accept exact registration bytes.

## Verification

`make test-compose-proxy` runs the external proxy topology described in [testing](testing.md#gateway-proxy-isolation). `CONTROL_TEST_MANAGED_PROXY=true make test-compose-proxy` uses the same isolated networks with Control's own listener, checking standby bootstrap, restart recovery, retirement, retention for another pending invitation, and shutdown after the final dependency. Native Windows, macOS, and Linux installation checks remain necessary for service startup, elevation, and proxy propagation; cross-compilation does not verify them.

`internal/node` tests cover managed listener target restriction, source refusal, buffered handshake bytes, update fencing, deactivation without cutting tunnels, shutdown joins, bind failure and standby readiness, state-file failure recovery, and retirement acknowledgement retries. `internal/gateway` tests cover bootstrap from standby, revocation and expiry, Hybrid companions, persistence across history clearing and restarts, offline unregistration, exact acknowledgement, owner release, route replacement, fleet separation, and status reads while the persistence lock is held.

See also [configuration](protocol.md#gateway-proxy), [installation](installation.md#install-through-a-worker-proxy), [D146](decisions.md#d146-explicit-worker-proxy-for-isolated-machines), and [D147](decisions.md#d147-gateway-owned-lifecycle-for-managed-proxy-listeners).
