# Worker-local secrets

[README](../README.md) · [Protocol](protocol.md) · [GUI automation](rpa.md) · [Installation](installation.md)

Secrets let an orchestrator request credential use without putting the credential value in prompts, tool arguments, or task specifications. The worker resolves a name immediately before GUI input or an HTTP request. Control has no secret-value read command, RPC method, MCP tool, or resource. Secret management is local to the worker; values are not synchronized through the gateway or copied between nodes.

## Enter credentials outside chat

Run these commands yourself on the worker, under the OS account that owns its profile:

```sh
control secrets set app.password
control secrets set api.token
control secrets list
control secrets delete app.password
```

`set` uses a hidden terminal prompt. Do not paste the value into an AI conversation or supply it as a command argument. `--stdin` accepts a value from an external credential source, removing one trailing newline or CRLF and preserving other whitespace. Never run credential entry through an AI tool that records its input. Global `--config PATH` selects a different worker profile. The node need not be running, and existing nodes read changes on their next GUI or HTTP invocation.

Names contain 1..128 ASCII letters, digits, dots, underscores, or hyphens and begin with a letter or digit. Values contain 1..4096 UTF-8 bytes without NUL. Each profile stores at most 128 secrets and at most 1 MiB of encoded data. `list` returns names only. `delete` removes the named entry without returning its value.

Storage is `<dataDir>/secrets/values.json`, outside `workDir`. Mutations use a separate file lock and atomic replacement. Unix directories use `0700` and files `0600`; Windows relies on the profile directory's inherited ACLs. Storage is not encrypted and is not an OS keychain. Protect the worker account, filesystem, and backups. If `dataDir` is inside `workDir`, move the profile's state or workspace before adding secrets. Existing secret storage inside `workDir` prevents node startup and credential use, including paths resolved through directory symlinks.

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

This prevents accidental exposure through the supported credential-entry operations. It is not protection against an AI or application trying to extract a credential. Existing fleet membership, capability access rules, task ownership, leases, and maintenance admission still apply. Permission to invoke `rpa.run` or `http.request` includes permission to use the worker's stored secrets through that capability.

Unrestricted commands, filesystem access outside the filesystem API, custom helpers, malicious HTTP destinations, screenshots, application-managed files, and arbitrary value transformations can expose credentials. Masking covers configured values from the invocation's snapshot, not historical values deleted or rotated before a later inspection. Existing transcripts and retained outputs are not scrubbed retroactively. Do not use these secrets for gateway administration keys or assume they create an execution sandbox.
