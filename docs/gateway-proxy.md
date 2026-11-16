# Install through a worker proxy

Some machines cannot reach the gateway directly. They may sit on an isolated LAN, behind a VPN that only another machine joins, or in a subnet with no internet route. A worker that reaches both networks can host a separately managed HTTP CONNECT proxy. Control can then install and run the restricted machine, called the child, through that proxy.

The child keeps its canonical gateway URL, identity, and credentials. Scripts and binaries still come from the gateway and stream through the proxy; the proxy worker stores and prepares nothing per invitation. Gateway TLS is verified end to end by the child. Peer WebRTC sessions, provider requests, and child process environments are unaffected.

This release supports one explicitly selected proxy endpoint per child. Control does not install or configure the proxy, change firewalls, switch routes automatically, chain proxies, or replay application work. An embedded proxy listener is not implemented.

## Requirements

- An HTTPS gateway URL (`CONTROL_PUBLIC_URL` or the address the host supplies). Proxied invitations and profiles reject plain HTTP gateways because TLS verification is mandatory.
- A CONNECT proxy on the proxy worker, such as tinyproxy or Squid, maintained separately from Control. It must allow CONNECT to the gateway's host and port and accept connections from the child's network. Use a stable LAN or VPN address; Control never infers it from the worker's own gateway connection.
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

Only `ready` endpoints of online, enabled workers can be selected. The gateway never probes endpoints itself: that would not prove the child can reach them and would turn the gateway into an arbitrary URL fetcher. Readiness proves proxy-to-gateway access only. It does not prove that the child can reach the endpoint or passes the proxy's source allowlist.

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

The gateway validates, under its fleet lock, that the worker belongs to the requesting fleet, is online and enabled, advertises the endpoint, and has a fresh `ready` observation. Invalid or foreign selections fail before any invitation state is written. This API never accepts an arbitrary proxy URL without a worker. The invitation stores a snapshot of the worker's stable ID and the endpoint URL in its existing transactional record, so it survives gateway restarts and later address changes never rewrite it.

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

Maintain the proxy software separately from Control node updates. Restarting the proxy disconnects its children's gateway traffic until it returns. Managed updates of the proxy worker's node do not affect an external proxy process.

## Rollout

1. Deploy gateway support.
2. Publish compatible installer binaries with `make update` or a release.
3. Upgrade workers that advertise proxy endpoints.
4. Upgrade CLI and dashboard clients.
5. Create proxied invitations.

Mixed versions fail closed. Clients refuse to submit a proxied invitation unless the gateway advertises `gatewayProxy` in `GET /v1/auth`, and reject responses that do not echo the selection. Older installers fail the capability check or the enrollment flag before redemption, without consuming the ticket. Direct invitations, scripts, and redemption proofs are unchanged. New registration fields are signed only for gateways that accept exact registration bytes.

## Verification

`make test-compose-proxy` runs the isolated topology described in [testing](testing.md#gateway-proxy-isolation). Native Windows, macOS, and Linux installation checks remain necessary for service startup, elevation, and proxy propagation; cross-compilation does not verify them.

See also [configuration](protocol.md#gateway-proxy), [installation](installation.md#install-through-a-worker-proxy), and [D146](decisions.md#d146-explicit-worker-proxy-for-isolated-machines).
