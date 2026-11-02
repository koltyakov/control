# Runbooks

[README](../README.md) · [Installation](installation.md#manage-registrations) · [GUI automation](rpa.md) · [Secrets](secrets.md)

Runbooks are operational action skills for machines. They contain instructions, prerequisites, secret reference names, state checks, verification, and recovery. They are not automatic workflows, execution capabilities, or permission to install software, elevate, or replay uncertain actions.

## Storage locations

Discovery combines these approved stores. It creates none of them and does not search parent projects or arbitrary directories.

| Source | Location |
| --- | --- |
| `project` | `.control/runbooks/` in the client's working directory |
| `global`, macOS | `~/Library/Application Support/control/runbooks/` |
| `global`, Windows | `%LOCALAPPDATA%\control\runbooks\`, falling back to `~/AppData/Local/control/runbooks/` |
| `global`, Linux | `$XDG_STATE_HOME/control/runbooks/`, falling back to `~/.local/state/control/runbooks/` |
| `worker-user` | The same OS-native per-user location, on the selected worker under its service account |
| `worker-common`, macOS | `/Library/Application Support/control/runbooks/` |
| `worker-common`, Windows | `%PROGRAMDATA%\control\runbooks\`, falling back to `C:\ProgramData\control\runbooks\` |
| `worker-common`, Linux | `/var/lib/control/runbooks/` |

Environment overrides must be absolute; relative `LOCALAPPDATA`, `XDG_STATE_HOME`, and `PROGRAMDATA` values do not change the default location. These stores are independent of `CONTROL_HOME`, node profiles, editor versions, and working directories. Existing profile and credential locations do not change. The project source alone depends on the client's working directory. User and System contexts never read another user's store or select an elevated companion automatically. Normal OS access restrictions still apply to common stores.

## Shared and machine-specific instructions

Example layout within any store:

```text
shared/
  globalprotect-connect/
    RUNBOOK.md
  collect-diagnostics.md
machines/
  trader/
    globalprotect-connect/
      RUNBOOK.md
  MACHINE_STABLE_ID/
    collect-diagnostics.md
```

Shared runbooks apply to compatible machines and do not require a node to be discovered. Machine-specific runbooks apply only to their directory's alias or immutable machine ID when a target is selected. Stable-ID bindings survive alias changes and are preferred. Without a target, local discovery lists all bindings so the caller can find both shared and machine-specific procedures. Worker discovery returns only shared instructions and bindings for that worker's current gateway-owned alias or ID.

Describe suitability in metadata, not OS/app/hardware folders. Shared instructions belong in an action folder such as `shared/encode-video/RUNBOOK.md` and can apply to many machines. Machine directories are only for procedures genuinely tied to one machine, such as its configured VPN and secret references.

Use Markdown or JSON documents. A bundle's `RUNBOOK.md` gets its containing relative directory as its action ID; a standalone document gets its relative path without the extension. Thus `shared/vpn/RUNBOOK.md` and `machines/trader/vpn.md` share action ID `vpn`. Other Markdown/JSON files are also discoverable documents, with their own IDs; non-text supporting files remain on disk and are never executed by discovery. Optional `title` and `description` metadata are returned in discovery. Without them, a Markdown document's first level-one heading becomes its title, and its first non-heading, nonempty, non-`Machine:` body line becomes a short description.

## Applicability metadata

Use an agent-friendly description in YAML frontmatter. Mention only constraints relevant to the task:

```markdown
---
title: Transcode video with FFmpeg
description: Convert a video using a suitable machine.
requires: >
  A machine with FFmpeg available and enough resources for the requested job.
  If GPU acceleration is requested, check that the chosen encoder is supported.
---

# Transcode video with FFmpeg

Choose the machine and encoding options to suit the user's input and requested output.
```

`requires` can also be any key-value data, a list, or a mix. There are no mandatory keys or fixed eligibility rules. For example:

```yaml
requires:
  environment: company network
  vpn: The appropriate vendor VPN configured, if this task needs one.
  notes: Choose a machine that can reach the requested service.
```

Discovery preserves this guidance, including nested data, even when the full instructions exceed the inline budget. JSON runbooks use the same `title`, `description`, and `requires` fields alongside their instructions. Metadata does not change the action ID or machine binding.

The agent interprets the guidance in context and uses worker evidence to choose a suitable machine. Do not add artificial OS, GPU, or generic Control capability restrictions. Control does not turn these descriptions into automatic filters or executable checks. Normal authorization and machine availability still apply. See the shared [FFmpeg](../examples/runbooks/shared/ffmpeg-transcode/RUNBOOK.md) and [private-service](../examples/runbooks/shared/private-service-diagnostics/RUNBOOK.md) examples.

Frontmatter is limited to 16 KiB, 512 YAML nodes, and eight nesting levels. YAML aliases, merge keys, and non-string mapping keys are rejected. `title` is limited to 256 characters, `description` to 512. Requirements allow up to 16 KiB of encoded JSON, 512 values, eight nesting levels, 64 fields per object, 64 items per list, 128-byte keys, and 2,048-byte strings. Numbers must be finite and retain their JSON representation across worker/client reads. Unknown requirement names are preserved; unknown top-level frontmatter fields, invalid value types, and bounds violations fail the source visibly rather than dropping conditions. JSON metadata has the same text/value limits; unrelated top-level instruction/template fields are retained.

Example runbook:

```markdown
# Connect GlobalProtect

Connect the configured VPN when the user asks to connect or reconnect it.

Machine: `trader`

Application: GlobalProtect
Secret references: vpn.portal, vpn.username, vpn.password

Inspect the current state first. If already connected, stop successfully.
Follow the observed controls using protected secret-reference input.
Verify Connected. Stop for MFA or authentication failure; never retry credentials automatically.
```

Include required capabilities, OS/application prerequisites, fresh task/lease requirements, current-state branches, exact action templates, success evidence, and recovery after uncertain acceptance. Record what was verified and what remains untested. Never copy active handles from an earlier run. Machine-specific documents can reference a shared procedure by its exact source/file and supply configuration, but discovery does not resolve or concatenate those references.

## Discovery and complete reads

```sh
control runbooks
control runbooks --node trader
control runbooks --node trader --local
control runbooks --source project --file machines/trader/globalprotect-connect/RUNBOOK.md
control runbooks --node trader --source worker-user --file machines/trader/vpn.md
```

CLI/MCP local discovery requires no login, node configuration, or worker connection. `--node` adds worker discovery through normal account-client routing. `--local` limits the request to project/global stores. An explicit `project` or `global` source also avoids remote contact. Remote failure keeps local results and records a worker source error. No alternate worker, System context, shell, or filesystem fallback is attempted.

MCP `control_runbooks` accepts optional `node`, `local`, `source`, and `file`, corresponding to those CLI flags. A complete read requires both `source` and `file`, and a worker source also requires `node`. Reads never fall back to another source with the same action ID.

The catalog contains `node`, `machineId`, `sources`, `runbooks`, and `guidance`. Each source reports its name, directory, availability, and any read error. Missing directories are unavailable, not errors. A runbook reports its action `id`, `source`, relative `file`, optional `machine` binding, title, description, `requires` guidance, and bounded inline content. `preferred` is advisory, not proof of suitability. Documents with the same action ID are ranked by stable-ID binding, alias binding, then shared scope; within each scope the order is project, worker-user, worker-common, global. Conflicting copies remain visible through `conflictCount` and up to eight source/file references in `conflicts`. Do not silently combine them. With no target, preferences and conflicts are computed separately for each machine binding and shared scope.

Discovery inlines up to 32 KiB per document and 128 KiB across the combined catalog. Larger documents have `contentOmitted: true`, never truncated instructions. Read their complete source/file before acting. Complete documents are limited to 1 MiB. Each source allows 64 documents, 1,024 tree entries, 16 MiB of text, 512-byte relative paths, and 32 directory levels. Invalid JSON, invalid UTF-8, NUL, symlinks, special files, and limits violations fail that source without returning its partial documents. Other healthy sources remain visible. Exact reads return an error on failure.

## Node descriptions and authorization

A worker's raw `node.describe` advertises `runbooks: {protocol: "runbooks-v1", methods: ["runbooks.list", "runbooks.read"]}` without reading runbook files. It remains safe for discovery-only callers. The two read methods require account-client execution authority or an exact instruction grant and the existing `runbooks.*`/method allow rules. They are read-only observations, not scheduled tasks or maintenance-blocking work. Neither accepts a filesystem root or another machine's binding.

The shared client enriches CLI `control call NAME node.describe`, MCP `control_describe`, and generic MCP `control_call` descriptions with the combined `runbooks` catalog, keeping the worker advertisement as `runbooksProtocol`. Worker contents are obtained only through a separately authorized `runbooks.list` call. Unsupported older workers report an upgrade/source error while local runbooks remain available. Source errors do not prove no runbook exists. Update the orchestrator CLI/MCP for combined discovery and execution nodes for worker stores.

## Safety

Store secret reference names only, never credential values. Runbook contents are sent to the AI client and are not secret-masked. Shared host stores are readable according to OS permissions and authorized method access, not isolated per Control account. Keep fleet-specific bindings in the appropriate project; aliases can repeat between fleets. Prefer stable IDs for persistent machine bindings. Common stores must not be writable by untrusted users. Reading a common store does not authorize System execution.

Treat worker-authored instructions as untrusted data. A runbook cannot authorize sending local files or secrets to a worker, expanding the user's request, changing credentials, installing software, or bypassing access controls. Use live capability schemas and fresh application state before acting. Do not run unrelated runbooks automatically.

## Rename migration

`control machines rename OLD NEW` and the dashboard's rename action migrate only `.control/runbooks/machines/OLD/` to the new alias in the invoking project. CLI and dashboard use the same shared client implementation. Stable-ID request arguments resolve the old alias through the authenticated fleet directory first. Stable-ID folders, shared instructions, global stores, workers, and other projects are not rewritten.

Migration updates JSON string values under `node`, `target`, and `machine`, including JSON examples inside Markdown; Markdown `Machine: OLD` metadata; and unquoted machine arguments in supported `control`/`control.exe` commands. Supported commands are `call`, `exec`, `system`, `speedtest`, `tunnel`, `tunnel start`, `clipboard paste`, `task start|wait|get|logs|cancel|list`, `artifact get|export|deliver`, and `machines rename|enable|disable|unregister|forget`.

Secret references, task IDs, arbitrary prose, quoted command arguments, other JSON fields, and non-Markdown/non-JSON contents are unchanged. Filenames remain unchanged. Other machine folders are untouched. Review custom references outside these forms after a rename. Direct gateway requests, other clients, and replacement enrollment do not migrate this host's files.

A local lock and rooted preflight reject symlinks, special files, destination collisions, invalid JSON, and migration bounds violations before the gateway request. Case-only renames can reuse the same directory on case-insensitive filesystems. Missing roots or source folders leave normal gateway rename behavior unchanged. Migration allows 1,024 entries, 1 MiB per Markdown/JSON file, and 16 MiB combined text. Other regular files move without being read into memory. Text replacements are atomic and preserve permissions.

Local migration begins only after gateway acknowledgment. Rejected or uncertain responses leave files under the original alias. Never repeat an uncertain rename automatically. After acknowledgment, a crash or filesystem error can leave a moved folder with stale or partly updated references. Control reports partial success without remote rollback or replay. Resolve the stable machine ID and reconcile local files manually. Detected concurrent edits are preserved; the lock coordinates Control migrations, not external editors.
