# Install Control behind expose

[README](../README.md) · [Linux VPS gateway](gateway.md) · [Installation](installation.md) · [Managed updates](updates.md)

Use [expose](https://github.com/koltyakov/expose) for public HTTPS while the Control gateway listens on loopback. On the same VPS, a server-local bypass route needs no expose client or API key:

```text
CLI and nodes → https://control.example.com → expose server → 127.0.0.1:7330
```

Expose terminates HTTPS and forwards Control's HTTP and WebSocket endpoints. Peer execution and artifacts still use Control's pinned TLS over WebRTC or its encrypted gateway relay. Do not publish the node API on port 7331.

## 1. Prepare expose

For a new server, follow expose's [VPS deployment guide](https://github.com/koltyakov/expose/blob/main/docs/vps-deployment.md). This example assumes:

- `EXPOSE_DOMAIN=example.com`, with DNS for `control.example.com` pointing to the expose server.
- Public TCP 443 reaches expose's HTTPS listener. TCP 80 reaches its ACME listener when using HTTP-01 certificates.
- A build supporting `EXPOSE_BYPASSES` and the `/_expose/v1` service namespace. Older expose versions using `/v1` conflict with Control's API paths.

Expose defaults to internal ports 10443 and 10080. Keep the existing port mappings, or use its documented privileged-port service configuration. Do not start Caddy or a second service on the same public ports.

## 2. Install the Control gateway

Follow steps 2 through 4 of the [Linux VPS guide](gateway.md#2-install-the-binary) to install the binary, create a private service account and operator key, and enable `control-gateway.service`. Skip its Caddy setup.

In `/etc/control-gateway/environment`, set the public address before starting the gateway:

```dotenv
CONTROL_PUBLIC_URL=https://control.example.com
```

Keep the generated `CONTROL_SUPERUSER_KEY` in that private file and leave `CONTROL_TOKEN` unset for a new deployment. The systemd unit should retain:

```ini
ExecStart=/usr/local/bin/control gateway --listen 127.0.0.1:7330 --data /var/lib/control-gateway
```

Restart after changing the environment, then check the local listener:

```sh
sudo systemctl restart control-gateway
curl -fsS http://127.0.0.1:7330/healthz
```

Expect `ok`. Keep port 7330 closed to the public network. `CONTROL_PUBLIC_URL` controls generated installation links; use the HTTPS hostname, not the loopback address.

## 3. Add the expose route

Add this entry to the expose server's configuration. With the linked VPS layout, edit `/opt/expose/.env` using `sudoedit`:

```dotenv
EXPOSE_BYPASSES='[{"subdomain":"control","port":7330}]'
```

If `EXPOSE_BYPASSES` already contains routes, append the Control entry to its JSON array rather than replacing them. An environment value or `--bypasses` flag overrides the `.env` value. Restart expose to apply the change:

```sh
sudo systemctl restart expose
curl -fsS https://control.example.com/healthz
```

Expect Control's `ok`. `/_expose/healthz` tests expose itself, not the gateway.

The route reserves `control.example.com` and forwards to `127.0.0.1:7330` on the expose server. Both services must share that loopback network; separate containers do not. Existing tunnels or publications at this hostname become inaccessible while the bypass is configured.

WAF stays enabled by default, and Control's bearer credentials reach the gateway unchanged. No expose login is needed for this route. See [server-local services](https://github.com/koltyakov/expose/blob/main/docs/server-configuration.md#server-local-services) for route options.

## 4. Provision and verify a fleet

On your administrator machine, install the [orchestrator CLI](installation.md#install-the-orchestrator-cli-and-ai-integrations), then run:

```sh
control login --gateway https://control.example.com
control users create alice
control update status
```

Enter the gateway superuser key at the hidden login prompt. Give Alice only her new account key. Alice logs in at the same URL and follows [worker installation](installation.md#install-workers-on-each-platform). Use `control login --gateway https://control.example.com --key-stdin` to replace a saved operator login with an account key without putting it in shell history.

Worker invitations need a selected release bundle. If none is available, the operator can run `control update check`, or publish a development bundle with `make update`.

The same HTTPS route serves the [web console](console.md). With a saved operator login, set its separate browser password using `control console password`, then sign in as `@operator`. Use `control console password alice` to provision Alice's browser login. Keep the gateway on loopback and preserve the public Host header. A non-loopback upstream proxy must use TLS to the gateway or a trusted local forward; forwarded scheme headers alone do not enable browser login.

After enrolling a worker, verify from the account-authenticated orchestrator. Replace `worker` with its registered name:

```sh
control machines
control call worker node.describe
control exec worker -- hostname
```

The health probe checks HTTP routing; these commands also exercise authentication, client sessions, and worker connectivity.

## Gateway on another host

Use an expose tunnel instead of a bypass when the gateway runs on a different machine, including behind NAT. On that machine, install expose and run:

```sh
expose login --server example.com
expose http 7330 --domain=control
```

Enter an expose API key at its prompt. Keep Control on `127.0.0.1:7330` and set `CONTROL_PUBLIC_URL=https://control.example.com` as above. Remove any server-local bypass reserving `control` first. Use a named subdomain, not a temporary URL, because nodes and invitations retain the gateway address.

The tunnel stays in the foreground. For unattended operation, run it under a service manager using the same OS user and saved expose login, or private service credentials. Both the gateway and tunnel must remain running. Do not add `--protect`, or inherit `EXPOSE_USER`/`EXPOSE_PASSWORD`: Control clients and installers do not perform expose's form login, and Basic authentication conflicts with Control's bearer header.

Use expose server and client builds with uncapped streamed HTTP bodies and matching service namespaces. Stock expose v0.28.2 has a 10 MiB tunnel-body cap, too small for Control binaries. `EXPOSE_PUBLISH_MAX_BYTES` controls static-site publishing, not tunnel uploads. See [client configuration](https://github.com/koltyakov/expose/blob/main/docs/client-configuration.md).

## Limits and troubleshooting

- Allow streamed uploads and downloads up to Control's 128 MiB binary limit through every proxy. Do not buffer whole bodies. A successful health probe does not verify update uploads; see [development updates](updates.md#push-from-a-development-machine).
- Keep WebSocket upgrades and long-lived connections enabled. Each node or client gateway WebSocket occupies one expose in-flight slot. Updated expose bypass routes separate bounded HTTP and upgrade admission beneath an aggregate limit. Defaults allow 128 total requests, with at most 96 pending/active upgrades, leaving 32 slots that upgrades cannot consume. Route JSON supports `max_in_flight`, `max_http_requests`, and `max_upgrades`; zero inherits defaults. An explicit example is `{"subdomain":"control","port":7330,"max_in_flight":128,"max_http_requests":32,"max_upgrades":96}`. Ordinary HTTP can otherwise use the full aggregate budget. Older expose builds share all 128 slots with WebSockets and do not recognize these new fields. Upgrade expose before adding them. Fleets above 96 persistent connections need a deliberately sized override, not an assumed hardware-capacity increase. Tunneled deployments retain their independent expose-client `EXPOSE_MAX_CONCURRENT_FORWARDS` limit, default 128.
- Size Control's [connection, handshake and aggregate packet budgets](protocol.md#gateway-resource-admission) separately. Expose cannot infer Control accounts from bearer headers and does not provide account fairness. Private expose metrics report bypass occupancy, limits and rejections; Control's superuser metrics report authenticated admission and packet retention. Neither set of limits is a measured user-capacity guarantee.
- Keep WAF enabled unless a confirmed rule blocks legitimate Control traffic. Inspect expose's logs before changing policy. A same-host route can opt out with `"disable_waf":true` without disabling WAF for other applications.
- Expose's HTTP/3 transport is not Control's WebRTC transport. Direct peer traffic does not pass through the HTTP route. STUN/TURN, if configured, needs separately reachable listeners and relay ports.

| Symptom | Check |
| --- | --- |
| Public health probe fails, local probe works | DNS, certificate, expose port mappings, and whether the route configuration loaded |
| `502` or upstream unavailable | Gateway service status and `127.0.0.1:7330` reachability from expose |
| `404` on `/v1/*` | Older expose service namespace conflicting with Control, or a wrong hostname |
| `403` or an HTML login page | WAF logs or unintended expose password protection |
| `413` during `make update` | Old tunnel-body caps or another proxy's upload limit |
| `503` as the fleet grows | Expose's in-flight and client forward limits, including persistent WebSockets |

The expose operator terminates HTTPS and can see gateway API traffic, including bearer keys and enrollment URLs. Trust that operator alongside the Control gateway operator. Peer TLS still protects execution payloads carried by the gateway relay. Keep both services' credentials and state outside worker-accessible storage.
