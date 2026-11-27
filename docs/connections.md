# On-demand connection tests

[README](../README.md) · [Protocol](protocol.md) · [Architecture](architecture.md) · [Testing](testing.md)

Test this orchestrator's connection to a worker:

```sh
control speedtest SER5
```

Test directly between two workers:

```sh
control speedtest SER5 --from est-vps
control speedtest SER5 --from est-vps --size 64 --samples 20 --json
```

`--from` selects the initiating worker. Without it, the initiating endpoint is the CLI/MCP process on this orchestrator, with no local node required. Upload means source to target; download means target to source. Worker-to-worker test payloads never pass through the orchestrator. Both paths use Control's normal pinned TLS and bulk-lane WebRTC connection, with gateway relay fallback. The result identifies the transport of the measured stream, rather than inferring it from other connections.

Results also identify the selected route as `direct`, `turn`, `gateway-relay`, or `unknown`. WebRTC can carry traffic through TURN, so `transport: "webrtc"` alone does not prove a direct path. JSON `route` includes local/remote candidate types and protocol when available, never addresses or credentials. CLI output prints the route separately; older nodes may omit it.

## Measurements

- Setup time includes opening the bulk stream and the destination's acknowledgement. It can reuse an existing session; this is not necessarily a cold ICE/TLS handshake.
- RTT is a warmed eight-byte echo on that stream. Results include minimum, mean, maximum, and sample count. Jitter is the mean absolute difference between successive RTT samples, or zero for one sample.
- Throughput is sequential, first upload then download. Each direction streams synthetic bytes and verifies SHA-256. Mbps uses decimal megabits per second. Upload timing includes the receiver's verification acknowledgement; download timing includes receiving and verifying its checksum.

The default is 16 MiB per direction and ten RTT samples. `--size` accepts 1..256 MiB per direction; `--samples` accepts 1..100. `--timeout` sets an overall deadline from one second to two minutes, default two minutes. JSON includes endpoint names and IDs, using the fleet's routing name for a worker source even when its installation profile has an older configured name, transport, start time, setup milliseconds, latency milliseconds, and transferred bytes, seconds, and Mbps for each direction.

This measures Control's application throughput, including encryption, flow control, hashing, host load, and any relay bottleneck. It is not an internet speed test, raw link capacity, packet-loss measurement, or simultaneous full-duplex benchmark. A small payload may not saturate a high-bandwidth, high-latency path. Increase `--size` when needed. A Linux receiver whose UDP buffers stay at the kernel default can lose bursts and hold a long path far below its TCP throughput; see [stream limits](streaming.md). Tests consume bandwidth and can compete with real work; they never run during dashboard polling or resource sampling.

## Authority and lifetime

Use an account login and default client routing. Explicit `--api` or `CONTROL_API` is rejected, rather than reporting a local node's path as this orchestrator's connection. Both selected workers must be online, enabled, and policy-acknowledged. Restricted destinations need `connection.open`; worker-to-worker sources also need `connection.test`. Both workers must support this test protocol, and the destination must support instruction-bound delegation.

The client authorizes one exact `connection.open` instruction at the destination for the selected source worker. It carries that grant only through `connection.test` and revokes it after the call, using a bounded best-effort cleanup. There is no ambient worker permission or nested delegated test coordination. A failed cleanup can leave a grant until normal expiry or restart, but a used grant cannot run a second test.

Each node permits at most two concurrent diagnostic streams or initiating tests. Tests hold normal work admission and block managed updates while active. Receiving traffic appears as a `connection.open` transfer in authorized activity snapshots; source coordination appears as a `connection.test` operation. Cancellation, disconnect, revocation, timeout, or node shutdown terminates the stream. Tests are synchronous, not durable tasks, and are never retried automatically. Synthetic payloads use bounded buffers and leave no files or artifacts.

## MCP and JSON operations

`control_speedtest` accepts required `node`, optional `from`, `bytes`, and `samples`. `bytes` is 1..268435456 per direction, default 16777216. `samples` is 1..100, default ten. The overall deadline is at most two minutes and honors earlier request cancellation.

For worker-to-worker calls, the shared client also prepares delegation for:

```sh
control call est-vps connection.test '{"target":"SER5","bytes":16777216,"samples":10}'
```

`node.describe.connectionTest` advertises protocol `connection-test-v1`, methods `connection.test` and `connection.open`, and the byte, sample, and lifetime limits. `connection.test` takes `target`, `bytes`, and `samples`; it is a JSON node operation, not a provider or task capability. `connection.open` is peer-only streaming, not a JSON call or local HTTP proxy. Its request includes the protocol plus byte/sample options. After a framed protocol acknowledgement, peers exchange one warm-up echo and the requested samples, upload bytes plus a SHA-256 trailer, a framed verification receipt, a one-byte download marker, download bytes plus a SHA-256 trailer, and framed verification receipts in both directions. Bulk bytes never enter control frames. Unsupported peers fail without replaying a test or falling back to older authorization rules.

## Diagnose a connection

`control doctor` answers "why can't I reach this machine" or "why is this slow" in one read-only report:

