# Clipboard pastes

[README](../README.md) · [Protocol](protocol.md) · [Architecture](architecture.md) · [Testing](testing.md)

Paste the orchestrator machine's current clipboard onto a named node:

```sh
control clipboard paste worker
```

Text replaces the remote OS clipboard. Files copied in Finder, Explorer, or a supported Linux file manager stream into the node's `workDir`. Choose an existing subdirectory with `--dir`:

```sh
control clipboard paste worker --dir incoming
```

Reverse the direction to paste the remote clipboard onto the orchestrator machine:

```sh
control clipboard paste worker --reverse
control clipboard paste worker --reverse --dir ./downloads
```

In reverse mode, text replaces the local OS clipboard and files go into the selected local directory. The default directory is `.`. File pastes write files rather than putting file references onto the destination OS clipboard. They do not simulate a keyboard shortcut or paste text into the focused application.

The CLI deadline defaults to one hour. Use `--timeout 4h` for a longer transfer, up to 24 hours. Cancellation closes the stream. These are explicit, process-owned operations, not durable tasks or a background clipboard bridge. They work through a local node API or a standalone authenticated client, over WebRTC or the encrypted relay. Upgrade the receiving node and any local API node before use; unsupported versions fail without fallback or replay.

## MCP

`control_clipboard_paste` accepts required `node`, optional `reverse`, and optional `dir` with the same meanings. Its result reports `kind`, transferred file basenames, and byte count. It never returns clipboard text or source paths to the AI client. Only invoke it when the user asks to transfer their clipboard. A tool request's cancellation interrupts the transfer, and peer streams have the existing 24-hour maximum lifetime.

## Desktop requirements

The machine whose clipboard is read must have a logged-in desktop session. Text pastes also require clipboard access on the destination. A file destination needs only filesystem access, not a desktop.

- macOS uses Foundation through JavaScript for Automation to read Finder file URLs and text, and `pbcopy` to write text.
- Windows uses an STA PowerShell process and the desktop clipboard. Run the node in [user context](installation.md#switch-windows-to-user-login-startup), not as a LocalService system service.
- Linux uses `wl-paste`/`wl-copy` under Wayland or `xclip` under X11. File references use `text/uri-list`. `xsel` supports text only. The node must inherit the desktop's display environment.

Control does not install these tools, grant OS permissions, or choose another user's desktop. Native file-manager and clipboard behavior needs a smoke test on each platform. Headless or unavailable clipboards produce an error.

## File transfer and safety

Reading the clipboard obtains file references, not file bytes. Paste opens the selected regular files and exchanges basenames and sizes. The receiving side validates the existing destination directory and checks for collisions before allowing file streaming. Bytes then move in bounded buffers, with a SHA-256 digest after each file. The receiver publishes each verified file through an atomic, no-overwrite hard link. A filesystem without hard-link support reports an error rather than using an unsafe overwrite fallback.

Remote destinations are confined to `workDir` through `os.Root`, including symlink traversal checks. Clipboard source files may be outside `workDir`, because they are files explicitly selected on that desktop. The caller cannot supply arbitrary source paths through the protocol. Source paths do not travel to the receiving machine. Changes to source size or modification time fail the transfer; this is a live file read, not an immutable artifact snapshot. Use [artifacts](protocol.md#node-operations) for retained, resumable transfers.

Limits are 1 MiB of UTF-8 text, 128 regular files, 1 TiB total file bytes, and eight simultaneous clipboard streams per node. Portable basenames must be unique without regard to case. Directories, symlinks, device files, images, other rich clipboard formats, and remote file URIs are rejected. Empty files and filenames containing spaces are supported. A copied cut operation remains a copy; source files are never deleted.

Interrupted or corrupt files never replace destination entries, and their temporary files are removed. Files completed earlier in a multi-file paste remain if a later file fails. A lost completion response may mean the paste already succeeded. Inspect the destination before explicitly trying again; Control never retries a paste automatically. There is no background polling, automatic resume, or Finder/Explorer paste hook.

## Permissions

`clipboard.paste` permits changing the node user's clipboard or writing pasted files within its workspace. `clipboard.open` permits reading that user's clipboard and streaming its selected files, including outside the workspace. Both enforce the existing fleet membership and access rules. `node.describe` reports the `clipboard-v1` protocol and both methods. Nodes trust their fleet by default; restrict these permissions on machines with sensitive desktop data:

```json
{
  "allow": {
    "ORCHESTRATOR_STABLE_ID": ["node.describe", "clipboard.open", "clipboard.paste"]
  }
}
```

Clipboard text and file bytes can contain passwords or other private information. Transfers are encrypted, but clipboard contents are not secret-masked. Do not use this to retrieve [worker-local secrets](secrets.md). Results and activity records exclude clipboard text; text is still present in the authorized paste request and destination clipboard. Clipboard streams hold maintenance admission until closed so managed updates do not interrupt a paste.
