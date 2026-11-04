# Streaming file uploads

[README](../README.md) · [Protocol](protocol.md) · [Streaming](streaming.md) · [Delegation](delegation.md) · [Testing](testing.md)

Upload a regular file from the machine running the CLI into a worker's existing workspace directory:

```sh
control upload start worker ./build.zip incoming/build.zip --id build-20261104
control upload status worker build-20261104
```

The remote path is relative to `workDir`, uses `/` separators on every platform, and must have an existing parent directory. Destinations are create-only: existing files, directories, and symlinks are never replaced, even if their content matches. Uploads do not preserve executable permissions, install software, or run the uploaded file. Directories, symlinks, devices, stdin, and automatic retry are unsupported.

One attempt uses one client identity and one bulk stream, regardless of the number of blocks. Do not build uploads by starting one CLI process per `files.write` chunk. That repeats handshakes, permanent identity bindings, base64 conversion, and disk syncs, and can exhaust the gateway's identity-rate allowance. Raising gateway limits is not required for streaming uploads.

## Resume and reconciliation

The CLI prints an upload ID before sending work. Omit `--id` to generate one. Choose a new unique ID for genuinely new work; never choose a new ID merely because a response was lost.

After interruption, inspect status and explicitly resume using the same ID and unchanged source:

```sh
control upload status worker build-20261104
control upload resume worker build-20261104 ./build.zip --timeout 4h
```

The worker stores a durable contiguous byte offset and its SHA-256. It discards any speculative tail, rehashes that prefix, and requires the sender to verify the same local prefix before sending a suffix. Corrupt or short partials fail rather than silently restarting. The full local source is hashed before each attempt, and the worker hashes the complete staged file from disk before publication. The source is a live open file, not a filesystem snapshot; size, modification metadata, and the transmitted digest are checked before requesting publication.

Upload state belongs to the stable client owner, not an individual CLI/MCP process or everyone sharing the account key. Private local recovery records retain the immutable worker ID, destination, size, digest, and local source path. Keep the client state directory to resume across processes. Resume cannot silently follow a machine name to a replacement worker. The local source path never travels to the worker.

Default attempt timeout is one hour; `--timeout` accepts one second through 24 hours. Cancellation closes the attempt and preserves the last acknowledged checkpoint. Status can briefly report `active: true` while the receiver releases the stream; wait for it to become inactive before resuming or aborting. Keepalives and status reads do not renew partial retention.

`state: published` is a retained completion receipt, not a fresh assertion that nobody has subsequently changed or deleted the workspace file. Repeating a matching completed upload returns that receipt without sending payload or publishing again. An unknown or expired ID is not proof that publication never happened.

Abandon an inactive partial with:

```sh
control upload abort worker build-20261104
```

Abort never removes a published destination. Failed cleanup remains recorded and charged until deletion succeeds. Unresolved publication intents are preserved for recovery rather than expired or replayed automatically.

## MCP

`control_upload` takes required `node`, `id`, `localPath`, and `path`, plus optional `timeoutSeconds`. `localPath` is on the machine running the MCP server, not necessarily the AI application's host. `control_upload_resume` takes `node`, `id`, `localPath`, and optional `timeoutSeconds`; it uses the saved destination. `control_upload_status` and `control_upload_abort` take `node` and `id`.

Results contain metadata only: ID, immutable worker ID, destination, size, digest, state, durable offset, and timestamps. Transfer results add `resumedFrom` and `bytesSent`. File bytes never enter tool arguments or results. MCP clients supplying a progress token receive durable checkpoint notifications. Request cancellation interrupts the attempt without starting a replacement.

## Safety and publication

Uploads require account-client routing, with saved gateway login, and destination permission for `files.upload.open`; status and abort have separate permissions. Explicit `--api` or `CONTROL_API` is unsupported and fails before opening the source. Instruction-only System destinations receive fresh exact grants for each operation. Worker credentials and knowledge of an upload ID confer no upload authority. Grants are not persisted with partials, and revocation/expiry closes admitted streams. Active attempts hold maintenance admission through verification and publication.

`node.describe.fileUpload` advertises `file-upload-v1`, methods, configured budgets, and fixed protocol limits. Upgrade unsupported receivers; there is no fallback to shell, clipboard, another machine, or chunked JSON calls. No gateway upgrade or gateway upload endpoint is needed beyond the existing account-client/delegation requirements.

Bytes go into a private `.control-uploads` staging directory beside the destination, on the same filesystem. Upload metadata lives in `dataDir/uploads`. Ordinary filesystem calls and artifact export reject these locations and existing symlink aliases. Remote paths remain confined through `os.Root`. This does not sandbox unrestricted commands running as the node user or prevent that user's programs from modifying workspace files.

After explicit `Finish`, the receiver syncs and rehashes the complete staged file, records publication intent, and atomically hard-links it into the final name without overwrite. It persists the completion receipt before removing the redundant staging link and returning success. A filesystem without hard-link support fails rather than copying into a visible partial destination. A lost reply after publication can be reconciled using the retained staging identity and journal. Recovery never creates a final name automatically, and content equality alone is not evidence that this upload published it.

Unix directory entries are synced before acknowledging publication. Windows has atomic visibility and process-restart reconciliation, but this implementation does not promise directory-entry durability through power loss. Native filesystem behavior must be verified on the target OS; a cross-build is not a runtime test.

## Limits and storage

| Limit | Default |
| --- | ---: |
| Binary data record | 256 KiB plus an 8-byte offset |
| Metadata record | 16 KiB |
| Maximum declared file | 1 TiB, additionally bounded by quotas |
| Active operations | 8 per node, 2 per owner |
| Incomplete/uncleaned records | 256 per node, 64 per owner |
| All retained records | 4,096 per node, 1,024 per owner |
| `maxUploadStagingBytes` | 16 GiB |
| `maxUploadOwnerBytes` | 4 GiB |
| `uploadReserveBytes` | 256 MiB per involved filesystem |
| `uploadRetentionHours` | 168 hours |

Zero configuration values select defaults; negative byte limits and invalid retention fail startup. Reserve the declared size against staging quotas before accepting an upload. Active and abandoned partials, plus failed cleanup, remain charged. Published workspace files do not consume the staging budget. Upload and artifact budgets are separate.

Checkpoints are synced after 8 MiB or two seconds, checked between incoming records, and at finish. Real free-space checks run at admission and approximately every MiB, including the metadata filesystem. External writers can still exhaust physical disk; the logical quota is not an OS disk reservation.

Streaming uses a reusable 256 KiB application buffer at each endpoint, with fixed framing overhead, rather than buffering the file or queuing chunks. Existing TLS, yamux, carrier, and relay buffers remain separately bounded. Hashing and rehashing use bounded buffers but require linear disk reads. The current accepted-session receive window remains unchanged.

Startup/periodic retention removes expired abandoned staging and old terminal receipts, skipping active records. Normal workspace files are never automatically removed by upload retention. Local recovery records are retained in the client's private state; their presence does not extend worker retention.
