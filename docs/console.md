# Web console

[README](../README.md) · [Gateway deployment](gateway.md) · [Users](users.md) · [Dashboard](dashboard.md) · [Protocol](protocol.md)

The gateway serves a password-protected management console at its public URL. `/` redirects to `/console`, which redirects signed-out visitors to `/login`. HTML, CSS, JavaScript, and icons are embedded in the Control binary. No frontend build, external CDN, separate web service, or Node.js runtime is needed to deploy it.

## Provision browser access

Browser passwords are separate from CLI account keys. Existing accounts have no browser access until a password is provisioned. There is no default password, public signup, or unauthenticated password-reset route.

With a saved gateway superuser login:

```sh
control console password
```

Enter and confirm a password at the hidden prompts. The operator's browser username is `@operator`. It is separate from fleet account names and remains usable only while the gateway has superuser administration configured.

To give an existing fleet account browser access:

```sh
control console password alice
```

Only the operator can select another account. An account owner can run `control console password` with their own saved account login to set or reset their own browser password. Common keys and installation credentials cannot provision passwords.

For noninteractive provisioning, use `--password-stdin`. It removes one trailing line ending and requires 12..1024 password bytes. Passwords are not accepted as command-line arguments. Do not put them in shell history or commit them to project files.

Open the gateway's HTTPS URL and sign in with the fleet account's exact, case-sensitive name or `@operator`. Users can change their own password on the Account page by entering their current password. Operators can set fleet passwords on the Accounts page. Every password change invalidates that login's existing browser sessions without rotating CLI keys or cancelling accepted work.

## Pages and actions

- Overview shows gateway resources with version and uptime, online and offline machine counts, and reported active work. Each count links to the corresponding Machines filter. A gateway sample older than one minute is labeled with its age. The overview always shows the fleet independently of the Machines search.
- Machines supports name, platform, and label search, state filters (All, Online, Busy, Idle, Offline, Disabled, with counts; reserved machines count as busy), and sorting by name, state, connections, CPU, memory, or disk. The disk column shows the first sampled node path with a known size; the detail drawer lists every sampled path. Unknown values sort last in either direction. The state column also shows active work by kind, lease expiry, last activity, missing health reports, and when offline machines were last seen. The connections column shows three fixed counters: direct peers, relayed peers (TURN or gateway relay), and active tunnels; hovering one shows its breakdown. CPU shows the logical core count. A System or User tag shows whether the node runs as a boot-time service account (root, or LocalService/SYSTEM on Windows) or in a user session. Narrow screens show each machine's state and resource usage in stacked rows. Select a machine row to open its detail drawer with resources, activity, platform, version, labels, identity, rename, enable/disable, and unregister with a typed name confirmation. An offline machine shows Wake when an online machine on its verified LAN can send a Wake-on-LAN packet for it; see [console wake](wake.md#wake-from-the-console). Offline and unknown metrics are not displayed as zero. Rename changes the gateway alias, not project-local scenario files on CLI hosts.
- Add machine chooses the platform and User, System, or Hybrid startup context, then displays a one-time installation command. The target detects its architecture. Blank names or `auto` use the target's hostname. The gateway needs a published bundle with both supported architectures for that OS. The dialog watches invitation redemption and then requests a fresh status snapshot before reporting the redeemed identity online. Hybrid completion requires both identities online in their expected contexts.
- Invitations lists metadata and permits revocation. Redeemed invitations link to their machine, which is renamed, disabled, or unregistered from its detail drawer rather than revoked here. Clear history, after confirmation, deletes revoked invitations and unredeemed ones past expiry and hides redeemed ones; the hidden records keep their machines' credentials, so enrolled machines keep working. Pending invitations stay. `control machines invites clear` does the same. Revoking a redeemed invitation also revokes its installation key, disconnecting its gateway session. Accepted work and established direct sessions may continue.
- Access keys creates, renames, and revokes common fleet keys. Renaming changes only the label. New secrets are displayed once. Account keys are not listed on this page. Installation commands also cannot be recovered after their dialog closes.
- Account changes the current browser password. The account menu under the avatar links there, selects the system, light, or dark theme, and signs out after confirmation. Sign out revokes only the current browser session.
- Operator-only Accounts provisions and disables private fleets and sets their browser passwords. Account creation shows the CLI account key once.
- Operator-only Updates displays gateway-wide deployment progress and node acknowledgments. A confirmed release check may begin a verified, idle-only rollout. Development bundle publication stays in `control update push DIR`.

A pending invitation name conflict offers an explicit checkbox to retract the existing invitation on the next submission. It never forces or retries a request automatically. A lost mutation response instructs the user to refresh and inspect state before submitting again.

Right-click a machine row on Overview or Machines, or use its three-dot Actions button, to view details, rename, change startup context for a User node, enable/disable, wake when eligible, or unregister. Rename and lifecycle actions keep their existing confirmations, including the typed name for unregister. Shift+F10 or the Context Menu key opens the menu from a focused machine name; arrow keys, Home, and End move between actions, and Escape closes it. Shift+right-click keeps the browser's native context menu.

