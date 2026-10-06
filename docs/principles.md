# Engineering principles

[Contributor guide](../AGENTS.md) · [Architecture](architecture.md) · [Design decisions](decisions.md) · [Protocol reference](protocol.md)

These principles guide changes to Control. The [decision record](decisions.md) documents how the current implementation applies them.

## Every machine is a peer

A node can receive work, serve artifacts, and consume another node's capabilities under orchestrator-issued delegation. Orchestrator and worker are roles within an operation, not fixed machine types. An account-authenticated client initiates execution authority and can delegate coordination to a node. Follow the [terminology](terminology.md): client, node, and gateway name components; agent refers to AI software. Avoid introducing assumptions that all requests originate from one laptop or that only workers can produce files.

An orchestrator's CLI or MCP process is an authenticated client, not an enrolled execution machine. Keep client transport identities and task ownership separate from the fleet machine catalog, resource observation, lifecycle controls, and managed binary rollout. Remote client execution must not require enrolling the requesting machine as a worker.

Windows, Linux, and macOS are equal targets. Power BI, FFmpeg, databases, and AI CLIs are examples of capabilities, not categories built into the connection protocol.

## Execution and connectivity stay separate

Providers operate on the destination machine and use its installed software, filesystem, credentials, and network. They should not know whether the caller reached them through WebRTC or a relay.

The gateway handles enrollment, discovery, signaling, relaying, and administrative software distribution. Scheduling, AI reasoning, and application execution belong outside it. New integrations should fit the provider contract without changing the gateway.

## Direct and relayed operations have the same meaning

Use outbound connections so private-network nodes remain reachable without inbound SSH. Prefer direct peer sessions when they can be established and retain a relay path for restricted networks.

Transport choice must not change identity verification, authorization, task ownership, or the interpretation of an operation. A connection failure can interrupt a stream; it must not silently start the same command again.

## Accepted work has an identity and a lifecycle

Persist a task before acknowledging acceptance. Keep its ID, owner, specification, state, logs, and result available for reconciliation after a caller disconnects.

Idempotent submission prevents duplicate task creation. It does not guarantee exactly-once effects in arbitrary programs or external APIs. Interrupted work needs an explicit recovery decision. Durable tasks and synchronous calls have different cancellation lifetimes; preserve that distinction.

## Move data between its producers and consumers

Use structured control messages for arguments, status, and artifact references. Stream large files directly between participating peers, with relay fallback when needed.

Keep artifacts immutable and content-addressed. Resume partial transfers by offset and verify the complete content before publishing a received artifact. The orchestrator should not need to download and re-upload another node's output.

## Authority is explicit at the destination

Each user owns a separate fleet. Derive ownership from authenticated credentials, scope all gateway discovery and routing to that fleet, and verify membership at peer acceptance and dispatch. Client-supplied labels, names, access rules, artifact grants, and cached connections must never override the user boundary. Identity ownership is immutable, including after a registration is forgotten.

Authenticate peers and enforce access on the node that owns the requested capability or artifact. Preserve owner-scoped task access and recipient-bound artifact grants.

Fleet membership and a worker's local API credential permit discovery, not independent peer execution. Bind delegation to the orchestrator owner, destination, authenticated worker, and requested instruction. Do not install ambient worker permissions. Check expiry and revocation on existing sessions as well as new connections; cancellation must cover admitted tasks and streams. See [delegation](delegation.md).

Only the gateway's superuser role can publish executable updates or provision user accounts. A user can issue common keys only within their own fleet. A node must acknowledge an idle maintenance reservation before updating. Never interrupt accepted work to apply an update, and never equate a disconnected participant with a successful restart.

Document the authority under which providers run. A filesystem root limits filesystem API paths; it does not sandbox an executable. An AI CLI is a provider running as the node's OS user, with the authority available to that process.

## Resource ownership includes cleanup

Bound queues, message sizes, retained output, and execution concurrency. Use cancellation and backpressure rather than accumulating work indefinitely in memory.

The component that creates a connection, goroutine, subprocess, or temporary file must define how it terminates. Node shutdown should wait for owned work to stop before releasing its state directory. Lease expiry must not allow a new owner to overlap a still-running leased task.

## Keep changes testable across operating systems

Use structured arguments instead of shell command strings. Isolate OS-specific behavior and keep the core build independent of CGO.

Test behavior across real local peer connections, including failure and recovery paths. Use installed application workflows as smoke tests without making the core test suite depend on those applications.

## Document the implementation precisely

Keep schemas, examples, defaults, limits, and documentation consistent. Record architectural tradeoffs when they change. Describe incomplete features as incomplete, and preserve enough context for a later contributor to understand why a simpler implementation was chosen.
