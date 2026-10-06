# Users and private fleets

[README](../README.md) · [Installation](installation.md) · [Architecture](architecture.md) · [Protocol](protocol.md)

One gateway can serve multiple users. Each user owns one private fleet with any number of nodes. Account-authenticated clients address workers by name, subject to access rules. Workers collaborate only under instruction-bound orchestrator delegation; fleet membership alone permits discovery. A client does not enroll its machine. Another user's credentials cannot discover or connect to those machines. See [terminology](terminology.md) and [delegation](delegation.md).

## Register a user

The gateway operator provisions accounts with the superuser key:

```sh
export CONTROL_GATEWAY=https://control.example.com
export CONTROL_SUPERUSER_KEY=YOUR_OPERATOR_KEY
control users create alice
control users create bob
```

Each command returns a user record and a random account key, shown once. Give each user only their own account key. Account registration is operator-provisioned; there is no public signup or password-login endpoint.

On each of Alice's host machines:

```sh
control login --gateway https://control.example.com
control setup --name alice-laptop --client opencode
```

Enter Alice's account key at the login prompt, or supply it through `CONTROL_USER_KEY`. Login persists it for future commands; setup reuses it and issues a common key for that host's node. Use a different machine name for her second host. Both hosts join Alice's fleet and can manage its installations:

```sh
control machines add render --platform windows
control machines
control dashboard
```

Bob follows the same flow with his account key. He can also name a machine `render`; names are unique within a fleet. He sees only his own registrations, invitations, keys, and activity. Knowing Alice's machine ID does not grant access.

## Credential roles

| Credential | Scope |
| --- | --- |
| User account key | Manage that user's invitations, common keys, and registrations; enroll and discover machines in that fleet |
| Common key | Enroll and discover machines in its owning fleet; no account, fleet-management, or update administration |
| Redeemed installation key | Common permissions, with registration restricted to its bound identity, name, OS, and architecture |
| Gateway superuser | Provision/disable accounts and publish gateway-wide software updates; fleet commands operate only on the operator's legacy fleet |

Users issue, list, and revoke their own common keys with `control keys create|list|revoke`. They cannot issue account keys for other users, change credential ownership, publish executable updates, or inspect gateway-wide rollout status. Fleet-management commands appear in user-account help. Gateway administration appears only for the superuser. Common-key help exposes neither.

Only account-authenticated client sessions initiate execution authority. Enrolling a node with an account key does not grant its node identity that authority. Keep account keys off execution workers, since someone who steals one can authenticate a new orchestrator client. Worker installation/common keys do not independently execute peer commands, including through the local API or a new common-key client session.

The gateway operator remains trusted. The operator owns the database and release-distribution authority and can inspect gateway metadata. Fleet isolation protects users from other users; it does not turn the gateway into an untrusted service or sandbox providers running on a machine.

## Enforcement

- Authentication resolves a credential to an immutable user ID. The gateway assigns node ownership from that authenticated identity, never from a client label or supplied user ID.
- Directory responses and offline notifications contain only the caller's fleet. Invitation/key lists and mutations are scoped by user. Requests for another user's registration, key, or invitation return the same not-found response as an unknown ID.
- Every signaling and relay packet checks the authenticated sender's fleet against the destination's fleet. The gateway overwrites the packet's sender identity. Raw packets cannot bypass directory filtering or forward a WebRTC offer to another fleet.
- Nodes obtain their fleet ID from `/v1/auth` and verify directory membership before accepting peer setup. Both WebRTC and relay sessions then require pinned mutual TLS.
- Node dispatch checks the attested membership before capability access rules and before artifact-grant handling. Wildcard access rules and signed artifact grants cannot create a cross-fleet connection.
- Identity ownership stays recorded after a registration is forgotten. The same key cannot move to another user, so cached direct sessions cannot become cross-user sessions after re-enrollment.

Task ownership remains narrower than fleet membership. One orchestrator can use a worker without gaining permission to cancel another orchestrator's tasks. Node-wide monitoring remains read-only and scoped to the fleet.

Revoking a key or disabling a user rejects subsequent gateway authentication and disconnects affected gateway sessions. It does not terminate already accepted work or an established direct session inside that same fleet. Existing direct sessions can survive a gateway outage because their authenticated identities cannot change fleet ownership.

## Persistence and upgrades

The gateway stores accounts, credential hashes, node registrations, permanent identity ownership, and invitations in `gateway.db`. SQLite uses WAL, full synchronous commits, foreign keys, and a unique machine-name constraint per user. Its driver is pure Go; all six supported platforms retain CGO-free builds. The gateway commits changes before acknowledging them and holds an exclusive state-directory lock.

