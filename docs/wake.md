# Wake-on-LAN

[README](../README.md) · [Protocol](protocol.md#wake-on-lan) · [Installation](installation.md) · [Testing](testing.md)

Control can send Wake-on-LAN magic packets to an offline registered machine from an online node on its LAN. The orchestrator can be elsewhere. It can also send from the local client when you explicitly identify its LAN.

## Identify the LAN

Set the same unique `lan` label in the target and sender node configurations, then restart those nodes while the target is still awake:

```json
{
  "labels": { "lan": "home-office" }
}
```

Merge this into each existing configuration, preserving other settings and labels. Give different broadcast domains different identifiers, even when they use identical private IP ranges. A matching subnet alone does not prove LAN membership. The label is an operator assertion, not network discovery or execution authority.

Opted-in nodes advertise up to 32 private IPv4 interface addresses with prefix lengths and six-byte unicast MAC addresses in signed registration metadata. The gateway retains `wakeInterfaces` in the fleet directory, including while offline and across gateway restarts. Collection excludes loopback, point-to-point, non-broadcast and non-Ethernet adapters. A reported adapter does not prove wake support. Metadata refreshes on registration and reconnection, including after detected network changes. It can become stale while a node is offline.

Upgrade the gateway, target, and sender before relying on retained wake metadata. Nodes omit it for gateways without signed-registration-byte support; older gateways that accept those bytes but do not recognize `wakeInterfaces` discard the field. No database schema migration is needed. Removing the `lan` label and reconnecting clears the node's advertised interfaces.

## Send a wake request

```sh
control wake workstation
control wake workstation --from always-on
control wake workstation --from local --lan home-office
control wake workstation --json
```

The default selects the first compatible online sender in the fleet's alphabetical directory order. `--from` accepts a name or stable ID. The reserved value `local` selects the invoking client instead, so use a stable ID for a worker named `local`. Local sending requires `--lan` to explicitly assert that this client is on the target's known LAN. It needs no enrolled local node. Control never falls back to another sender or retries a failed request.

MCP `control_wake` accepts `node`, optional `from`, and `lan` only when `from` is `local`. Direct worker calls use:

```sh
control call always-on network.wake '{"target":"workstation"}'
```

Account-client routing is required for the shared CLI/MCP wake operation. A worker sender requires `network.wake` permission under its normal execution authorization, admission, lease, and maintenance rules. Enrolled peers and common-key clients need exact instruction-bound delegation to invoke it remotely. The trusted local node API retains its existing local capability authority. The sender resolves its target in its own fleet directory, never from caller-supplied MAC or broadcast addresses. Automatic selection excludes disabled and policy-pending senders. Waking a disabled target does not enable it.

The sender checks its live interfaces against the target's retained interfaces. Both the LAN label and masked IPv4 prefix must match exactly. It sends one 102-byte magic packet to UDP port 9 on the subnet's directed broadcast address per distinct target MAC/subnet, binding the socket to the matching local address. Prefix lengths must be /8 through /30 and the entire subnet must be private. There is no internet broadcast, routed wake proxy, IPv6 wake, SecureOn password, or repeated packet burst.

Results contain `target`, `source`, and `packets`. Success means UDP accepted the packet for transmission, not that the NIC received it or the machine woke up. Check `control machines` afterward. A failure may follow partial transmission, so do not replay automatically. The gateway never sends magic packets or runs a wake scheduler, and normal execution never wakes a machine implicitly.

## Hardware and OS requirements

Enable Wake-on-LAN in the target's firmware and NIC/OS settings yourself. Control does not change power, firewall, router, or adapter settings. Wired Ethernet is the usual supported path. Wi-Fi wake, sleeping versus powered-off states, Windows Fast Startup, macOS hardware support, and service startup after wake depend on the machine. User-login nodes may remain offline until someone logs in even after the machine powers on.

The sender must stay awake and able to broadcast on that LAN. A VPN, matching private subnet, or shared public IP does not guarantee this. Automated tests verify packet bytes, validation, selection, persistence, authorization, and no-replay behavior without broadcasting onto the host LAN. Actual wake-up and broadcast delivery require hardware smoke tests on Windows, Linux, and macOS.