```sh
control doctor               # this client, the gateway, and one line per fleet machine
control doctor SER5          # adds peer-session, concurrency and diagnostics checks for one machine
control doctor SER5 --json
```

Each check reports `ok`, `warning`, `failed` or `skipped` with a reason, plus a hint naming the command or document to use next. The report is ordered:

| Check | What it reads |
| --- | --- |
| `login` | Whether a gateway URL and credential are configured, where the credential came from (`--token`, an environment variable, the saved login, or this host's node profile) and its role. The key is never printed |
| `gateway` | `/v1/auth` reachability and authentication, the gateway version from `/v1/status`, its protocol epoch, and whether it supports instruction-bound delegation |
| `version` | This client against the gateway version and the fleet's selected release from `/v1/updates`. Development builds are not compared. When this host has a node profile, also whether this binary's node protocol range includes the gateway's epoch |
| `clock` | The offset between this host and the gateway's `/v1/status` `observedAt`, measured against the request midpoint and reported with its round-trip uncertainty |
| `local-node` | Whether this host's node API answers `node.describe`. No local node is normal: the client works standalone |
| `environment` | `HTTPS_PROXY`/`HTTP_PROXY` and `NO_PROXY` as they apply to the gateway URL, an explicit profile gateway proxy, and a plain-HTTP gateway URL on a non-loopback host. Proxy credentials are never printed |
| `fleet`, `machine:NAME` | Without NODE: one line per machine from owner-visible `/v1/status` only (online, disabled, policy pending, update state, version against the gateway). No peer session is opened |
| `machine` | With NODE: registration, online state and last-seen time, disabled, `controlPending`, managed update state, an active lease, and a wake sender for keys that can run work. Re-enable hints name `control machines enable` only for keys that manage the fleet |
| `machine-version` | Worker version against the gateway, the fleet release and this client; OS/architecture; gateway protocol support; instruction-bound delegation support |
| `peer-session` | Opens this client's peer session with `node.describe` and reports each lane's route (`direct`, `turn` or `gateway relay`) from `control session` data. Relay is a warning unless the profile is relay-only |
| `concurrency` | `limit`/`inUse`/`queued` for each pool from `node.describe`. Saturation is a warning, not a fault |
| `diagnostics` | `diagnostics.get`: pending task cleanup, artifact budget pressure, MCP sessions starting, and cumulative transport counters since node start |

Exit status is 1 when any check failed. Warnings and skipped checks do not fail. `--timeout` (default 30s, at most 10m) bounds the whole run and `--check-timeout` (default 10s) bounds each check. Independent gateway and local reads run concurrently; a check that does not answer in time is reported as timed out and never holds the command.

`--json` prints a stable object:

```json
{
  "node": "SER5",
  "checks": [{"check": "clock", "status": "warning", "detail": "...", "hint": "..."}],
  "summary": {"status": "warning", "ok": 9, "warning": 1, "failed": 0, "skipped": 1}
}
```

`node` is empty without NODE. Every check always has `check`, `status`, `detail` and `hint`; `hint` is empty when there is nothing to do. `summary.status` is `failed` if any check failed, otherwise `warning` if any warned, otherwise `ok`. Machine lines use the check name `machine:NAME`.

The clock check warns above 10 seconds and fails at 30 seconds of offset beyond the measurement uncertainty. Remote secret writes and gateway secret-management requests carry 30-second expiries and are refused when they claim more than a minute, so 30 seconds breaks them outright; a third of that leaves room for transit and the worker's own offset. Delegation and artifact grants also carry absolute expiries.

### Read-only and role-scoped

Doctor starts no task, requests no grant or lease, writes no worker file, sends no wake packet, runs no connection test, and retries nothing. Its gateway requests are `GET /v1/auth`, `/v1/status`, `/v1/updates` and, with NODE, `/v1/nodes`. With NODE it also opens the account client's peer session like any remote command, which loads this client's owner identity, and sends only `node.describe` and `diagnostics.get`. Use `control speedtest NODE` to measure throughput; it moves test data and authorizes one `connection.open` grant, so doctor never runs it.

Instruction-only destinations, such as Hybrid System companions, need an exact grant for `diagnostics.get`. Doctor never requests one: it reports that check as skipped and names `control diagnostics NAME`, which authorizes that single read explicitly. Discovery and the peer route are still checked.

A common key runs what that role can: the gateway, version, clock and fleet checks, and peer discovery. `diagnostics.get` needs execution authority, so its refusal is reported as `skipped: not permitted`, not as a failure. A redeemed installation key cannot open client sessions, so the peer-session check fails with that reason. Without a credential, only the local checks run and every gateway and machine check is skipped. Hints follow [role-scoped help](updates.md#common-keys-and-hidden-administration-help): fleet-management commands appear only for account keys and rollout administration only for the superuser.

Doctor composes existing reads, so its limits are theirs. `diagnostics.get` reports how many MCP sessions are starting, not why a server failed to start. Transport counters are cumulative since the worker started, not a recent window. Owner health such as leases is visible to account keys only. There is no MCP tool; agents run the CLI.
