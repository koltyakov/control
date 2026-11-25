# Streaming file uploads

[README](../README.md) · [Protocol](protocol.md) · [Streaming](streaming.md) · [Delegation](delegation.md) · [Testing](testing.md)

Upload a regular file from the machine running the CLI into a worker's existing workspace directory:

```sh
control upload start worker ./build.zip incoming/build.zip --id build-20261104
control upload status worker build-20261104
```

The remote path is relative to `workDir`, uses `/` separators on every platform, and must have an existing parent directory. Destinations are create-only: existing files, directories, and symlinks are never replaced, even if their content matches. Uploads do not preserve executable permissions, install software, or run the uploaded file. Symlinks, devices, stdin, and automatic retry are unsupported. Directory trees use `--recursive`; see [directory uploads](#directory-uploads).

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

Receiver network reads and writes have a 60-second idle timeout, including the initial reply, retained completion receipt, and wait for Start. Terminal-error replies have a separate five-second write limit. A stalled handshake releases its active-operation slot without publishing content.

`state: published` is a retained completion receipt, not a fresh assertion that nobody has subsequently changed or deleted the workspace file. Repeating a matching completed upload returns that receipt without sending payload or publishing again. An unknown or expired ID is not proof that publication never happened.

Abandon an inactive partial with:

```sh
control upload abort worker build-20261104
```

Abort never removes a published destination. Failed cleanup remains recorded and charged until deletion succeeds. Unresolved publication intents are preserved for recovery rather than expired or replayed automatically.

## MCP

`control_upload` takes required `node`, `id`, `localPath`, and `path`, plus optional `timeoutSeconds`. `localPath` is on the machine running the MCP server, not necessarily the AI application's host. `control_upload_resume` takes `node`, `id`, `localPath`, and optional `timeoutSeconds`; it uses the saved destination. `control_upload_status` and `control_upload_abort` take `node` and `id`.

Results contain metadata only: ID, immutable worker ID, destination, size, digest, state, durable offset, and timestamps. Transfer results add `resumedFrom` and `bytesSent`, the bytes this attempt wrote, also when it fails before publication. File bytes never enter tool arguments or results. MCP clients supplying a progress token receive durable checkpoint notifications. Request cancellation interrupts the attempt without starting a replacement.

MCP uploads single files only. `control_upload` and `control_upload_resume` refuse a directory `localPath` with a pointer to the CLI, because the tools have no explicit recursive opt-in. `control_upload_status` and `control_upload_abort` given a directory transfer ID return its [summary](#status-abort-and-recovery-records).

## Safety and publication

Uploads require account-client routing, with saved gateway login, and destination permission for `files.upload.open`; status and abort have separate permissions. Explicit `--api` or `CONTROL_API` is unsupported and fails before opening the source. Instruction-only System destinations receive fresh exact grants for each operation. Worker credentials and knowledge of an upload ID confer no upload authority. Grants are not persisted with partials, and revocation/expiry closes admitted streams. Active attempts hold maintenance admission through verification and publication.

Start uploads before acquiring an execution lease. The destination rejects every new open attempt with `node_leased` while a lease is effective, including resumes and retained-receipt requests from the lease owner. Use status for reconciliation during a lease; owner-authorized abort remains available. An already admitted attempt blocks lease acquisition through publication, completion replies and cleanup without being preempted. Inactive partials do not block leases. Request schemas are unchanged and accept no streaming `leaseId`. Older workers do not enforce this restriction. See [lease policy](protocol.md#shared-pool-leases).

`node.describe.fileUpload` advertises `file-upload-v1`, methods, configured budgets, and fixed protocol limits. Upgrade unsupported receivers; there is no fallback to shell, clipboard, another machine, or chunked JSON calls. No gateway upgrade or gateway upload endpoint is needed beyond the existing account-client/delegation requirements.

Bytes go into a private `.control-uploads` staging directory beside the destination, on the same filesystem. The receiver removes that directory when it cleans up the last partial in it, after publication, abort or expiry. A finished upload therefore leaves no private entry beside its file. Creating the directory and its first partial is serialized against that removal, so another upload's partial is never removed. Upload metadata lives in `dataDir/uploads`. Ordinary filesystem calls and artifact export reject these locations and existing symlink or Windows junction aliases, including paths with missing descendants. Remote paths remain confined through `os.Root`. This does not sandbox unrestricted commands running as the node user or prevent that user's programs from modifying workspace files.

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

Startup/periodic retention removes expired abandoned staging and old terminal receipts, skipping active records. Normal workspace files are never automatically removed by upload retention. Local recovery records are retained in the client's private state; their presence does not extend worker retention. A record is marked when this client sees its upload published and is removed seven days later, along with lock files left without a record; unpublished records are kept as the only evidence for a safe explicit resume.

## Directory uploads

Upload a local directory tree into a new directory on a worker:

```sh
control upload start worker ./project incoming/project --recursive --id project-20261124
control upload status worker project-20261124
control upload resume worker project-20261124 ./project --timeout 4h
```

`--recursive` is required, so a mistyped path cannot silently send a tree, and it requires `LOCAL_PATH` to be a directory. A directory transfer is client-side orchestration over the single-file protocol above: `files.mkdir` creates directories and each file is one ordinary create-only, verified, resumable `file-upload-v1` upload. There is no directory stream, archive or new peer method. The worker must advertise `file-upload-v1` and the `files.mkdir` capability; an older worker is refused before anything is created. See [D187](decisions.md#d187-directory-transfers-over-existing-upload-and-artifact-mechanisms).

### Scanning and validation

The client scans the whole tree before contacting the destination filesystem. It never follows symbolic links, including a symbolic link given as the local root. These entries are refused:

- symbolic links, devices, FIFOs, sockets and Windows reparse points other than ordinary directories;
- names the worker rejects: empty or over 255 bytes, containing `/ \ : < > " | ? *` or control characters, ending in a dot or space, Windows device names such as `CON`, `aux.txt` or `COM1`, the reserved `.control-uploads`, invalid UTF-8, or destination paths over 4,096 bytes;
- two names that differ only by case under Unicode simple case folding, such as `README` and `readme`. Windows and default macOS destinations cannot hold both. Collisions are always refused, never skipped.

A refusal reports the number of unsupported entries and the first ten paths, and sends nothing. `--skip-unsupported` skips them instead, and a skipped directory's contents with it. The result reports `skipped` and the first ten `skippedPaths`. Names that macOS treats as equal only after Unicode normalization (NFC and NFD forms) are not detected; on such a destination the second file fails with `destination_exists` and nothing is overwritten.

| Limit | Value |
| --- | ---: |
| Files per transfer | 512 |
| Directories per transfer | 512 |
| Each file | worker's advertised `maxUploadOwnerBytes` (4 GiB default) |
| Whole tree | worker's advertised `maxUploadStagingBytes` (16 GiB default), at most 1 TiB |
| Files uploading at once | 2, the worker's active uploads per owner |

Every limit is checked before anything is created or sent. The file limit is half of the worker's 1,024 retained upload records per owner: each published file keeps a receipt for `uploadRetentionHours`. If other uploads have consumed that budget, a file fails with `upload_storage_exhausted`; resume once older receipts expire. Another active upload by the same owner can make a file fail with `upload_limit`; resume after it finishes.

### Creation order and visibility

The remote root must not exist and its parent must. The client creates it with `files.mkdir` without `parents`, which fails if anything exists at that path, so a new transfer never merges into an existing directory. The local recovery record is written only after the root is created. When the worker replies that creation failed, for example because the root exists or its parent is missing, nothing was created and the error says so. If that creation's response is lost, the root may exist without a record, and the transfer reports state `unknown`: starting again with the same ID then fails because the root exists. Inspect it and choose another destination rather than reusing it blindly.

Next, the client creates every leaf directory with `parents`, which creates all directories, including empty ones, before any file. Files then upload in sorted path order. Each file appears atomically when its own upload publishes, but the tree as a whole is not atomic: other programs on the worker can see a partly uploaded tree. Do not start work on it until the transfer reports `state: published`. Executable permissions and timestamps are not preserved.

The first failed file stops new files from starting; files already in flight finish their own attempt. Failure, cancellation or abort never deletes remote files or directories. The error states how many files were published, and the printed summary shows the rest. `--timeout`, default one hour and at most 24 hours, bounds the whole attempt. Cancellation interrupts in-flight files at their last durable checkpoint.

### Resume

Resume uses the same ID and the local directory. It may be a different path, but it must contain the same names and sizes. The recovery record binds the worker identity, the remote root, the skip setting and the sorted names and sizes. Any difference fails with `source_changed` before anything is sent. Each file's content is bound by its own upload record when that file starts, exactly as for single files. Resume recreates missing directories with `parents`. A file this client saw published is not contacted again. A started file is resumed from its durable worker checkpoint. A file whose earlier open the worker refused before reserving anything, for example because of a lease, is opened again as new; that is still create-only. A file that already exists at a destination fails with `destination_exists` and is never replaced. Remove or rename the conflicting file, then resume. Resume within seven days: the client prunes a published file's record seven days after it saw the publication, and the worker drops receipts after `uploadRetentionHours`. After both, a file this transfer already published also reports `destination_exists`, because content equality is not proof of publication.

Each file's upload ID is `d` followed by 40 hexadecimal digits of SHA-256 over the transfer ID and its relative path. A resumed transfer therefore finds the same per-file records, and `control upload status NODE FILE_ID` inspects one file. Directory and single-file upload IDs are kept apart in local recovery state: a single-file upload refuses a directory transfer's ID and the reverse.

Leases apply per file, as for single files. A lease acquired between files makes the next open fail with `node_leased`, which stops the transfer cleanly. While the lease lasts, resume also fails, because `files.mkdir` is refused, before any file is sent. Resume after the lease ends.

Every `files.mkdir` and every file upload goes through its own permission check. Instruction-only destinations receive a fresh, exact, single-use grant for each call, exactly as for single files. There is no grant covering the tree.

### Status, abort and recovery records

A transfer attempt prints, and `control upload status NODE ID` and `abort` return, one compact summary, never one line per file. Status and abort recognize a directory transfer ID from the local recovery record. This attempt was stopped by a lease:

```json
{"kind": "directory", "id": "project-20261124", "nodeId": "…", "path": "incoming/project", "state": "incomplete",
 "directories": 14, "files": 120, "bytes": 52428800,
 "states": {"published": 80, "failed": 2, "pending": 38}, "filesAttempted": 82, "bytesSent": 31457280,
 "failures": [{"path": "src/main.go", "id": "d…", "state": "failed", "code": "node_leased", "error": "…"}]}
```

`state` is `published`, `incomplete`, or `aborted` after an abort. A new transfer refused before its remote root was created, for example by a scan refusal or an existing root, reports `not_started`; nothing was created, no recovery record exists, and the CLI prints only the error. `unknown` means the root's creation response was lost. `states` counts files by state: `published`; `staging` or `active` for a durable partial; `pending` for files never started; `failed` for an open refused or failed in this attempt; `aborted` or `expired`; and `unknown` when the worker has no record or the query failed. `unknown` is not proof that publication never happened; in status, a file refused before reservation also shows as `unknown`. `filesAttempted` and `bytesSent` appear only in transfer results. `bytesSent` counts file bytes this attempt wrote to the worker, including those of files interrupted before publication; the worker may hold fewer durably. `failures` lists at most ten files. Status reads published state from local records and queries the worker only for started, unpublished files, four at a time.

Abort abandons the staging of every started, unpublished file. It never removes published files or created directories. Aborted files cannot be resumed, so the transfer cannot complete afterwards: start a new transfer with a new ID and a new remote root.

The recovery record lives in the client's private state beside the single-file records, under `upload-dirs`. It is removed seven days after this client sees the whole transfer published. Incomplete transfers are kept as the only evidence for a safe resume. Directory transfers require the same account-client routing as single-file uploads.

## Directory export

`artifacts.export` with a directory `path`, for example `control artifact export worker results/run-42`, publishes one immutable artifact named `run-42.tar` holding a deterministic archive of the tree. It uses the ordinary import staging, `maxArtifactBytes` and `artifactReserveBytes`. A regular file `path` behaves as before. `node.describe.artifactExport` advertises `{"directories": "tar-v1", "maxEntries": 10000, "maxBytes": 1099511627776}`. Older workers lack that field and reject a directory path with an error rather than exporting something else. MCP `control_artifact_export` reaches the same method.

The `tar-v1` format is a POSIX tar stream written by Go's `archive/tar` in PAX format:

- one entry for each directory and regular file below the exported directory, but not the directory itself;
- names relative to the exported directory, slash-separated and UTF-8, with no leading `/` or `./` and no `..`; directories end with `/`;
- directories have typeflag `5` and mode `0755`; files have typeflag `0` and mode `0644`;
- uid and gid 0, empty user and group names, and modification time 0, the Unix epoch. No access or change times, extended attributes, ACLs, or link entries;
- a PAX extended header only for a name that a USTAR header cannot represent: one that is too long or not ASCII;
- entries sorted by the byte order of their relative path without the trailing `/`, so every directory precedes its contents;
- terminated by two zero blocks, without padding to a 10 KiB record.

Identical names and contents therefore produce an identical artifact ID on any operating system, regardless of timestamps or permissions. Content addressing returns the stored record, so the artifact keeps the name of its first export. Executable bits and timestamps are not preserved.

The export refuses the filesystem root, symbolic links, devices, FIFOs, sockets, Windows reparse points, non-UTF-8 names, private upload staging holding a partial anywhere in the tree, more than 10,000 entries, and more than 1 TiB of file data. An empty `.control-uploads` directory, such as one an earlier version left behind, holds no bytes and is skipped, so it does not change the artifact. Other spellings of that name are still refused. The tree is scanned first. A file that changes size or modification time while it is archived fails the export and publishes nothing. The archive is not a filesystem snapshot: unchanged files are checked individually.

Control does not extract archives; extraction is where path traversal bugs live. A consumer stages the artifact as a task input and extracts it with a tracked task, for example `tar -xf` on Linux, macOS, or Windows 10 and later. That task should still reject unexpected entry names.
