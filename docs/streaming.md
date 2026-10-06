# Fleet streaming and peer sessions

[Architecture](architecture.md) · [Protocol](protocol.md) · [Testing](testing.md) · [Decisions](decisions.md#d031-isolated-traffic-lanes-shared-webrtc-carriers-and-duplex-streams)

## Scenarios and the gaps they exposed

| Fleet workflow | Previous limitation | Implemented behavior |
| --- | --- | --- |
| Download a worker's large output while cancelling another task | Bulk data and control used one outbound multiplexer | Separate control, bulk, and interactive lanes; blocked bulk streams do not consume control stream slots |
| Contact several workers while one is unreachable | A global dial lock serialized every destination | Coalesced setup per peer and lane, with independently cancellable waiters |
| Run a database forward and collect artifacts over WebRTC | Independent lanes initially required repeated ICE handshakes | Compatible peers share one ICE/DTLS/SCTP carrier with independent ordered data channels and TLS/yamux sessions |
| Send a request to a service that waits for EOF before responding | Forwarding closed the response direction too | Negotiated EOF records preserve the opposite direction across peer streams and the local WebSocket API |
| Follow several long-running jobs | Each follower queried logs four times a second | One offset-based stream per follower, awakened by log appends and completion |
| Fetch files continuously from many workers | Receive packets allocated a new byte slice for every message | Sized buffer pools with copying, bounded queues, and explicit release on consumption and shutdown |
| Diagnose connection pressure | Connection inspection reported only a transport per peer | Session inspection includes lanes and active incoming/outgoing stream counts |

The orchestrator remains a client. These features do not enroll it, grant execution authority, or add it to machine dashboards and updates. Work, authorization, artifacts, and task ownership remain on the destination nodes.

## Connection layout

Each outgoing peer has lazily created lanes:

- `control` carries ordinary RPCs, task submission, status, and cancellation.
- `bulk` carries streamed artifacts. Its caller-side receive window is 1 MiB instead of the default 256 KiB, allowing more bytes in flight on higher-latency links.
- `interactive` carries TCP tunnels and followed task logs.

Each lane owns a TLS/yamux session. Upgraded peers negotiate `peerChannels` through signed gateway metadata and carry these sessions on separate reliable ordered WebRTC data channels within one carrier. Additional channels use `control-v1/<random-session-id>` labels and authenticate again with the same pinned peer identity. A new data channel does not grant a new identity or skip authorization. Extra, unreliable, or malformed channels are rejected or closed before serving application work.

With older peers, lanes use separate WebRTC connections. Relay lanes use separate TLS/yamux sessions over the existing gateway WebSocket. They still share that socket and its network capacity. Traffic lanes are isolation, not bandwidth reservations or strict scheduling priority. A failed WebRTC carrier ends all its channels; an individual channel or multiplexer can close without killing its siblings.

Existing sessions are reused. Name and ID lookups coalesce onto the same established lane. Stable peer-ID resolution uses the account-scoped identity endpoint instead of downloading the whole fleet directory. Live direct sessions remain usable during a gateway outage. Discovering a new target or establishing a new lane still requires gateway membership lookup. A connection failure never replays a submitted command, an artifact stream, or a TCP socket.

## Limits and cleanup

- At most 256 links and 32 simultaneous TLS/ICE setup operations per process.
- Pending outbound lookup/setup plans reserve separate capacity for 32 control, 16 bulk, and 16 interactive destinations. Alias lookups coalesce without consuming another TLS/ICE slot. Dependent bulk/interactive setup does not hold a TLS/ICE slot while waiting for a control carrier.
- At most 128 incoming or outgoing streams per multiplexer, with a 128-stream accept backlog.
- At most 512 active incoming handlers per process. Excess streams close without invoking a capability.
- Outgoing stream reservations are separate: 256 control, 64 bulk, and 192 interactive streams. Waiting for capacity observes caller cancellation.
- Each packet queue retains at most 256 messages of at most 16 KiB. Overflow closes that link rather than silently dropping reliable bytes.
- Receive buffers use 1 KiB, 4 KiB, and 16 KiB pools. Borrowed buffers are copied from callback data, returned after consumption, and drained on close. Pools are reclaimable by Go's garbage collector.
- TCP duplex records carry at most 32 KiB. A zero-length record is write-side EOF, not connection shutdown.
- Log records carry at most 64 KiB of bytes with the next offset and terminal status. Retained task logs remain limited to 10 MiB. Slow readers apply backpressure without accumulating per-subscriber log queues.
- The shared gateway writer has a 64-packet queue and a ten-second write timeout. Once a WebSocket write starts, its context belongs to the peer, not the application stream. Stream cancellation stops waiting but cannot tear down the shared socket. Queued packets retain their original socket and are never replayed after reconnect.

Setup cancellation does not cancel another waiter or close an established sibling lane. When the last setup waiter leaves, setup is cancelled. Peer shutdown closes channels, multiplexers, and streams and waits for owned setup and handler goroutines. Forwarding waits for both copy directions, uses reusable copy buffers, and aborts both sides on errors or cancellation. A graceful EOF preserves the response direction only when the destination supports it. Older raw peers and WebSocket clients retain full-close semantics.

## Follow logs and forward services

```sh
control exec worker --detach --id render-001 --timeout 4h -- COMMAND ARG...
control task logs worker render-001 --follow --offset 0
control task cancel worker render-001
control tunnel worker db.internal:5432 --listen 127.0.0.1:15432
control session
```

Accepted work survives log-follower or forward shutdown. Streaming logs preserve bytes and offset boundaries, including non-UTF-8 output. Disconnects report an error; reconnect explicitly from the last consumed offset. MCP's `control_task_logs` remains a bounded snapshot tool, while `control_forward_start`, `control_forward_list`, and `control_forward_stop` manage process-owned listeners.

## Verification and measurements

Tests cover both WebRTC and relay, concurrent lane creation, one shared WebRTC carrier, control traffic during bulk backpressure, sibling multiplexer closure, independent slow-peer setup, cancellation while waiting for stream capacity, malformed duplex records, legacy fallback, EOF-driven TCP responses, and log streaming with binary data, offsets, cancellation, and owner isolation. The full Compose suite also runs these core tests and the cross-container workflows.

Run adapter microbenchmarks with:

```sh
go test ./internal/transport -run '^$' -bench 'Benchmark(PacketConn|Duplex)' -benchmem -count=3
```

On a local macOS arm64 Apple M4 Pro, the 16 KiB packet adapter benchmark allocated 16,480 bytes per operation before receive-buffer pooling and 96 bytes afterward. The three local runs changed from 1.82–2.40 microseconds to 0.51–0.52 microseconds per operation. The 1 KiB case changed from 1,120 to 96 allocated bytes. These are in-process adapter measurements, not network throughput guarantees. Real throughput also depends on RTT, ICE route, SCTP congestion control, gateway capacity, disk speed, and encryption.

## Remaining work

These gaps are not implemented:

| Capability | Useful scenario | Constraint before implementing |
| --- | --- | --- |
| Interactive process stdin and PTYs | Drive a persistent remote shell or REPL | Explicit process ownership, input framing, resize, reconnect, and OS-specific terminal lifecycle |
| Reverse forwarding and UDP | Reach a local orchestrator service from a worker, or tunnel a UDP protocol | Listener authorization, resource limits, datagram semantics, and lifetime rules |
| Parallel workflow steps and load-aware placement | Distribute independent rendering or test shards | Admission must remain destination-owned; resource samples are not reservations |
| Stream scheduling on the relay | Protect control latency when a gateway link is saturated | A bounded, fair scheduler across users and sessions, not extra unbounded queues |
| Transfer quotas and completed-task pruning | Keep a large, long-lived fleet within disk budgets | Retention policy must protect active inputs, output references, and owner-scoped recovery |

TCP reconnection cannot transparently recover a database transaction or an arbitrary process session. Any resumable application protocol needs its own explicit recovery contract.
