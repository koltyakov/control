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

## Measurements

- Setup time includes opening the bulk stream and the destination's acknowledgement. It can reuse an existing session; this is not necessarily a cold ICE/TLS handshake.
- RTT is a warmed eight-byte echo on that stream. Results include minimum, mean, maximum, and sample count. Jitter is the mean absolute difference between successive RTT samples, or zero for one sample.
- Throughput is sequential, first upload then download. Each direction streams synthetic bytes and verifies SHA-256. Mbps uses decimal megabits per second. Upload timing includes the receiver's verification acknowledgement; download timing includes receiving and verifying its checksum.

The default is 16 MiB per direction and ten RTT samples. `--size` accepts 1..256 MiB per direction; `--samples` accepts 1..100. `--timeout` sets an overall deadline from one second to two minutes, default two minutes. JSON includes endpoint names and IDs, transport, start time, setup milliseconds, latency milliseconds, and transferred bytes, seconds, and Mbps for each direction.

This measures Control's application throughput, including encryption, flow control, hashing, host load, and any relay bottleneck. It is not an internet speed test, raw link capacity, packet-loss measurement, or simultaneous full-duplex benchmark. A small payload may not saturate a high-bandwidth, high-latency path. Increase `--size` when needed. Tests consume bandwidth and can compete with real work; they never run during dashboard polling or resource sampling.

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
