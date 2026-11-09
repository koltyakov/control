<!-- Managed by control -->

# Uploads, artifacts and clipboard pastes

## Upload local files

For a regular file on this orchestrator, use `control upload start WORKER LOCAL_PATH REMOTE_PATH --id UNIQUE_ID` or MCP `control_upload` with `node`, explicit `id`, `localPath`, and `path`. The MCP source path is on the machine running the MCP server. Discover `node.describe.fileUpload` first. Remote paths use `/` separators relative to `workDir` and require an existing parent directory. Uploads stream binary bytes through one bulk connection, verify SHA-256, and use atomic no-overwrite publication. They do not install or execute the uploaded file. No clipboard access or local enrolled node is needed; explicit local API routing is unsupported.

Do not transfer bulk files using repeated base64 `files.write` calls or one CLI process per chunk. Each process creates a new transport identity and handshake; the gateway defaults to 120 new identities per minute per account. Larger chunks only reduce that churn, not reuse the connection. Use the upload tool rather than raising limits or restarting services.

After interruption or an uncertain reply, inspect `control upload status WORKER ID` or `control_upload_status`. If published, do not send again. If inactive and partial, explicitly use `control upload resume WORKER ID LOCAL_PATH` or `control_upload_resume` with the same unchanged source. Resume verifies the durable prefix and pins the original worker identity. Never invent a new ID to replay uncertain publication, switch workers, or fall back to System or a shell. An unknown/expired ID does not prove the file was never published. Keep the client's private recovery state. `control upload abort WORKER ID` or `control_upload_abort` abandons an inactive partial but never deletes a published file. Default attempt timeout is one hour, configurable up to 24 hours. See https://github.com/koltyakov/control/blob/main/docs/uploads.md for quotas and filesystem limitations.

## Move results directly

Export files from their source with `control artifact export SOURCE PATH`, then request recipient-bound access with `control call SOURCE artifacts.grant '{"id":"ARTIFACT_ID","target":"DESTINATION"}'`. Pass the granted reference, including `grant`, as a task input so the destination can fetch it directly. Planned workflow inputs prepare their signed grants automatically. Use `control artifact deliver SOURCE ARTIFACT_ID DESTINATION` for peer-to-peer delivery. Download through the host only when the user needs a local copy:

```sh
control artifact get worker ARTIFACT_ID ./result.bin
```

## Paste a clipboard

Only transfer a clipboard when the user requests it. Use `control clipboard paste NAME` or MCP `control_clipboard_paste` with `node`. Text replaces the remote OS clipboard; copied regular files stream into the node's workspace when the call is made. `--dir incoming` chooses an existing directory relative to `workDir`. With `--reverse` or MCP `reverse: true`, the remote clipboard is the source and `dir` is an existing local directory. Results report only basenames and byte counts, never clipboard text. Do not inspect clipboard text through generic calls or use clipboard transfer to retrieve secrets.

Both desktops need clipboard access for text; a file destination needs only filesystem access. Discover the `clipboard` protocol field in `node.describe`, and require `clipboard.paste` permission or `clipboard.open` for reverse. There is no background synchronization or native Finder/Explorer paste hook. Directories, symlinks, images, and rich formats are unsupported. Source files are not deleted, existing destinations are not overwritten, and completed files can remain after a later failure. An uncertain paste may already have succeeded; inspect destination metadata rather than retrying automatically. Use artifacts for durable, resumable transfers.
