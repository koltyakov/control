# Local machine scenarios

[README](../README.md) · [Installation](installation.md#manage-registrations) · [GUI automation](rpa.md) · [Secrets](secrets.md)

Keep project-local machine scenarios in `.control-scenarios/ALIAS/`, using filenames without spaces, such as `connect-to-vpn.md`. These are local instructions and task templates, not a new execution capability or an automatic scenario runner. Store secret references, never credential values.

## Rename migration

`control machines rename OLD NEW` and the dashboard's rename action automatically migrate `.control-scenarios/OLD/` to `.control-scenarios/NEW/` in the client's current working directory. Stable-ID rename requests resolve the current alias through the authenticated fleet directory first. CLI and dashboard use the same shared client implementation.

Migration updates these references in Markdown and JSON files, including nested files:

- JSON string values under `node`, `target`, and `machine`, including JSON examples inside Markdown.
- Markdown `Machine: OLD` or `Machine: \`OLD\`` metadata.
- Unquoted machine arguments in Markdown `control call`, `exec`, `system`, `speedtest`, `tunnel`, `tunnel start`, `clipboard paste`, `task start|wait|get|logs|cancel|list`, `artifact get|export|deliver`, and `machines rename|enable|disable|unregister|forget` commands. `control.exe` is also recognized.

Secret references, task IDs, arbitrary prose, quoted command arguments, other JSON fields, and non-Markdown/non-JSON file contents are not rewritten. File names remain unchanged. Other machines' scenario folders are untouched. Review custom references outside these supported forms after a rename.

Run the client from the project containing its scenarios. This alias-based directory layout is project-local; keep each project's scenarios associated with the selected fleet. Migration does not search parent directories, other projects, other orchestrator hosts, or remote workspaces. Renames through direct gateway HTTP requests, other clients, or replacement enrollment do not migrate this host's files.

## Conflicts and recovery

Control takes a local migration lock and validates the source before sending the gateway rename. An existing destination blocks migration without merging or overwriting it. Case-only renames may reuse the same directory on case-insensitive filesystems. Symlinks and special files in the source, a symlink scenario root or lock, invalid JSON, and migration bounds violations also stop before submission. Missing scenario roots or source folders leave normal gateway rename behavior unchanged.

The source tree is limited to 1,024 entries. Markdown/JSON files are limited to 1 MiB each and 16 MiB combined. Other regular files move with the directory without being read into memory. Updated text files retain their permissions and are replaced atomically.

Local migration begins only after the gateway acknowledges the rename. A rejection or uncertain response leaves scenarios at their original paths and reports the error. Resolve the machine's stable ID before deciding what to do next; never automatically repeat an uncertain rename.

The gateway and local filesystem do not share a transaction. A process crash or local failure after gateway acknowledgment can leave the old folder, a moved folder with stale references, or partially updated references. Control reports local failures as an incomplete migration after a successful machine rename and does not roll the gateway back or retry it. Inspect the current alias and local files to finish recovery. Concurrent edits detected before a text replacement are preserved and reported rather than overwritten. The migration lock coordinates Control clients, not external editors.
