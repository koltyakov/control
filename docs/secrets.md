# Worker-local secrets

[README](../README.md) · [Protocol](protocol.md) · [GUI automation](rpa.md) · [Installation](installation.md)

Secrets let an orchestrator request credential use without putting the credential value in prompts, tool arguments, or task specifications. The worker resolves a name immediately before GUI input or an HTTP request. Control has no secret-value read command, RPC method, MCP tool, or resource. Secret management is local to the worker; values are not synchronized through the gateway or copied between nodes.

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

`list --metadata` returns a name-sorted JSON array containing `name` and known `createdAt` and `updatedAt` timestamps, never values. Timestamps use UTC RFC 3339 with fractional seconds when needed. Compare `updatedAt` with the current time to check the age of the last local credential entry. These dates describe Control storage, not when the credential was issued or changed in the application.

New secrets receive identical creation and update timestamps. Every successful `set`, including setting the same value, refreshes `updatedAt` and preserves the original `createdAt`. Deleting and recreating a name starts a new record. Unknown timestamps are omitted, not inferred from the storage file's modification time. Legacy secrets remain usable with both dates unknown until explicitly set again; that first update records only `updatedAt`, because their original creation date is still unknown. Reading, changing another secret, or deleting another secret does not invent or refresh their dates.

Storage is `<dataDir>/secrets/values.json`, outside `workDir`. Mutations use a separate file lock and atomic replacement. Unix directories use `0700` and files `0600`; Windows relies on the profile directory's inherited ACLs. Storage is not encrypted and is not an OS keychain. Protect the worker account, filesystem, and backups. If `dataDir` is inside `workDir`, move the profile's state or workspace before adding secrets. Existing secret storage inside `workDir` prevents node startup and credential use, including paths resolved through directory symlinks.

The reader accepts legacy name-to-string entries and records containing `value` with optional `createdAt` and `updatedAt`. Reads never rewrite storage. A successful mutation atomically writes records and their metadata together, retaining unknown dates on untouched legacy entries. Upgrade both the worker node and the local management CLI before mutating secrets with this format; older binaries cannot read the record format.

## Fill application fields by reference

With a configured desktop helper:

```json
{
  "actions": [
    {
      "type": "setValue",
      "target": { "app": "Example login", "name": "Password" },
      "secret": "app.password"
    }
  ]
}
```

Use actual selectors from GUI inspection. `setValue` and `type` accept exactly one of `text` or `secret`. The node replaces `secret` with `text` only in the private helper's stdin request. The helper contract is unchanged. Resolved values must satisfy the action contract and the helper's platform validation. Missing references reject the whole batch before desktop effects. Both synchronous `rpa.run` and durable tasks support references.

GUI text results and errors mask currently configured secret values, including in subsequent inspection calls. When any secrets are configured, Control suppresses raw helper stdout/stderr in task logs and failure diagnostics. This avoids leaks from chunked writes or truncated output. Structured results remain available after masking. Screenshots are not masked. Do not capture a screen displaying credentials or enable an application's reveal-password control.

### Agent-desktop login handoff

For an [agent-desktop workflow](rpa.md#use-agent-desktop-on-a-worker), prefer the opt-in [native `desktop.run` adapter](desktop.md). Pass `{"command":"setValue","ref":"@SNAPSHOT:e1","secret":"app.password"}` using a ref from that adapter's own snapshot. It resolves credentials on the worker and passes text only over an anonymous stdin pipe to Control's isolated Go helper mode. That subprocess calls agent-desktop's C library through purego, without C++, Python, or CGO. Private usernames can use the same secret-reference path.

Native status, snapshots, action results, and error envelopes mask currently configured values, even on later calls without a `secret` input. Raw native-helper logs are always suppressed, successful status omits private state metadata, and native state paths are masked. Persisted task arguments remain reference-only. The native helper has no screenshot, clipboard, tracing, or headed-input operations. Native refmaps can contain private observed text and live outside `workDir`; protect that state and its backups.

`rpa.run` remains an alternative on the same worker/profile, using inspected selectors rather than native refs. Ordinary agent-desktop CLI text commands do not understand Control secret names, and `exec.run` does not mask output. If protected input is not configured or headless input is unsupported, ask for manual login or approved setup. Never retrieve stored values through a shell, environment, clipboard, script, or filesystem call to bridge that gap. Use masked observations during credential entry and verify non-sensitive application state. Masking remains best-effort, not isolation from an unrestricted process.

## Authenticate HTTP requests by reference

```json
{
  "url": "https://api.example.com/status",
  "headerSecrets": {
    "Authorization": { "secret": "api.token", "prefix": "Bearer " }
  }
}
```

Pass this to `http.request` through `control call`, MCP `control_call`, or a task. `prefix` is optional. At most 32 secret headers are accepted. A header cannot also appear in literal `headers`, ignoring name case. All references resolve on the destination worker before sending the request. Secret-bearing requests return redirect responses without following them, even on the same host. Use HTTPS and only destinations authorized to receive the credential.

Control masks configured secret values in response headers, response body bytes before their base64 encoding, and request errors. JSON response strings are decoded before masking, which may reformat the JSON body. It also masks common base64, JSON-escaped, and URL-escaped representations. Requests without secret references retain normal redirect behavior. Arguments and persisted task specifications contain names, not resolved values. Rotating a secret before a queued task begins changes the value it uses; retrying an accepted task ID does not replay execution.

## What this protects

This prevents accidental exposure through the supported credential-entry operations. It is not protection against an AI or application trying to extract a credential. Existing fleet membership, capability access rules, task ownership, leases, and maintenance admission still apply. Permission to invoke `desktop.run`, `rpa.run`, or `http.request` includes permission to use the worker's stored secrets through that capability.

Unrestricted commands, filesystem access outside the filesystem API, custom helpers, malicious HTTP destinations, screenshots, application-managed files, and arbitrary value transformations can expose credentials. Masking covers configured values from the invocation's snapshot, not historical values deleted or rotated before a later inspection. Existing transcripts and retained outputs are not scrubbed retroactively. Do not use these secrets for gateway administration keys or assume they create an execution sandbox.
