# agent-desktop setup

For the full upstream command surface, install the verified native CLI and companion files after approval, set its absolute path as `desktop.cli`, and invoke `desktop.run` with `{"command":"cli","args":["list-apps"]}`. CLI-only profiles are supported. To add protected credential input, also install and configure the native library below. The [example profile](node-config.json) enables both; omit a backend you have not installed. See [full commands, discovery, output artifacts and limits](../../docs/desktop.md#full-upstream-command-surface). Installing only the library does not provide full CLI functionality.

Control includes an isolated Go helper mode using purego. No separate helper build, C++ compiler, Python runtime, or agent-desktop CLI is needed. See [setup, API, and security limits](../../docs/desktop.md).

When a GUI task needs missing desktop support, suggest installation and ask for approval. After approval, select the latest compatible release from the public [upstream releases](https://github.com/vercel-labs/agent-desktop/releases). Download its `agent-desktop-ffi-*` archive for the worker's OS/architecture and verify SHA-256 against `checksums.txt` from the same release before extraction. Control releases do not bundle these archives. Compatibility requires ABI major 6 or 7 and a 96-byte `AdAction`; the isolated helper rejects other versions and layout mismatches before dispatch. agent-desktop v1.2.0 retains ABI 7, including its native Windows ARM64 library. Existing ABI 6 and v1.1.0 libraries remain supported. Do not bypass these checks or use credentials until native status succeeds.

Extract the full package, keeping upstream companion executables and licenses beside the library. Install outside `workDir` in a directory private to the worker's OS user. Use the logged-in User profile, never a Hybrid System companion for GUI or credentials.

Merge [node-config.json](node-config.json) into the worker profile, replacing `desktop.library` with an absolute path. Use `lib/libagent_desktop_ffi.dylib` on macOS, `lib/libagent_desktop_ffi.so` on Linux, or `lib/agent_desktop_ffi.dll` on Windows with JSON-escaped backslashes. Preserve other profile settings, restart while idle, and check `desktop.run` status. The serving node never loads this library; each request launches `control __desktop LIBRARY` in a separate process.

`make update` ships the helper code, but does not install libraries, enable desktop access, or grant OS permissions. Reuse the installed compatible library across Control updates. Existing external `desktop.command` configurations still work; they cannot be combined with `library`. If building upstream yourself, use `cargo build --profile release-ffi -p agent-desktop-ffi` to retain its panic boundary.

## Tests

Default tests need no native library, compiler, or live desktop. They cover purego ABI calls through Go callbacks, framing, ABI/layout refusal, output bounds, failed-action envelopes, early CLI helper routing, and worker credential privacy:

```sh
go test ./internal/desktop ./internal/node ./internal/client ./cmd/control -run 'Desktop|MCP'
CGO_ENABLED=0 go build -o bin/control ./cmd/control
```

An optional real-library status check verifies loading and private native environment settings without GUI mutations:

```sh
CONTROL_TEST_DESKTOP_LIBRARY=/absolute/path/to/libagent_desktop_ffi.dylib \
  CGO_ENABLED=0 go test ./cmd/control -run '^TestDesktopCLIRealLibraryStatus$' -count=1
```

Actual OS permissions and application behavior still require native GUI smoke tests. Cross-builds do not validate those behaviors.
