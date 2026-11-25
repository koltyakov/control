<!-- Managed by control -->

# Uploads, artifacts and clipboard pastes

## Upload local files

Upload before acquiring an execution lease. New opens, resumes and receipt-open requests require an unleased destination, even for the lease owner. Status and owner-authorized abort remain available during leases; inactive partials do not block lease acquisition. An already admitted upload blocks acquisition until its publication/reply/cleanup finishes and is not preempted. Older workers do not enforce this restriction; see the protocol lease policy.

For a regular file on this orchestrator, use `control upload start WORKER LOCAL_PATH REMOTE_PATH --id UNIQUE_ID` or MCP `control_upload` with `node`, explicit `id`, `localPath`, and `path`. The MCP source path is on the machine running the MCP server. Discover `node.describe.fileUpload` first. Remote paths use `/` separators relative to `workDir` and require an existing parent directory. Uploads stream binary bytes through one bulk connection, verify SHA-256, and use atomic no-overwrite publication. They do not install or execute the uploaded file. No clipboard access or local enrolled node is needed; explicit local API routing is unsupported.

Do not transfer bulk files using repeated base64 `files.write` calls or one CLI process per chunk. Each process creates a new transport identity and handshake; the gateway defaults to 120 new identities per minute per account. Larger chunks only reduce that churn, not reuse the connection. Use the upload tool rather than raising limits or restarting services.

After interruption or an uncertain reply, inspect `control upload status WORKER ID` or `control_upload_status`. If published, do not send again. If inactive and partial, explicitly use `control upload resume WORKER ID LOCAL_PATH` or `control_upload_resume` with the same unchanged source. Resume verifies the durable prefix and pins the original worker identity. Never invent a new ID to replay uncertain publication, switch workers, or fall back to System or a shell. An unknown/expired ID does not prove the file was never published. Keep the client's private recovery state. `control upload abort WORKER ID` or `control_upload_abort` abandons an inactive partial but never deletes a published file. Default attempt timeout is one hour, configurable up to 24 hours. See https://github.com/koltyakov/control/blob/main/docs/uploads.md for quotas and filesystem limitations.

## Upload a directory tree

Use the CLI: `control upload start WORKER LOCAL_DIR REMOTE_DIR --recursive --id UNIQUE_ID`. MCP `control_upload` refuses a directory; do not archive by hand with `exec.run tar` instead. The remote directory must not exist and its parent must. The client refuses symlinks, special files, names the worker rejects and names that differ only by case, listing them and sending nothing; add `--skip-unsupported` only when the user accepts skipping them. Limits are 512 files and 512 directories, each file within the worker's `maxUploadOwnerBytes` and the tree within `maxUploadStagingBytes`; split larger trees. Each file is a separate create-only upload, so the tree is not atomic: until the summary reports `state: published`, the worker holds a partial tree. Nothing is deleted on failure. After interruption, inspect `control upload status WORKER ID` (or `control_upload_status`), then explicitly run `control upload resume WORKER ID LOCAL_DIR`; it skips published files and fails if the local names or sizes changed. A `destination_exists` file is never replaced: ask the user before removing the conflicting file. Abort abandons unpublished partials and makes the transfer unresumable.

## Move results directly

List a node's artifacts with `control artifact list NODE` or MCP `control_artifacts`: newest-first compact pages with total count and bytes. Continue with `nextCursor`; `--all` prints every full reference.

Export files from their source with `control artifact export SOURCE PATH`, then request recipient-bound access with `control call SOURCE artifacts.grant '{"id":"ARTIFACT_ID","target":"DESTINATION"}'`. Pass the granted reference, including `grant`, as a task input so the destination can fetch it directly. When `node.describe.artifactExport.directories` is `tar-v1`, exporting a directory produces one deterministic `NAME.tar` artifact; it refuses symlinks and special files and holds at most 10,000 entries. Control never extracts it: extract with a tracked task on the consumer and check the entry names. Older workers reject a directory path. Planned workflow inputs prepare their signed grants automatically. Use `control artifact deliver SOURCE ARTIFACT_ID DESTINATION` for peer-to-peer delivery. Download through the host only when the user needs a local copy:

```sh
control artifact get worker ARTIFACT_ID ./result.bin
```

## Paste a clipboard

Both clipboard methods require an unleased serving node, including the lease owner and reverse reads. Perform the transfer before acquiring or after releasing that node's execution lease. An admitted clipboard operation blocks acquisition through completion/cleanup. There is no streaming leaseId or automatic retry/fallback. A node merely relaying bytes is not lease-sensitive; artifact and TCP access retain their existing exemptions.

Only transfer a clipboard when the user requests it. Use `control clipboard paste NAME` or MCP `control_clipboard_paste` with `node`. Text replaces the remote OS clipboard; copied regular files stream into the node's workspace when the call is made. `--dir incoming` chooses an existing directory relative to `workDir`. With `--reverse` or MCP `reverse: true`, the remote clipboard is the source and `dir` is an existing local directory. Results report only basenames and byte counts, never clipboard text. Do not inspect clipboard text through generic calls or use clipboard transfer to retrieve secrets.

Both desktops need clipboard access for text; a file destination needs only filesystem access. Discover the `clipboard` protocol field in `node.describe`, and require `clipboard.paste` permission or `clipboard.open` for reverse. There is no background synchronization or native Finder/Explorer paste hook. Directories, symlinks, images, and rich formats are unsupported. Source files are not deleted, existing destinations are not overwritten, and completed files can remain after a later failure. An uncertain paste may already have succeeded; inspect destination metadata rather than retrying automatically. Use artifacts for durable, resumable transfers.
