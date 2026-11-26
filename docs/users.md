# Users and private fleets

[README](../README.md) · [Installation](installation.md) · [Architecture](architecture.md) · [Protocol](protocol.md)

One gateway can serve multiple users. Each user owns one private fleet; offline registrations are separate from the gateway's configurable live connection and node limits. Account-authenticated clients address workers by name, subject to access rules. Workers collaborate only under instruction-bound orchestrator delegation; fleet membership alone permits discovery. A client does not enroll its machine. Another user's credentials cannot discover or connect to those machines. See [terminology](terminology.md), [resource admission](protocol.md#gateway-resource-admission), and [delegation](delegation.md).

## Register a user

The gateway operator provisions accounts with the superuser key:

```sh
export CONTROL_GATEWAY=https://control.example.com
export CONTROL_SUPERUSER_KEY=YOUR_OPERATOR_KEY
control users create alice
control users create bob
```

Each command returns a user record and a random account key, shown once. Give each user only their own account key. Account registration is operator-provisioned; there is no public signup. The [web console](console.md) uses separately provisioned browser passwords, not account keys. An operator can run `control console password alice`; an account owner can set their own password with `control console password`.

On Alice's orchestrator machine:

```sh
control login --gateway https://control.example.com
control machines
control dashboard
```

Enter Alice's account key at the login prompt, or supply it through `CONTROL_USER_KEY`. Login persists it for future commands. The client needs no local node or machine enrollment. Alice can add workers to her fleet:

```sh
control machines add render --platform windows
control machines
control dashboard
```

Bob follows the same flow with his account key. He can also name a machine `render`; names are unique within a fleet. He sees only his own registrations, invitations, keys, and activity. Knowing Alice's machine ID does not grant access.

To also execute requested work on an orchestrator machine, run `control setup --name alice-laptop --client opencode`. Setup reuses the account login, issues a separate common key for the node, and installs its background startup. Use a different name on each machine. See [startup contexts](installation.md#choose-a-startup-context) before selecting a service account.

## Credential roles

| Credential | Scope |
| --- | --- |
| User account key | Manage that user's invitations, issued keys, and registrations; enroll and discover machines; initiate execution through account-client sessions, subject to destination access rules |
| Orchestrator key | Initiate work as an account client in the issuing fleet; cannot administer the fleet or enroll machines |
| Common key | Enroll and discover machines in its owning fleet; no account, fleet-management, or update administration |
| Redeemed installation key | Common permissions, with registration restricted to its bound identity, name, OS, and architecture |
| Gateway superuser | Provision/disable accounts and publish gateway-wide software updates; fleet management and account-client execution operate only on the operator's legacy fleet |

Account owners can also set, override and remove machine labels on the gateway with `control machines label` or the console. Labels remain discovery and selection metadata that grant no authority, and common keys cannot change them. See [owner labels](protocol.md#owner-labels).

Users issue, list, rename, and revoke their own keys with `control keys create|list|rename|revoke`. `control keys create NAME --role orchestrator` issues a key for execution and discovery; the default `--role common` grants enrollment and discovery. `PATCH /v1/fleet/keys/{id}` with `{"name":...}` changes only the label. Permissions are fixed at issuance. Fleet managers cannot issue account keys for other users, change credential ownership, publish executable updates, or inspect gateway-wide rollout status. Fleet-management commands appear in help for keys with that permission. Gateway administration appears only for the superuser.

Only account-authenticated client sessions initiate execution authority. Enrolling a node with an account key does not grant its node identity that authority. Keep account keys off execution workers, since someone who steals one can authenticate a new orchestrator client. Worker installation/common keys do not independently execute peer commands, including through the local API or a new common-key client session.

The console exposes [key permissions and recovery](console.md#lock-worker-access). Use `control keys remove ID` to permanently remove an already-revoked key. Reactivation and replacement of the account key use the independent browser login.

Select **Manage fleet** for a named key that needs to administer its own fleet. It can manage named keys with the same or narrower permissions. It cannot replace the account key, reset the browser login or administer other fleets.

For a build host that needs to enroll machines as well as run work, use `control keys create build-host --role orchestrator-enroll`. For discovery and execution without enrollment, use `--role orchestrator`. Use the issued key for `control login`. In the [web console](console.md#lock-worker-access), turn on the fleet lock and give the key an allowance for the intended workers. Every orchestrator using the key shares that allowance. Without a lock, the key can request work on any worker its other access rules permit.

Fleet account owners can manage worker secrets through encrypted CLI/console routes, subject to destination `secrets.manage` permission. Common keys cannot list metadata, set or delete through those routes. This is fixed fleet management, not a worker execution grant or value-read permission. See [remote secrets](secrets.md#manage-a-worker-from-the-orchestrator).

The gateway operator remains trusted. The operator owns the database and release-distribution authority and can inspect gateway metadata. Fleet isolation protects users from other users; it does not turn the gateway into an untrusted service or sandbox providers running on a machine.

## Enforcement

- Authentication resolves a credential to an immutable user ID. The gateway assigns node ownership from that authenticated identity, never from a client label or supplied user ID.
- Directory responses and offline notifications contain only the caller's fleet. Invitation/key lists and mutations are scoped by user. Requests for another user's registration, key, or invitation return the same not-found response as an unknown ID.
- Every signaling and relay packet checks the authenticated sender's fleet against the destination's fleet. The gateway overwrites the packet's sender identity. Raw packets cannot bypass directory filtering or forward a WebRTC offer to another fleet.
- Nodes obtain their fleet ID from `/v1/auth` and verify directory membership before accepting peer setup. Both WebRTC and relay sessions then require pinned mutual TLS.
- Node dispatch checks the attested membership before capability access rules and before artifact-grant handling. Wildcard access rules and signed artifact grants cannot create a cross-fleet connection.
- Identity ownership stays recorded after a registration is forgotten. The same key cannot move to another user, so cached direct sessions cannot become cross-user sessions after re-enrollment.

Task ownership remains narrower than fleet membership. One orchestrator can use a worker without gaining permission to cancel another orchestrator's tasks. Node-wide monitoring remains read-only and scoped to the fleet.

Revoking a key or disabling a user rejects subsequent gateway authentication and disconnects affected gateway sessions. It does not terminate already accepted work or close an established direct session inside that same fleet. A surviving transport is not permission to dispatch a new instruction: a worker that negotiated fleet-lock checks needs a gateway decision for new peer admissions, even over a warm connection.

## Persistence and upgrades

The gateway stores accounts, credential hashes, node registrations, permanent identity ownership, and invitations in `gateway.db`. SQLite uses WAL, full synchronous commits, foreign keys, and a unique machine-name constraint per user. Its driver is pure Go; all six supported platforms retain CGO-free builds. The gateway commits changes before acknowledging them and holds an exclusive state-directory lock.

Authenticated CLI/MCP client sessions belong to an account but are not fleet machines. Concurrent processes have independent transport identities and a shared persistent task-owner key. The gateway verifies both signatures and persists immutable account ownership, client role, and transport-to-owner bindings. Only live routing metadata is kept in memory, for same-account peer authentication. Machine discovery, status, enrollment management, and managed updates exclude clients. Revoking their credential or disabling their account disconnects them like other authenticated connections. See [standalone CLI and MCP](installation.md#standalone-cli-and-mcp).

Derived client identity names remain reserved within their account after disconnecting. A machine or installation invitation cannot claim that name and impersonate a client trusted by a node's name-based access rule. Prefer full identity IDs for access rules.

The gateway initially loads at most 4,096 client owner records and 4,096 transport bindings. It hydrates evicted identities from SQLite before identity/role checks and uses an indexed name-prefix lookup for reservations. Cache trimming retains live clients and their owners, so live-session counts can exceed the cache target. Eviction never deletes immutable account or role history from SQLite.

### Database schema

`gateway.db` uses schema version 10. A new state directory gets the whole schema, created from [`schema.sql`](../internal/gateway/schema.sql), and the `legacy` operator fleet in one transaction. Later schema changes upgrade it transactionally, one version at a time. Executables older than a database's schema refuse to open it, so back up the gateway state before upgrading.

Control v0.14.0 is the upgrade boundary. Later gateways open its databases unchanged and refuse older ones, naming that release. They also refuse to start in a directory holding only pre-SQLite `nodes.json`, `keys.json`, or `installations.json` state, rather than starting empty. To upgrade such state, start the gateway once with v0.14.0, then install the newer version. v0.14.0 imports pre-SQLite files into the `legacy` fleet, because old records cannot identify separate users. It also upgrades schema versions 1 through 9. Online backups and `control gateway backup verify` refuse databases older than schema version 10 the same way.

The tables are:

- `users`, `credentials`, and `access_key_times`: accounts, credential hashes, and each access key's last successful authentication time.
- `identities`: permanent identity ownership, never reassigned to another fleet.
- `nodes`, `machine_states`, `machine_labels`, and `proxy_dependencies`: registrations, owner lifecycle decisions and unregistration tombstones, owner label overlays, and managed proxy listener dependencies. Node policy changes and directory removal commit together, and disconnect cleanup never recreates a removed node. Unregistering a machine deletes its overlay in the same transaction. See [owner labels](protocol.md#owner-labels) and [gateway proxies](gateway-proxy.md).
- `client_identities` and `client_transports`: account-client roles and immutable transport-to-owner bindings. They store no registrations, session metadata, or routing metadata and are not pruned automatically. Client roles survive disconnects and gateway restarts and cannot be reused for machine enrollment. The persistent client key remains the task owner, so previous tasks and leases stay recoverable.
- `invitations`: installation invitations.
- `console_passwords` and `console_sessions`: salted Argon2id browser passwords with revocation revisions, and session digests and metadata. Accounts stay browser-disabled until provisioned. Valid browser logins survive updates and restarts; idle and absolute expiry still apply, and restart never renews them. Password resets, sign-out, and account disable commit revocation before acknowledgment. See [console security and limits](console.md#credentials-sessions-and-limits).
- `audit_events`: a bounded history of sign-ins and management changes, committed with the change it describes. Account owners read their own fleet's events with `control audit list` or the console Activity page; the operator reads every fleet's. See [activity log](console.md#activity-log).
- `fleet_vault` and `global_vault`: each fleet's lock policy with key-only allowances, and the operator's gateway-wide lock. Pauses and finite allowances use absolute UTC deadlines, so restarting the gateway does not renew them.

On startup, the gateway moves the previous standalone implementation's exact capability-free, unmanaged, unbound `cli-<identity-prefix>` entries out of `nodes`, preserving their account ownership and task-owner identity. Capability-bearing machines, invitation-bound identities, lifecycle-managed machines, and retired identities are not converted.

`CONTROL_TOKEN` on the gateway is an optional bootstrap common key for the legacy fleet only. For a new shared gateway, omit it and configure `CONTROL_SUPERUSER_KEY`, then create separate accounts. Do not give unrelated users the same account key or legacy bootstrap token.

**Upgrade the gateway before upgrading nodes from the pre-isolation version.** New nodes require fleet-aware authentication and refuse an older gateway. Use v0.14.0 for this one-time migration and pause automated release polling during it. Later managed updates independently reserve eligible nodes, allowing node-before-gateway installation only with explicit executable compatibility. An older coordinator retains its previous rollout order until upgraded. Starting the old executable against the stale JSON files does not reverse the SQLite migration.

To split an old shared pool into new accounts, enroll fresh node identities under those accounts in separate state directories. Existing identities and their task ownership remain in the legacy fleet. For backups, take an [online gateway backup](gateway.md#back-up-and-restore) of the database while the gateway runs. Stopping the gateway and copying its whole state directory, including the database and release storage, remains the offline alternative and also preserves rollout state. Node tasks, artifacts, and workspaces remain on their respective machines; executable blobs and rollout state retain their existing filesystem storage.

## API reference

| Endpoint | Permission | Result |
| --- | --- | --- |
| `POST /v1/admin/users` | Superuser | JSON `name`; returns `user` and one-time `token` |
| `GET /v1/admin/users` | Superuser | Registered users, without credentials |
| `DELETE /v1/admin/users/{id}` | Superuser | Disable that account and its credentials |
| `GET /v1/auth` | Authenticated | `role`, `userId`, management capabilities, and features such as `machineLabels` |
| `PUT /v1/console/password` | Account owner or superuser | Set own browser password; superuser may select an existing account by `username` |
| `GET /v1/nodes` | Authenticated | Only the credential owner's fleet |
| `GET /v1/client/connect` | Authenticated, unbound credential | Role-proven client session, without machine enrollment |
| `GET /v1/peers/{id}` | Authenticated | Same-account machine identity or a connected client's transient identity; other IDs return 404 |
| `GET /v1/status` | Authenticated | Gateway version, URL, resources, and own directory; account keys also receive fresh aggregate health for their fleet |
| `POST /v1/fleet/keys` | Fleet owner | JSON `name` and optional `role` (`common` by default, or `orchestrator`); returns key metadata and one-time `token` |
| `GET /v1/fleet/keys` | Fleet owner | Own credential metadata |
| `DELETE /v1/fleet/keys/{id}` | Fleet owner | Revoke own credential |
| `/v1/fleet/installations` | Fleet owner | Scoped installation management, detailed in [installation](installation.md) |
| `DELETE /v1/fleet/nodes/{id}` | Fleet owner | Forget own offline registration |
| `PATCH /v1/fleet/nodes/{id}` | Fleet owner | Enable/disable with JSON `disabled`, rename with JSON `name`, or change owner labels with JSON `labels` |
| `DELETE /v1/fleet/nodes/{id}?stop=true` | Fleet owner | Unregister own machine regardless of online state or lifecycle support |

The older `/v1/admin/keys`, `/v1/admin/installations`, and `/v1/admin/nodes/{id}` aliases remain superuser-only and operate on the legacy fleet. They do not provide a cross-user route. Account IDs cannot be reassigned or deleted; `users revoke` disables the account. The legacy operator fleet cannot be disabled through this endpoint.
