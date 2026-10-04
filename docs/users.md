# Users and private fleets

[README](../README.md) · [Installation](installation.md) · [Architecture](architecture.md) · [Protocol](protocol.md)

One gateway can serve multiple users. Each user owns one private fleet with any number of host agents and workers. All of that user's agents can address their workers by name, subject to the workers' access rules. Workers can collaborate with other machines in the same fleet. Another user's credentials cannot discover or connect to those machines.

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
control setup --gateway https://control.example.com --name alice-laptop --client opencode
```

Enter Alice's account key at the prompt, or supply it through `CONTROL_USER_KEY`. Use a different machine name for her second host. Setup saves the account key separately and issues a common key for that host's node. Both hosts join Alice's fleet and can manage its installations:

```sh
control machines add render --platform windows/amd64 --ttl 15m
control machines
control dashboard
```

Bob follows the same flow with his account key. He can also name a machine `render`; names are unique within a fleet. He sees only his own registrations, invitations, keys, and activity. Knowing Alice's machine ID does not grant access.

## Roles

| Credential | Scope |
| --- | --- |
| User account key | Manage that user's invitations, common keys, and registrations; enroll and discover machines in that fleet |
| Common key | Enroll and discover machines in its owning fleet; no account, fleet-management, or update administration |
| Redeemed installation key | Common permissions, with registration restricted to its bound identity, name, OS, and architecture |
| Gateway superuser | Provision/disable accounts and publish gateway-wide software updates; fleet commands operate only on the operator's legacy fleet |

Users issue, list, and revoke their own common keys with `control keys create|list|revoke`. They cannot issue account keys for other users, change credential ownership, publish executable updates, or inspect gateway-wide rollout status. Fleet-management commands appear in user-account help. Gateway administration appears only for the superuser. Common-key help exposes neither.

The gateway operator remains trusted. The operator owns the database and release-distribution authority and can inspect gateway metadata. Fleet isolation protects users from other users; it does not turn the gateway into an untrusted service or sandbox providers running on a machine.

## Enforcement

- Authentication resolves a credential to an immutable user ID. The gateway assigns node ownership from that authenticated identity, never from a client label or supplied user ID.
- Directory responses and offline notifications contain only the caller's fleet. Invitation/key lists and mutations are scoped by user. Requests for another user's registration, key, or invitation return the same not-found response as an unknown ID.
- Every signaling and relay packet checks the authenticated sender's fleet against the destination's fleet. The gateway overwrites the packet's sender identity. Raw packets cannot bypass directory filtering or forward a WebRTC offer to another fleet.
- Nodes obtain their fleet ID from `/v1/auth` and verify directory membership before accepting peer setup. Both WebRTC and relay sessions then require pinned mutual TLS.
- Node dispatch checks the attested membership before capability access rules and before artifact-grant handling. Wildcard access rules and signed artifact grants cannot create a cross-fleet connection.
- Identity ownership stays recorded after a registration is forgotten. The same key cannot move to another user, so cached direct sessions cannot become cross-user sessions after re-enrollment.

Task ownership remains narrower than fleet membership. One agent can use a worker without gaining permission to cancel another agent's tasks. Node-wide monitoring remains read-only and scoped to the fleet.

Revoking a key or disabling a user rejects subsequent gateway authentication and disconnects affected gateway sessions. It does not terminate already accepted work or an established direct session inside that same fleet. Existing direct sessions can survive a gateway outage because their authenticated identities cannot change fleet ownership.

## Persistence and upgrades

The gateway stores accounts, credential hashes, node registrations, permanent identity ownership, and invitations in `gateway.db`. SQLite uses WAL, full synchronous commits, foreign keys, and a unique machine-name constraint per user. Its driver is pure Go; all six supported platforms retain CGO-free builds. The gateway commits changes before acknowledging them and holds an exclusive state-directory lock.

The first startup imports existing `nodes.json`, `keys.json`, and `installations.json` into a single `legacy` fleet in one transaction. Old records cannot identify separate users, so migration never guesses ownership. Legacy files are retained but ignored after the database migration commits. Failed migration leaves them intact and can be retried.

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
| `POST /v1/fleet/keys` | Fleet owner | JSON `name`; returns common key metadata and one-time `token` |
| `GET /v1/fleet/keys` | Fleet owner | Own credential metadata |
| `DELETE /v1/fleet/keys/{id}` | Fleet owner | Revoke own credential |
| `/v1/fleet/installations` | Fleet owner | Scoped installation management, detailed in [installation](installation.md) |
| `DELETE /v1/fleet/nodes/{id}` | Fleet owner | Forget own offline registration |

The older `/v1/admin/keys`, `/v1/admin/installations`, and `/v1/admin/nodes/{id}` aliases remain superuser-only and operate on the legacy fleet. They do not provide a cross-user route. Account IDs cannot be reassigned or deleted; `users revoke` disables the account. The legacy operator fleet cannot be disabled through this endpoint.