Authenticated CLI/MCP client sessions belong to an account but are not fleet machines. Concurrent processes have independent transport identities and a shared persistent task-owner key. The gateway verifies both signatures and persists immutable account ownership, client role, and transport-to-owner bindings. Only live routing metadata is kept in memory, for same-account peer authentication. Machine discovery, status, enrollment management, and managed updates exclude clients. Revoking their credential or disabling their account disconnects them like other authenticated connections. See [standalone CLI and MCP](installation.md#standalone-cli-and-mcp).

Derived client identity names remain reserved within their account after disconnecting. A machine or installation invitation cannot claim that name and impersonate a client trusted by a node's name-based access rule. Prefer full identity IDs for access rules.

The first startup imports existing `nodes.json`, `keys.json`, and `installations.json` into a single `legacy` fleet in one transaction. Old records cannot identify separate users, so migration never guesses ownership. Legacy files are retained but ignored after the database migration commits. Failed migration leaves them intact and can be retried.

Schema version 2 adds `machine_states` for disabled policies and unregistration tombstones, referencing the existing permanent identities. Upgrading schema version 1 creates this table transactionally. Older gateways reject the newer schema; do not point them at the upgraded database. Node policy changes and directory removal commit together, and disconnect cleanup never recreates a removed node.

Schema version 3 adds `client_identities`, referencing permanent identities without storing machine registrations or session metadata. Versions 1 and 2 upgrade transactionally. On startup, the gateway migrates only the previous standalone implementation's exact capability-free, unmanaged, unbound `cli-<identity-prefix>` entries out of `nodes`, preserving their account ownership and task-owner identity. Capability-bearing machines, invitation-bound identities, lifecycle-managed machines, and retired identities are not converted. Client roles survive disconnects and gateway restarts and cannot be reused for machine enrollment. Back up the gateway state before upgrading; earlier executables reject schema version 3.

Schema version 4 adds `client_transports` with immutable references to permanent transport and owner identities. Versions 1 through 3 upgrade through transactional schema migrations. The existing persistent client key remains the task owner, so previous tasks and leases stay recoverable. Each process's transport binding survives disconnects and restarts to prevent role changes or owner reassignment, including for cached direct sessions. This ledger stores no routing metadata and is retained without automatic pruning. Back up the gateway state before upgrading; older gateways reject schema version 4.

`CONTROL_TOKEN` on the gateway is an optional bootstrap common key for the legacy fleet only. For a new shared gateway, omit it and configure `CONTROL_SUPERUSER_KEY`, then create separate accounts. Do not give unrelated users the same account key or legacy bootstrap token.

**Upgrade the gateway before upgrading nodes from the pre-isolation version.** New nodes require fleet-aware authentication and refuse an older gateway. Pause automated release polling during this one-time gateway-first upgrade. Subsequent fleet-aware updates use the usual nodes-first managed rollout. SQLite migration cannot be reversed by starting the old executable against stale JSON files.

To split an old shared pool into new accounts, enroll fresh node identities under those accounts in separate state directories. Existing identities and their task ownership remain in the legacy fleet. For backups, stop the gateway and copy its state directory, including the database and release storage. Node tasks, artifacts, and workspaces remain on their respective machines; executable blobs and rollout state retain their existing filesystem storage.

## API reference

| Endpoint | Permission | Result |
| --- | --- | --- |
| `POST /v1/admin/users` | Superuser | JSON `name`; returns `user` and one-time `token` |
| `GET /v1/admin/users` | Superuser | Registered users, without credentials |
| `DELETE /v1/admin/users/{id}` | Superuser | Disable that account and its credentials |
| `GET /v1/auth` | Authenticated | `role`, `userId`, and management capabilities |
| `GET /v1/nodes` | Authenticated | Only the credential owner's fleet |
| `GET /v1/client/connect` | Authenticated, unbound credential | Role-proven client session, without machine enrollment |
| `GET /v1/peers/{id}` | Authenticated | Same-account machine identity or a connected client's transient identity; other IDs return 404 |
| `GET /v1/status` | Authenticated | Gateway version, URL, resources, and own directory; account keys also receive fresh aggregate health for their fleet |
| `POST /v1/fleet/keys` | Fleet owner | JSON `name`; returns common key metadata and one-time `token` |
| `GET /v1/fleet/keys` | Fleet owner | Own credential metadata |
| `DELETE /v1/fleet/keys/{id}` | Fleet owner | Revoke own credential |
| `/v1/fleet/installations` | Fleet owner | Scoped installation management, detailed in [installation](installation.md) |
| `DELETE /v1/fleet/nodes/{id}` | Fleet owner | Forget own offline registration |
| `PATCH /v1/fleet/nodes/{id}` | Fleet owner | Enable/disable with JSON `disabled`, or rename with JSON `name` |
| `DELETE /v1/fleet/nodes/{id}?stop=true` | Fleet owner | Unregister own machine regardless of online state or lifecycle support |

The older `/v1/admin/keys`, `/v1/admin/installations`, and `/v1/admin/nodes/{id}` aliases remain superuser-only and operate on the legacy fleet. They do not provide a cross-user route. Account IDs cannot be reassigned or deleted; `users revoke` disables the account. The legacy operator fleet cannot be disabled through this endpoint.
