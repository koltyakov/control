# Native agent-desktop library setup

Control includes an isolated Go helper mode using purego. No separate helper build, C++ compiler, Python runtime, or agent-desktop CLI is needed. See [setup, API, and security limits](../../docs/desktop.md).

Obtain the worker's OS/architecture FFI archive from [agent-desktop releases](https://github.com/vercel-labs/agent-desktop/releases) and verify its published checksum. The bindings target ABI major 6 and a 96-byte `AdAction`, as shipped in v1.0.2. Both are checked before dispatch. If building upstream yourself, use `cargo build --profile release-ffi -p agent-desktop-ffi`; the ordinary Rust release profile does not provide the same panic boundary.

Keep upstream native dependencies beside the library as required by its release. Install outside the worker's workspace in a location writable only by its trusted OS user or administrator.

Merge [node-config.json](node-config.json) into the worker profile, replacing `desktop.library` with an absolute path. Use `libagent_desktop_ffi.dylib` on macOS, `.so` on Linux, or `agent_desktop_ffi.dll` on Windows with JSON-escaped backslashes. Restart the node while idle. The serving node never loads this library; each request launches `control __desktop LIBRARY` in a separate process.

`make update` ships the helper code, but does not install libraries, enable desktop access, or grant OS permissions. Existing external `desktop.command` configurations still work; they cannot be combined with `library`.

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