The console does not submit tasks, open peer connections, read task logs/results, stream artifacts, or manage host-local tunnels. Account views contain only their fleet. The operator can administer users and gateway-wide updates, but its machine view and machine actions remain scoped to the legacy operator fleet, not all users' machines.

### Change an installed User node's startup context

Choose **Startup context** in the machine's actions menu. In the detail drawer, Startup context and Unregister are under the three-dot More actions button; Rename, Enable/Disable, and Wake when available stay visible. This is available for Linux, macOS, and Windows nodes reporting User execution, including offline nodes. **Apply startup context** first requests the change from the node through the gateway. The node must be idle and permitted by `service.startup` allow rules. No arbitrary scripts, remote profile paths, UAC, or sudo prompts are sent. If the node cannot apply the change, the console displays a command to run locally. The optional configuration path affects that fallback only; the remote request always applies to the running profile.

- Windows System can apply remotely only when the installed node itself already runs with administrator rights. Its verified helper waits for the old supervisor to stop and migrates the original profile to boot-time LocalService startup. A normal limited User node returns a local fallback, `control service start --mode auto`, with `--config` when supplied. Run it in Administrator PowerShell under the same user, not through a node execution provider. It retains identity, workspace, and history, and needs no invitation or published installer. Admission is not completion: the dialog waits for that identity to reconnect and report System execution. If unconfirmed after one minute, inspect the machine and `startup-change.log` before using the displayed recovery command.
- Linux/macOS System needs a separate root-owned profile and an unused routing name. The command first runs `control service uninstall --mode user` against the existing profile, then the new System installer with sudo. The old files and registration remain, but identity, credentials, workspace, and task history are not migrated. Verify the new System node before separately unregistering the old registration. To keep the original name instead, follow the explicit [Unix replacement procedure](installation.md#choose-a-startup-context).
- Hybrid creates a same-name replacement User invitation and an independent `NAME-system` invitation. Run it from the user's shell with sudo available on Unix, or Administrator PowerShell under that user on Windows. It re-enrolls the original User profile and installs a protected companion. Both contexts must report online before the dialog reports completion. An existing companion is not replaced; manage it separately. Names longer than 56 characters need renaming first.

System and Hybrid installation commands expire after 15 minutes and are shown only once. Generating one does not stop, unregister, or elevate the existing node. Pending invitation conflicts require explicit retraction confirmation; lost responses are never automatically retried. See [startup contexts](installation.md#choose-a-startup-context) and [Hybrid isolation](installation.md#hybrid-user-and-system-contexts).

### Hybrid rows and counters

A Hybrid host has one machine row with User and System chips. The gateway supplies the pairing from its enrollment records; similarly named unrelated machines are not grouped. The machine counter, filter counts, and online/offline totals count hosts once, not their System companions. A host is online if either context is online, and offline only if both are offline. Offline context chips are struck through and remain selectable. The row's status reflects connected contexts regardless of chip selection, with updating, busy, and reserved reports taking precedence over idle reports. An offline row's last-seen age uses the more recent observation. Active work still counts real operations in both contexts.

User is selected by default, including when only System is online. Select System to inspect that context's metrics or manage it; the drawer shows its actual routing name. Rename, disable, unregister, and other actions affect only the selected context, not both. Chip selection stays in memory across polling and is cleared at sign-out. It does not change CLI/MCP routing. User work does not automatically retry as System; explicitly choose System for an authorized operation that requires elevation.

## Refresh and accessibility

The console reads gateway snapshots every five seconds while its tab is visible. Refresh requests do not initiate resource sampling or peer probes. Gateway metrics retain their original collection times; node health expires under the same rules as the terminal dashboard. Missing fresh health means unknown work, not zero work. Reported work is a count of tracked operations, not necessarily distinct jobs.

Failed requests preserve the last view and show a warning with the last update time. The console supports light/dark themes, narrow layouts, labeled native form controls, keyboard-scrollable tables, keyboard focus, modal dialogs, and reduced motion. Theme selection is the only value stored in browser local storage. Passwords, session tokens, keys, and installation commands are not stored there.

## HTTPS and reverse proxies

Use the existing [Caddy](gateway.md#5-enable-public-https) or [expose](expose.md) deployment with the gateway on loopback and `CONTROL_PUBLIC_URL=https://control.example.com`. Preserve the original HTTP Host header. Password endpoints and console pages reject hosts that differ from the configured public origin.

TLS can terminate directly in Control or at a trusted loopback proxy. The console accepts a configured HTTPS public origin over a plaintext upstream only when the socket peer is loopback. It does not trust `Forwarded` or `X-Forwarded-*` headers. A proxy on another host or container network must use TLS to the gateway, or connect through a trusted local forward. Keep the plaintext gateway listener inaccessible from public networks. Do not put a second login in front of the whole gateway, because CLI and node API traffic must retain bearer authentication.

Without a public URL, direct HTTPS works. Plain HTTP is accepted only for direct loopback development with a loopback Host such as `http://127.0.0.1:7330`. A configured loopback HTTP public URL is also supported. Remote plaintext access is rejected, including password provisioning.

## Credentials, sessions, and limits

SQLite schema version 5 added browser password records referencing immutable existing user IDs. Passwords use Argon2id with a random 16-byte salt, 19 MiB of memory, two passes, one lane, and a 32-byte derived hash. Password work runs outside the gateway mutation lock, with at most two concurrent verifications/derivations and no waiting queue. Changes commit before acknowledgment; failed writes retain the old password and sessions.

Successful login generates a new 256-bit opaque cookie. The gateway retains only its digest in a bounded, in-memory session map. Cookies are HttpOnly, SameSite=Strict, and Secure for HTTPS origins. Sessions expire after 30 minutes without authenticated requests or 12 hours total. Polling counts as authenticated activity. All sessions disappear on gateway restart; passwords survive. Account disable and password revisions are rechecked on authenticated requests. Operator sessions also require configured superuser administration.

There are at most 4,096 sessions and a bounded login-attempt map. Over a 15-minute window, admission permits up to 10 attempts per username, 60 per socket source address, and 120 globally. These counts include successful logins. Loopback reverse proxies share a source-address allowance because forwarded addresses are not trusted. An overloaded verifier returns HTTP 429 rather than queueing password work.

Browser mutations require both the exact same Origin and a session-bound CSRF header. Login requires the same Origin too. Responses use `no-store`, a restrictive Content Security Policy, no-referrer policy, and frame blocking. Cookies authorize only an explicit management-route allowlist under `/console/api/`. They do not authenticate `/v1/` APIs, node registration, or peer/client WebSockets. CLI keys never need to be copied into a browser.

Back up the gateway state before upgrading. Earlier gateways reject schema version 6. Restoring an older executable does not reverse the database migration. Browser access has no MFA; protect operator passwords and the gateway state accordingly.

## Activity log

Activity lists sign-ins, failed sign-ins, sign-outs, and management changes newest first: key creation, rename, and revocation; invitation creation and revocation; machine enrollment, rename, enable/disable, and unregistration; account creation, disable, and browser password changes; and, for the operator, update publication and release checks. Each event records its time, fleet, actor (browser login, or account name with the key ID for bearer keys), action, target, a short detail such as a previous name, and source IP. Events never contain passwords, keys, installation commands, or execution arguments. Filter by category and load older pages; new events appear at the top while the page is open.

Account owners see their own fleet's events. The operator sees every fleet, including failed sign-ins for unknown usernames, which belong to the operator fleet. The source IP is the direct TCP peer, because forwarded headers are not trusted; behind a reverse proxy it is the proxy's address.

Schema version 6 adds `audit_events`. Each event commits in the same SQLite transaction as the change it describes, so a change is never acknowledged without its record. A sign-in is refused if its record cannot be written; a failed sign-in or sign-out still proceeds. Update publication and release checks live in the update store, so their events are written immediately afterwards and a failed write is logged. The gateway keeps the newest 50,000 events, with failed sign-ins limited separately to the newest 2,000 so password guessing cannot displace management history. `GET /v1/audit` and `control audit list [N]` read the same log.

Clear log removes all events, or only those older than 7 or 30 days, after confirmation. Account owners clear their own fleet; the operator clears every fleet. Each fleet that loses events receives one "Cleared activity log" event with the actor, source IP, and count, written in the same transaction, so history cannot disappear without a trace. `DELETE /v1/audit` and `control audit clear [DAYS]` do the same.

## Verification

Core Go tests cover password provisioning, transactional rollback, migration/restart persistence, login, logout, expiry, account disable, password resets, CSRF/origins, secure cookies, verifier limits, fleet isolation, and browser-to-bearer authority separation. They run in the normal native and Compose suites.

Optional browser smoke verification requires Node.js 22+ and an isolated Chromium instance:

```sh
make build
npm --prefix tests/console ci
npm --prefix tests/console exec -- playwright install chromium
npm --prefix tests/console test
```

The smoke script starts its own temporary gateway and node. It does not read saved Control credentials or touch a real fleet. `CONTROL_CONSOLE_TEST_CHROME` can select an installed Chrome executable for headless testing. `CONTROL_CONSOLE_SCREENSHOTS` selects an output directory for desktop/mobile screenshots. Temporary fixture state is retained for inspection; owned processes are stopped when the script finishes.

The favicon and console brand icon use the selected ivory `ctrl` keycap artwork, not the mascot illustration. The original download is preserved in `assets/icon-source.png` from [the selected image](https://chatgpt.com/s/m_6ade4fe3bd8c81918065326918846c74). The generator removes stray pixels with alpha at most 1, crops to the remaining content, and centers it on a transparent square with 1% padding. It writes `assets/icon-cropped.png`, a 512px `assets/icon.png`, a 64px console brand icon, a 16/32/48px ICO, a 32px PNG favicon, and a 180px Apple touch icon. Regenerate them with `npm --prefix tests/console run icons` after installing the browser-test dependencies above. The generator supports `CONTROL_CONSOLE_TEST_CHROME` too. Browser tests check the served assets, dimensions, tight crop, transparency, and matching header/login icons.
