# Wake-on-LAN

[README](../README.md) · [Protocol](protocol.md#wake-on-lan) · [Installation](installation.md) · [Testing](testing.md)

Control automatically learns which fleet machines can broadcast to one another on a LAN. It retains those relationships and interface addresses so an awake machine can later wake an offline peer. No LAN labels, MAC entry, or router configuration is needed in Control.

## Automatic interface reporting and LAN discovery

Nodes report up to 32 eligible private IPv4 addresses with prefix lengths and six-byte unicast MAC addresses in signed registration metadata. Collection excludes loopback, point-to-point, non-broadcast and non-Ethernet adapters. It runs on every registration, independently of labels. The network monitor handles address/route changes, and discovery checks interface metadata every ten seconds, reconnecting to publish changes. Failed samples do not erase the last successful registration.

Matching private IP ranges only identify candidates. They do not establish a relationship. For each candidate, the receiving node opens a temporary UDP listener on an ephemeral port and sends a random challenge through authenticated, fleet-scoped gateway signaling. The other node sends its signed proof only as a UDP directed broadcast from the matching local interface. The receiver verifies the challenge, signature, source address, interfaces, and freshness before reporting the proof over its authenticated gateway connection.

The gateway verifies the other node's signature, both registered interfaces, current fleet membership, and connection presence before committing the relationship to SQLite. Registration cannot supply trusted relationships. `lanPeers` on a target records each verified sender ID, target `local` interface, sender `remote` interface, and `verifiedAt`. These are directional observations. Each node learns the reverse direction independently. The gateway never emits discovery or wake broadcasts itself.

Relationships survive disconnects, gateway restarts, and ordinary node reconnects. Each relationship is tied to exact interface metadata, refreshed after fifteen minutes while both machines are online, and expires after seven days without verification. A changed target interface removes its old relationships at registration; a changed sender interface fails the wake-time match. Evidence describes the last observed LAN, not a guarantee that an offline machine has not moved. Reusing the same addresses and MACs on another network can leave stale evidence until discovery refreshes or expires it.

Discovery is connectivity observation, not execution. It runs without an orchestrator or an execution grant and works even with `relayOnly: true`, because the small challenge uses the gateway while its proof travels on the LAN. It does not run commands, open a fixed listening port, change firewall rules, or bypass execution authorization. A firewall blocking the temporary UDP listener or broadcast will prevent a relationship from being established. Existing Windows Control executable UDP rules cover discovery.

Discovery reads its own fleet directory every ten seconds, considers at most 128 candidate interface pairs per inventory, and runs one initiating probe at a time. Probes last at most three seconds and failed pairs wait two minutes before another attempt. Receivers allow four concurrent responses and at most one request per peer every five seconds, with bounded rate-tracking maps. Proof datagrams are at most 2 KiB. Each target retains at most 128 relationships. No machine scans arbitrary IP ranges or sends discovery traffic outside its registered candidate subnets.

## Send a wake request

```sh
control wake workstation
control wake workstation --from always-on
control wake workstation --from local
control wake workstation --json
```

The default first checks whether this host has a registered NIC with a verified relationship to the target. Otherwise it selects the first compatible online worker in the fleet's alphabetical directory order. `--from` accepts a name or stable ID; the reserved value `local` selects this host, so use a stable ID for a worker named `local`. Local sending still requires a previously verified relationship and a matching live NIC. It never assumes LAN membership from a shared subnet. Control never switches sender or retries after an uncertain wake result.

MCP `control_wake` accepts `node` and optional `from`. Direct worker calls use:

```sh
control call always-on network.wake '{"target":"workstation"}'
```

Account-client routing is required for the shared CLI/MCP operation. A worker sender requires `network.wake` permission under normal execution authorization, admission, lease, and maintenance rules. Enrolled peers and common-key clients need exact instruction-bound delegation to invoke it remotely. The trusted local node API retains its existing local capability authority. The sender resolves its offline target in its own fleet directory, never from caller-supplied MAC or broadcast addresses. Automatic selection excludes disabled and policy-pending senders. Waking a disabled target does not enable it.

The sender rechecks its live interfaces against the verified relationship and the target's retained metadata. Both endpoints must still match an exact private IPv4 prefix. It sends one 102-byte magic packet to UDP port 9 on the directed broadcast address per distinct target MAC/subnet, binding the socket to the matching local address. Prefix lengths must be /8 through /30 and the entire subnet must be private. Internet broadcast, IPv6 wake, SecureOn passwords, and repeated packet bursts are unsupported.

Results contain `target`, `source`, and `packets`. Success means UDP accepted the packet for transmission, not that the NIC received it or the machine woke up. Check `control machines` afterward. A failure may follow partial transmission, so do not replay automatically. Normal execution never wakes a machine implicitly.

## Upgrade and hardware requirements

Upgrade the gateway and nodes. Authenticated gateway metadata advertises `lanDiscovery`, which compatible nodes include in signed registration before automatically reporting interfaces or using the new signaling messages. During nodes-first managed rollouts, new nodes omit automatic interface metadata until the gateway advertises support. This preserves compatibility with the first Wake-on-LAN gateway, which rejects unlabeled interface records. Explicit legacy `labels.lan` profiles can still report to that gateway, but cannot establish automatic relationships. Older gateways receive no unsupported messages. No database schema migration is needed.

Both machines must be online together at least once to learn their relationship, normally within tens of seconds for a small fleet. This cannot reconstruct a sleeping machine's MAC address or LAN membership if it never reported them. Bring an already-offline older node online manually once. The previous `labels.lan` requirement and `--lan` argument have been removed; labels are no longer LAN evidence.

Enable Wake-on-LAN in the target's firmware and NIC/OS settings yourself. Control does not change power, firewall, router, or adapter settings. Wired Ethernet is the usual supported path. Wi-Fi wake, sleeping versus powered-off states, Windows Fast Startup, macOS hardware support, and service startup after wake depend on the machine. User-login nodes may remain offline until someone logs in even after the machine powers on.

Tests verify packet bytes, proof signatures, candidate selection, persistence, authorization, and no-replay behavior using synthetic interfaces and loopback sockets. Compose also verifies actual broadcast discovery between three isolated containers over both WebRTC and relay-only Control connections, without touching the host LAN. Actual hardware wake-up still requires native smoke tests on Windows, Linux, and macOS.
