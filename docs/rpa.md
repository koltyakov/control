# GUI automation

[README](../README.md) · [Protocol](protocol.md) · [Installation](installation.md) · [Testing](testing.md)

For remote UI work, prefer the opt-in [native agent-desktop adapter](desktop.md) when `desktop.run` is advertised. It supports secret-reference input and masked observations without Python. The ordinary [agent-desktop CLI](https://github.com/vercel-labs/agent-desktop) can also run through `exec.run`, but its output is not secret-masked.

`rpa.run` lets an AI agent inspect and manipulate a worker's logged-in desktop. It is absent unless the node configuration includes an `rpa` helper command. Control remains CGO-free; the desktop helper uses installed Python libraries. No gateway or transport changes are needed.

## Use agent-desktop on a worker

Discover the worker with `node.describe`, then check the binary and load its version-matched instructions on that same machine:

```sh
control exec worker -- agent-desktop version
control exec worker -- agent-desktop skills get desktop
control exec worker -- agent-desktop skills get platform
control exec worker -- agent-desktop status
```

MCP uses `control_call` with `node: "worker"`, `method: "exec.run"`, and `params: {"command":"agent-desktop","args":["version"]}`. Change the argument array for the remaining commands. Read the returned skills' `data.content` before acting. A local installation does not establish remote availability. Keep subsequent commands routed through Control to that worker, using structured argument arrays and fresh snapshot-qualified refs. Follow the installed skill's observation and delivery rules; never replay uncertain effects. Physical input with `--headed` needs explicit user permission, not an automatic fallback after a semantic failure.

`version` checks presence, not desktop readiness. `status` reports platform and permissions. The worker still needs the logged-in user's graphical session and upstream OS dependencies and permissions. Do not switch to System to recover GUI access. Missing executables, execution denial, unavailable sessions, and missing OS grants must be reported rather than bypassed. If absent, use an already configured `desktop.run` or `rpa.run` when suitable, or ask for setup. Control does not automatically install agent-desktop or prompt for permissions.

When a GUI task needs missing protected input, suggest the [native adapter setup](desktop.md#enable-on-a-worker) and ask for approval. It uses the public upstream FFI archive and a verified checksum, without installing the CLI or Python. Control releases do not bundle these dependencies. If the ordinary CLI is needed instead, install it on the worker with approval using upstream instructions. For an agent harness that supports skill installation, upstream also provides:

```sh
npx skills add https://github.com/vercel-labs/agent-desktop --skill agent-desktop
```

This optional harness skill does not install the worker's binary. Reading the embedded desktop and platform skills through Control needs no harness installation. Restart or reload the intended harness when required by its installer.

### Login without credentials in agent context

Use [worker-local secret references](secrets.md#fill-application-fields-by-reference) for both private usernames and passwords. The user enters them on the worker with `control secrets set app.username` and `control secrets set app.password`, outside chat. Prefer [native `desktop.run`](desktop.md) with `setValue`, a ref from its private snapshots, and `secret` instead of `text`. The worker resolves values into an anonymous stdin pipe to the native helper and masks subsequent observations. If using `rpa.run` instead, inspect its selectors; agent-desktop refs are not RPA selectors. Tool arguments and durable task specifications contain names only.

Agent-desktop's literal CLI `type`/`set-value` commands and ordinary `exec.run` do not resolve Control secret references. Raw command output is not secret-masked. Do not read the secret store, construct shell credential substitutions, or send credentials in tool arguments, environment, clipboard, scripts, or agent prompts. If neither protected adapter is available, stop for manual login or explicitly approved helper setup.

While credentials may be visible, use masked `desktop.run` snapshots or RPA inspection instead of raw CLI snapshots, field reads, screenshots, clipboard reads, or traces. Do not enable password reveal or credential-bearing recording. Verify login by non-sensitive application state and resume raw CLI observations only after credentials are no longer visible. Text masking is not a sandbox and cannot protect screenshots, arbitrary transformations, or unrestricted execution. See [what secrets protect](secrets.md#what-this-protects).

Control's RPA lock does not cover external agent-desktop commands. Coordinate one writer; neither tool excludes human input or unrelated desktop software.

## Install the helper

Copy the entire [examples/rpa](../examples/rpa) directory to a permanent location on the worker. Use Python 3.11 or newer, preferably in a dedicated virtual environment, and install its [requirements](../examples/rpa/requirements.txt):

```sh
python3 -m venv /absolute/path/to/rpa-env
/absolute/path/to/rpa-env/bin/python -m pip install -r /absolute/path/to/rpa/requirements.txt
```

On Windows, create the environment with `py -3 -m venv C:\Tools\rpa-env`, then run `C:\Tools\rpa-env\Scripts\python.exe -m pip install -r C:\Tools\rpa\requirements.txt`.

| Platform | Accessibility backend | Desktop requirements |
| --- | --- | --- |
| Windows | pywinauto, UI Automation | Run the node under the intended logged-in user, not LocalService. Session 0 is rejected. Elevated applications may deny access to a limited helper. |
| macOS | PyObjC, Accessibility AX APIs | Grant Accessibility permission to the helper/node's responsible application. Screenshots also require Screen Recording permission. Restart after granting access. |
| Linux | pyatspi, AT-SPI | Install distro `python3-pyatspi`, enable application accessibility, and supply the desktop session's D-Bus environment. Coordinate input and screenshots require X11 and `DISPLAY`; Wayland is rejected for those actions. |

For Debian/Ubuntu Linux, install `python3-pyatspi python3-tk scrot gnome-screenshot` through the OS package manager. Create the virtual environment with `--system-site-packages` so it can import distro pyatspi. PyAutoGUI/PyScreeze needs a working screenshot utility. An SSH shell or user service may lack the desktop's environment even when the user is logged in. Set it in the service configuration or launch the node from that desktop. Do not enable GUI access in a headless container and expect an interactive user's screen.

Windows startup can be changed explicitly with `control service start --mode user`; [system-to-user migration](installation.md#switch-windows-to-user-login-startup) requires Administrator PowerShell under the intended user. This interrupts running work, so migrate while idle. Linux and macOS nodes also need the actual desktop session. The helper does not unlock a machine, bypass OS permissions, or create a desktop.

## Opt in on a worker

Add this to the worker's node configuration, using absolute paths, then restart the node while idle:

```json
{
  "rpa": {
    "command": "/absolute/path/to/rpa-env/bin/python",
    "args": ["/absolute/path/to/rpa/provider.py"]
  }
}
```

Windows equivalent:

```json
{
  "rpa": {
    "command": "C:\\Tools\\rpa-env\\Scripts\\python.exe",
    "args": ["C:\\Tools\\rpa\\provider.py"]
  }
}
```

The helper runs as the node's OS user and can operate that user's applications. Screenshots and accessibility labels may contain private information. Restrict `allow` to trusted caller IDs and grant `rpa.run` explicitly where appropriate. Tracked tasks also need `tasks.start` and owner-scoped task management permissions. Leases need their existing `leases.*` permissions; image downloads need `artifacts.open`. Omitting `allow` permits account-client execution, not independent worker execution. Opt-in is not an execution sandbox, and callers already permitted unrestricted `exec.run` can invoke other desktop tools themselves.

## Inspect, then act

Discover `rpa.run` with `node.describe` or MCP `control_describe`. MCP `control_rpa` accepts `node` and the same `actions` array as the capability. Its presence in the client does not mean the target has enabled GUI access.

```sh
control call worker rpa.run '{"actions":[{"type":"inspect","limit":100},{"type":"screenshot"}]}'
control task start worker @examples/rpa-task.json
```

Responses have a `results` array in action order. `inspect` returns `elements` with `app`, `name`, and native `role`, plus a `truncated` flag. Windows also reports `automationId` and `bounds`. Filter by exact `app` to narrow large desktops. On Windows `app` is the top-level window title; on macOS and Linux it is the application name. Roles are platform-native, for example `Button`, `AXButton`, or `push button`. Copy values from inspection rather than assuming a role spelling.

Selectors match their supplied fields exactly. They must resolve to exactly one element in a fresh, complete scan. Missing, ambiguous, or incomplete scans fail without acting on that selector. Scans inspect at most 2,000 nodes, 200 children per node, and 32 levels. A control can still change between resolution and activation; subsequent verification is necessary.

```json
{
  "actions": [
    { "type": "setValue", "target": { "app": "Editor", "name": "Search" }, "text": "Unicode text is supported here" },
    { "type": "click", "target": { "app": "Editor", "name": "Save", "role": "Button" } },
    { "type": "screenshot" }
  ]
}
```

Replace the example selector values with actual inspection results. Selector clicks invoke the native activation action once; they do not silently fall back to coordinates. Unsupported native actions fail. Use an explicitly chosen coordinate action after inspecting the current screen when accessibility is unavailable.

| Action | Fields and behavior |
| --- | --- |
| `inspect` | Optional exact `app`, `limit` 1..200, default 100 |
| `screenshot` | No arguments; returns a PNG `artifact` reference |
| `click` | Exact `target`, or `x` and `y`; coordinates allow `button` left/right/middle and `count` 1..2. Selector clicks allow only one left activation. |
| `focus` | Exact `target` |
| `setValue` | Exact `target`, Unicode `text` or worker-local `secret` name, at most 4,096 characters after resolution. Replaces the editable value through accessibility, not keystrokes. |
| `move` | `x`, `y` |
| `drag` | `x`, `y`, optional `durationMs` 100..5000, default 500; drags from the current pointer position with the left button |
| `scroll` | `clicks` -100..100; positive is up, negative is down, at the current pointer position |
| `type` | `text` or worker-local `secret` name, at most 4,096 characters after resolution; printable ASCII, newline and tab only, at the current focus. Use `setValue` for Unicode. |
| `keys` | `keys` array of 1..8 PyAutoGUI key names; presses a chord and releases it. Examples: `["ctrl","s"]`, `["command","s"]`. |
| `wait` | `milliseconds` 1..5000 |

Coordinates refer to the primary display's logical coordinate space, with integral values on that display. Multi-monitor coordinate input is not supported by the supplied helper. Screenshot results include image `width`/`height`, logical `screenWidth`/`screenHeight`, and `scaleX`/`scaleY`. Divide image coordinates by those scales before pointer input, especially on Retina displays. PyAutoGUI's corner failsafe remains enabled for its input actions; moving the mouse to a screen corner can stop them. It does not stop native accessibility actions. Use task cancellation to stop a batch.

Screenshots are imported into Control's immutable artifact store, not base64-encoded in control messages. Download with `control artifact get worker ARTIFACT_ID ./screen.png`, or deliver directly to another node. They are capped at 32 MiB and 64 million pixels. Helper workspaces are private temporary directories removed after each invocation. Artifacts remain until explicitly deleted.

Use [worker-local secrets](secrets.md) for credentials, never literal values in AI tool inputs. GUI text results and errors mask configured secret values; raw helper task logs and failure diagnostics are suppressed while any secrets are configured. Screenshots remain unmasked, and unrestricted execution can still extract credentials.

## Coordination and failures

Each batch contains 1..100 actions. Control validates the entire batch before launching the helper, which additionally validates backend-specific inputs before running actions. Invocations, including waiting for the desktop lock, are capped at two minutes or the caller's earlier deadline. Commands, tasks, owner checks, cancellation, and maintenance admission retain their existing behavior.

A shared file lock under the OS user's Control configuration directory serializes batches across node profiles using that directory. It does not exclude local human input or other automation software. For an interactive sequence on one node, acquire a node lease and submit each batch as a tracked `rpa.run` task with that `leaseId`. A synchronous capability call cannot bypass a lease. Leases on one node do not reserve another profile's desktop between batches.

On the first action failure, the helper stops and reports completed results plus an error. A tracked task retains those partial results, including any already-published screenshots. Synchronous calls return an error; use tasks when partial-result recovery matters. The failed action may itself have caused an effect. Never automatically replay a failed batch or retry a lost synchronous response. Inspect the current desktop, or reconcile a tracked submission by task ID, before deciding what to do next. Restart recovery never replays unfinished GUI tasks.

Cancellation kills the helper process tree, but does not undo clicks or edits. The helper releases pressed keys/buttons after ordinary exceptions and failsafe errors. Forced process termination can interrupt that cleanup; inspect the desktop and release input if needed before continuing. The integration does not claim atomic GUI transactions or exactly-once effects.

## Verification and limits

Core tests use a fake helper, without GUI dependencies, to verify opt-in, authorization, leases, idempotent tasks, serialization, cancellation, screenshot artifacts, and failure handling over WebRTC and relay. Run dependency-free Python contract tests with:

```sh
python3 -m unittest discover -s examples/rpa -p 'test_*.py'
```

Native helper behavior requires a logged-in Windows, macOS, or Linux desktop with the listed dependencies and permissions. Start with inspection and screenshots, then use a disposable application to verify native focus/value/activation and explicit pointer actions. Docker Compose and cross-compilation do not verify OS accessibility permissions or live desktop behavior. The helper provides no OCR, image matching, browser DOM automation, locked-screen operation, or native Wayland coordinate input.
