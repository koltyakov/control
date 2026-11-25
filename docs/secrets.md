# Worker-local secrets

[README](../README.md) · [Protocol](protocol.md) · [Native desktop](desktop.md) · [Installation](installation.md)

Secrets let an orchestrator request credential use without putting the credential value in prompts, tool arguments, or task specifications. The worker resolves a name immediately before command execution, GUI input or an HTTP request. Control has no secret-value read command, RPC method, MCP tool, or resource. Values are stored only on the worker and supplied to the requested application. Fleet owners can manage them locally, through encrypted CLI writes, or through the web console. The gateway relays ciphertext without storing values or synchronizing them between nodes.

## Enter credentials outside chat

Run these commands yourself on the worker, under the OS account that owns its profile:

```sh
control secrets set app.password
control secrets set api.token
control secrets list
control secrets list --metadata
control secrets delete app.password
```

`set` uses a hidden terminal prompt. Do not paste the value into an AI conversation or supply it as a command argument. `--stdin` accepts a value from an external credential source, removing one trailing newline or CRLF and preserving other whitespace. Never run credential entry through an AI tool that records its input. Global `--config PATH` selects a different worker profile. The node need not be running, and existing nodes read changes on their next GUI or HTTP invocation.

Names contain 1..128 ASCII letters, digits, dots, underscores, or hyphens and begin with a letter or digit. Values contain 1..4096 UTF-8 bytes without NUL. Each profile stores at most 128 secrets and at most 1 MiB of encoded data. `list` returns names only. `delete` removes the named entry without returning its value.

`list --metadata` returns a name-sorted JSON array containing `name`, known `createdAt` and `updatedAt` timestamps and, for restricted secrets, the [use policy](#restrict-where-a-secret-can-be-used) as `policy` or `policyInvalid: true`, never values. Timestamps use UTC RFC 3339 with fractional seconds when needed. Compare `updatedAt` with the current time to check the age of the last local credential entry. These dates describe Control storage, not when the credential was issued or changed in the application.

New secrets receive identical creation and update timestamps. Every successful `set`, including setting the same value, refreshes `updatedAt` and preserves the original `createdAt`. Deleting and recreating a name starts a new record. Unknown timestamps are omitted, not inferred from the storage file's modification time. Legacy secrets remain usable with both dates unknown until explicitly set again; that first update records only `updatedAt`, because their original creation date is still unknown. Reading, changing another secret, or deleting another secret does not invent or refresh their dates.

Storage is `<dataDir>/secrets/values.json`, outside `workDir`. Mutations use a separate file lock and atomic replacement. Unix directories use `0700` and files `0600`; Windows relies on the profile directory's inherited ACLs. Storage is not encrypted and is not an OS keychain. Protect the worker account, filesystem, and backups. If `dataDir` is inside `workDir`, move the profile's state or workspace before adding secrets. Existing secret storage inside `workDir` prevents node startup and credential use, including paths resolved through directory symlinks and Windows junctions.

The reader accepts legacy name-to-string entries and records containing `value` with optional `createdAt`, `updatedAt` and `policy`. Reads never rewrite storage. A successful mutation atomically writes records and their metadata together, retaining unknown dates on untouched legacy entries. Upgrade both the worker node and the local management CLI before mutating secrets with this format; older binaries cannot read the record format.

## Manage a worker from the orchestrator

Successful web-console creation and replacement return to the refreshed secrets list in the same side panel. The console does not show or edit use policies. Replacing a value there keeps the secret's stored policy. Manage policies with the CLI. Cancel in either editor clears the entry and returns to that list without saving. If the write succeeds but refreshing metadata fails, the panel offers a read-only list reload rather than another save.

From your own orchestrator terminal, logged in with your fleet account key:

```sh
control secrets set app.password --node worker
control secrets list --node worker --metadata
control secrets delete app.password --node worker
```

Without `--node`, commands retain their local behavior. An explicit empty target fails, never falling back to local storage. Remote commands target the running worker profile, not an arbitrary remote path. Local `--config` selects the orchestrator's gateway configuration. The CLI checks access before reading a value, then uses the same hidden prompt or explicit `--stdin` contract as local set. Never supply values through `control exec`, task arguments, or AI tools.

In the [web console](console.md), choose **Secrets** from an online machine's actions menu or the detail drawer's More actions menu. A right-side panel lists names and last-updated timestamps only, without creation dates or relative age. Plus, pencil and trash icons have accessible labels and tooltips for Add, Replace and Delete; deletion requires confirmation. Entry and confirmation remain in the panel. Values are never displayed or retrieved. Single-line editors use textarea-backed controls outside a form, without username/password field names, with autocomplete disabled and the value visually masked. Their height stays at 38 pixels, with no wrapping, resize handles or Enter-to-submit behavior. This avoids Safari's password classification of CSS-masked HTML inputs, including search inputs. Multiline values are rejected without silently changing them; enter those through the CLI. Browser or password-manager extensions can still apply their own heuristics; no web page can universally disable their suggestions. Browsers without safe text masking must use the CLI instead. Values are cleared before sending, including failed writes. Closing a panel or signing out removes its inputs. Values are not saved in browser storage.

Remote management requires updated gateway, worker and CLI/browser versions, an online enabled worker, and `secrets.manage` permission in the worker's `@orchestrator` or wildcard allow rules. Default unrestricted account rules permit it; empty rules deny it. Common/installation keys, peer callers and delegated workers cannot manage secrets. This is fixed fleet management, not a provider, task capability, peer RPC or MCP tool. An explicit Hybrid System target manages only that context's store, without granting execution or switching privileges.

Both orchestrator and worker require HTTPS to the gateway, with HTTP permitted only for direct loopback development. Browser entry also requires Web Crypto. The worker generates a P-256 key per process and advertises its public key in signed registration. CLI/browser encryption uses ephemeral P-256 ECDH, HKDF-SHA-256 and AES-256-GCM. Associated data binds worker identity, secret name, request ID and expiry. A use policy set with the value travels inside the same ciphertext; see [remote policies](#set-policies-remotely). The gateway relays ciphertext and value-free metadata without decrypting values. Audit records contain machine, operation and outcome, without secret names, values, policies or ciphertext.

Envelopes expire after 30 seconds by default. Workers accept deadlines at most one minute ahead and retain up to 128 unexpired IDs without evicting replay protection. There is one request in flight per worker and at most 64 gateway-wide. Processing participates in worker maintenance admission. There is no offline queue or automatic retry. A lost response has an unknown outcome; inspect metadata before deciding whether to submit another operation. A worker restart changes its key, so stale encrypted writes fail. Check both machines' clocks if valid requests expire unexpectedly.

Encryption keeps plaintext out of normal relay traffic, gateway persistence, tasks and logs. It does not make a compromised gateway trustworthy: the gateway authenticates management, supplies public-key discovery and serves browser JavaScript. Names and dates remain visible metadata. Worker storage is unencrypted, and applications and unrestricted worker processes can access credentials. See [the limits below](#what-this-protects).

## Fill application fields by reference

With the configured [native desktop adapter](desktop.md), pass this request to `desktop.run`, or as MCP `control_desktop.request`:

```json
{
  "command": "setValue",
  "ref": "@SNAPSHOT:e1",
  "secret": "app.password"
}
```

Use a qualified ref from that adapter's own fresh snapshot. `setValue` accepts exactly one of `text` or `secret`. Missing secrets fail before helper launch. Both synchronous `desktop.run` calls and durable tasks support references. There is no secret-reference keystroke typing or physical-input fallback. Do not capture a screen displaying credentials or enable an application's reveal-password control.

### Agent-desktop login handoff

For an [agent-desktop workflow](desktop.md#use-agent-desktop-on-a-worker), use the opt-in native adapter for credential entry. It resolves credentials on the worker and passes text only over an anonymous stdin pipe to Control's isolated Go helper mode. That subprocess calls agent-desktop's C library through purego, without C++, Python, or CGO. Private usernames can use the same secret-reference path.

Native status, snapshots, action results, and error envelopes mask currently configured values, even on later calls without a `secret` input. Raw native-helper logs are always suppressed, successful status omits private state metadata, and native state paths are masked. Persisted task arguments remain reference-only. The native helper has no screenshot, clipboard, tracing, or headed-input operations. Native refmaps can contain private observed text and live outside `workDir`; protect that state and its backups.

Ordinary agent-desktop CLI text commands do not understand Control secret names. `exec.run` without secret references does not mask output or share desktop locking. If protected GUI input is not configured or headless input is unsupported, ask for manual login or approved setup. Never retrieve stored values through a shell, environment, clipboard, script, or filesystem call to bridge that gap. The command injection below is for applications and SDKs that consume credentials directly, not a substitute for the native GUI input path. Use masked observations during credential entry and verify non-sensitive application state. Masking remains best-effort, not isolation from an unrestricted process. The removed RPA adapter has no automatic fallback; see [migration differences](desktop.md#migrate-from-rpa).

## Authenticate commands and SDKs by reference

`exec.run` supports the same pattern as job runners that inject secrets into an application's environment. Map the environment variable the application expects to a worker-local secret name:

```json
{
  "command": "python3",
  "args": ["approved-domain-audit.py"],
  "env": { "DOMAIN_USER": "ARVO\\andrew.koltyakov" },
  "envSecrets": { "DOMAIN_PASSWORD": "domain.admin.password" }
}
```

The approved program reads `DOMAIN_PASSWORD` from its own process environment and passes it to its LDAP/RPC client. It must not print the value, put it in a subprocess argument, or save it to a file. This removes the need to re-enter a stored password over SSH. It does not install an audit program or authorize new domain operations.

Cloud CLIs and SDKs work the same way, using their documented environment variables:

```json
{
  "command": "aws",
  "args": ["sts", "get-caller-identity"],
  "envSecrets": {
    "AWS_ACCESS_KEY_ID": "aws.access-key-id",
    "AWS_SECRET_ACCESS_KEY": "aws.secret-access-key"
  }
}
```

For Azure service-principal SDK authentication, map `AZURE_CLIENT_ID`, `AZURE_TENANT_ID` and `AZURE_CLIENT_SECRET` to the corresponding stored names. Control does not create cloud identities, grant permissions or choose an authentication flow.

If an installed command explicitly supports a password on stdin, use `"stdinSecret": "domain.admin.password"` instead. This sends the exact stored UTF-8 bytes through the command's stdin pipe, without adding a newline. `stdinSecret` and literal `stdin` are mutually exclusive, including an empty `stdin`. Environment references can accompany ordinary non-secret stdin or `stdinSecret`. There is no argv substitution, template expansion, temporary credential file or secret-value read operation.

Pass these objects through `control call WORKER exec.run`, MCP `control_call` with method `exec.run`, or the `args` of a tracked `exec.run` task. Use explicit task IDs for operations needing reconciliation, and keep `leaseId` when leased. Check the worker's advertised `exec.run` schema for `envSecrets` and `stdinSecret` before use; older workers ignore unsupported JSON fields and must be upgraded first.

At most 32 environment references are allowed. Variable names match `[A-Za-z_][A-Za-z0-9_]{0,127}` and must be unique ignoring case on every OS. A variable cannot also appear in literal `env`, ignoring case. References override inherited variables for this invocation only, not the node's environment or unrelated work. Invalid or missing references fail before launching the command. Values resolve after queueing, when execution starts; task specifications, delegation checks and idempotent comparisons retain only names.

For commands with references, Control captures each output stream up to 2 MiB without publishing raw chunks. Complete UTF-8 stdout, stderr and errors mask all secrets from the invocation's snapshot, including common encoded forms and decoded JSON strings. Masking can reformat JSON text or replace numeric tokens; stdout and stderr remain text fields, not parsed response objects. Sanitized stdout and stderr are then written to task logs in that order, only after the command ends. Live log following therefore has no output until completion. Oversized, cancelled, incompletely drained or non-UTF-8 output is suppressed in both streams and fails execution, even if the program exited successfully. Head-and-tail retention for longer output does not apply: no head, tail or omitted count is published or logged, so a task log tail never exists for these commands. This failure may follow completed effects and never authorizes replay. Commands without references keep their existing raw output and live-log behavior.

Injected environment values are visible to the program, its descendants, and other processes or administrators with sufficient OS access. Stdin recipients can also retain or expose input. Do not dump environments, enable credential-bearing debug traces, publish credential files or pass values onward in argv. Files, artifacts, arbitrary encodings, subprocess-managed logs and later calls without references are not scrubbed. Prefer stdin when the application supports it and the environment's wider visibility is unnecessary.

## Authenticate HTTP requests by reference

```json
{
  "url": "https://api.example.com/status",
  "headerSecrets": {
    "Authorization": { "secret": "api.token", "prefix": "Bearer " }
  }
}
```

Pass this to `http.request` through `control call`, MCP `control_call`, or a task. `prefix` is optional. At most 32 secret headers are accepted. A header cannot also appear in literal `headers`, ignoring name case. All references resolve on the destination worker before sending the request. Secret-bearing requests return redirect responses without following them, even on the same host. Use HTTPS and only destinations authorized to receive the credential. To make the worker enforce this, [restrict the secret to its hosts](#http-destinations).

Control masks configured secret values in response headers, response body bytes before their base64 encoding, and request errors. JSON response strings are decoded before masking, which may reformat the JSON body. It also masks common base64, JSON-escaped, and URL-escaped representations. Requests without secret references retain normal redirect behavior. Arguments and persisted task specifications contain names, not resolved values. Rotating a secret before a queued task begins changes the value it uses; retrying an accepted task ID does not replay execution.

`http.download` accepts the same `headerSecrets` with the same limits, conflict rule and no-redirect behavior, and masks its errors and returned headers the same way. It streams the response body into an artifact instead of returning it, and those downloaded bytes are not masked: an artifact may contain a secret echoed by the server. Without secret references it follows at most 10 http/https redirects. See [`http.download`](protocol.md#node-operations).

## Restrict where a secret can be used

Without a policy, any authorized `exec.run`, `desktop.run`, `http.request` or `http.download` invocation can use a stored secret. It can inject the secret into any command or send it to any URL. A mistaken or prompt-injected agent can therefore send a credential to the wrong place. A use policy, stored with the secret on the worker, limits which consumers may resolve it and where HTTP may send it:

```sh
control secrets set api.token --use http --host api.example.com --host '*.api.example.com'
control secrets set domain.admin.password --use exec
control secrets set app.password --use desktop,exec
control secrets policy api.token --use http --host api.example.com:8443   # local, value unchanged
control secrets policy api.token --clear                                   # unrestricted again
control secrets set api.token --clear-policy                               # new value, unrestricted
control secrets list --metadata
```

`--use` takes a comma-separated list of `http` (`headerSecrets` of `http.request` and `http.download`), `exec` (`envSecrets` and `stdinSecret`) and `desktop` (native `setValue`). A secret without a policy behaves exactly as before. A secret with a policy is refused for every consumer and destination the policy does not list.

`set` without policy flags changes only the value and keeps any stored policy. `set` with `--use`, `--host` or `--plain-http` replaces the value and the policy in one atomic write. `--clear-policy` removes the policy in the same write. `policy` changes only the policy of an existing local secret, without the value or its `createdAt`/`updatedAt`, which describe credential entry. `policy` has no `--node` form; see [remote policies](#set-policies-remotely).

### HTTP destinations

A policy that allows `http` needs at least one `--host`, up to 16. Each pattern is one of:

- an exact host name, such as `api.example.com`;
- a leading wildcard, such as `*.api.example.com`, which matches one or more labels in front of `api.example.com`, such as `eu.api.example.com` or `a.b.api.example.com`, but not `api.example.com` itself, `evilapi.example.com` or `api.example.com.evil.test`. The suffix after `*.` needs at least two labels;
- an IPv4 literal, such as `192.0.2.10`, or a bracketed IPv6 literal, such as `[2001:db8::1]`.

Each pattern can end with `:PORT`. Without a port, a pattern matches only the scheme's default port: 443, or 80 when plain HTTP is allowed. Host names are compared case-insensitively, with one trailing dot removed. Only HTTPS is permitted unless `--plain-http` explicitly allows `http://` as well.

The worker checks the URL its HTTP client will connect to, after parsing, not the text the caller wrote. A URL with userinfo (`https://user@host/`) is refused. IP-literal hosts match only an identical IP-literal pattern, never a host name or wildcard. IPv4-mapped IPv6 and zoned addresses are refused. Host names are ASCII. Write internationalized names in their `xn--` form in both patterns and URLs, because Unicode host names are refused, not converted. Labels contain letters, digits and hyphens. A final label that is numeric or `0x` hexadecimal, such as `127.1`, is refused because some resolvers read it as an address. Secret-bearing requests never follow redirects, so a permitted host cannot forward the credential elsewhere. The redirect response is returned instead. The worker's `HTTP_PROXY`/`HTTPS_PROXY` environment is trusted: plain HTTP through a proxy discloses the header to it.

Host matching trusts DNS. It does not defend against an attacker who controls the name's DNS records or the network path when TLS is not used: with `--plain-http`, whoever answers for the name receives the credential. With HTTPS, Go's certificate verification for the same host name protects against redirection at the DNS and network level.

### Commands and desktop input

For `exec` and `desktop`, a policy only allows or denies the use as a whole. There is no allowed-command list. A command name or path is chosen by the same caller the policy constrains, and an unrestricted process can spawn any program under a permitted name or read its own environment. A command list would be false assurance. Use `exec` only for secrets meant for authorized commands on that worker, and leave it out for API tokens that only HTTP needs.

### Enforcement and errors

The worker enforces policies when a reference resolves, in one shared check that every consumer calls before it reads a value. All references of one invocation are checked first. One refused reference fails the whole invocation before any command starts, header is set, request is sent or text is typed. Errors name the secret and the category of refusal: `secret "api.token" is not permitted for exec use by its policy on this worker`, or `... for this HTTP destination ...; nothing was sent`. They never contain the value, and they never contain the policy's hosts or other uses, which a caller without `list --metadata` access cannot otherwise see. Missing references keep their existing error. Masking is unchanged: refused and restricted values are still masked in every result. `exec.run` output masking stays all-or-nothing.

### Storage

The policy is stored in the secret's record in `values.json`, under the same lock and atomic replacement as the value, and is bounded to 2 KiB. Values are stored exactly as before. A store that contains at least one policy also carries a `"#format": 2` entry. Older Control binaries reject that entry as an invalid name and refuse the whole store, so a downgraded node fails to start instead of treating restricted secrets as unrestricted. Upgrade the node and the local CLI before adding policies. To downgrade, clear every policy first. Stores without policies keep their exact previous format.

A present policy that fails strict validation, including `null`, unknown fields, a non-canonical host or an oversized policy, makes that secret unusable for every consumer. It never falls back to unrestricted. Other secrets keep working, values are still masked, and `list --metadata` reports `policyInvalid: true`. Unrelated writes preserve the unreadable policy verbatim. Run `control secrets policy NAME ...` or `--clear` to repair it.

### Set policies remotely

`control secrets set NAME --node WORKER` with policy flags or `--clear-policy` encrypts the value and the policy together, using envelope version 2 ([protocol](protocol.md#node-operations)). The policy is inside the authenticated plaintext. A relay cannot strip, weaken or alter it, and cannot relabel the envelope as a value-only one: the version selects the key derivation and associated data, so a changed version fails to open. The client refuses before reading the value unless the worker advertises policy support in its signed registration and the gateway reports it. An older worker would ignore the policy and store the value unrestricted. Older gateways reject the versioned envelope outright, so nothing is sent through them.

Remote policy changes always carry the value. Envelopes are encrypted to the worker but not authenticated to a sender. A gateway can therefore create its own envelopes. A value-free remote policy change would let a compromised gateway remove the restriction of a credential it cannot read. With the value required, a forged write can only replace the credential with one the gateway already knows. Use `control secrets policy` on the worker itself for policy-only changes.

Remote value-only writes, from `set --node` without policy flags, older clients and the web console, keep the worker's stored policy, so editing a value cannot silently remove a restriction. Workers return policies in remote listings only to gateways that ask for them. As with values, the gateway is trusted for key discovery. An actively compromised gateway that substitutes the worker key can read and change both the value and the policy. This does not protect against that. Displayed remote policies are relayed by the gateway and are not authenticated. Check them on the worker when that matters. Encrypted writes are not ordered either: within an envelope's lifetime, a gateway can deliver an earlier write after a later one, so inspect metadata after changing a policy.

### What a policy does not protect against

Policies stop accidental and prompt-injected misuse through the structured secret-reference paths. They are not a sandbox. Any `exec.run` on the same worker, with or without references, runs with the node's OS permissions and can read `values.json`, including restricted values, and can rewrite policies. So can custom providers and other processes of the same OS account. The filesystem API cannot, because the store must be outside `workDir`. A command permitted to receive a secret can pass it anywhere. Restricting a credential's use is meaningful only when callers cannot also run unrestricted commands on that worker: restrict `exec.run` in the worker's allow rules for such callers.

## What this protects

This prevents accidental exposure through the supported credential-use operations. [Use policies](#restrict-where-a-secret-can-be-used) additionally limit which consumers and HTTP destinations those operations may use. It is not protection against an AI or application trying to extract a credential. Existing fleet membership, capability access rules, task ownership, leases, and maintenance admission still apply. Permission to invoke `exec.run`, `desktop.run`, `http.request` or `http.download` includes permission to use the worker's stored secrets through that capability. Command injection adds no secret-management or peer authority.

Unrestricted commands, filesystem access outside the filesystem API, custom helpers, malicious HTTP destinations, screenshots, application-managed files, and arbitrary value transformations can expose credentials. Masking covers configured values from the invocation's snapshot, not historical values deleted or rotated before a later inspection. Existing transcripts and retained outputs are not scrubbed retroactively. Do not use these secrets for gateway administration keys or assume they create an execution sandbox.
