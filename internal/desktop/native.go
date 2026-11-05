//go:build (darwin || linux || windows) && (amd64 || arm64)

package desktop

import (
	"errors"
	"os"
	"runtime"
	"strings"
	"unsafe"

	"github.com/ebitengine/purego"
)

// ABI 6/7 AdAction, passed by pointer only. Explicit padding pins its 64-bit
// layout on all six release targets; no structs cross the ABI by value.
// Binding reference: agent-desktop v1.2.0 crates/ffi/include/agent_desktop.h.
type nativeAction struct {
	Kind          int32
	_             uint32
	Text          *byte
	Scroll        [2]uint32
	Key           *byte
	Modifiers     *int32
	ModifierCount uint32
	_             uint32
	Drag          [6]uint64
}

type nativeAPI struct {
	abiVersion func() uint32
	init       func(uint32) int32
	actionSize func() uintptr
	create     func() uintptr
	destroy    func(uintptr)
	freeString func(*byte)
	status     func(uintptr, **byte) int32
	snapshot   func(uintptr, *byte, int32, uint8, bool, bool, **byte) int32
	execute    func(uintptr, *byte, *nativeAction, int32, int64, **byte) int32
}

func bindNative(symbol func(string) (uintptr, error)) (nativeAPI, error) {
	var api nativeAPI
	for _, entry := range []struct {
		name string
		fn   any
	}{
		{"ad_abi_version", &api.abiVersion}, {"ad_init", &api.init}, {"ad_action_size", &api.actionSize},
		{"ad_adapter_create", &api.create}, {"ad_adapter_destroy", &api.destroy},
		{"ad_free_string", &api.freeString}, {"ad_status", &api.status},
		{"ad_snapshot", &api.snapshot}, {"ad_execute_by_ref_timeout", &api.execute},
	} {
		address, err := symbol(entry.name)
		if err != nil || address == 0 {
			return nativeAPI{}, errors.New("native desktop symbol unavailable")
		}
		purego.RegisterFunc(entry.fn, address)
	}
	return api, nil
}

// This function must only run in the isolated helper process. Native crashes,
// blocking calls, and diagnostic output must never enter the node process.
func callNative(r Request, home, library string) ([]byte, error) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	for _, item := range os.Environ() {
		key, _, _ := strings.Cut(item, "=")
		if strings.HasPrefix(strings.ToUpper(key), "AGENT_DESKTOP_") {
			if err := os.Unsetenv(key); err != nil {
				return nil, errors.New("cannot isolate native desktop environment")
			}
		}
	}
	if err := os.Setenv("AGENT_DESKTOP_HOME", home); err != nil {
		return nil, errors.New("cannot set private native desktop state")
	}
	symbol, closeLibrary, err := openNativeLibrary(library)
	if err != nil {
		return nil, errors.New("cannot load native desktop library")
	}
	defer closeLibrary()
	api, err := bindNative(symbol)
	if err != nil {
		return nil, err
	}
	return invokeNative(api, r)
}

func invokeNative(api nativeAPI, r Request) ([]byte, error) {
	major := api.abiVersion()
	// ABI 7 changes low-level exact-identity APIs that this adapter never calls.
	// Our status, snapshot, and qualified-ref action contracts match ABI 6.
	if (major != 6 && major != 7) || api.init(major) != 0 || api.actionSize() != unsafe.Sizeof(nativeAction{}) {
		return nil, errors.New("native desktop ABI mismatch")
	}
	adapter := api.create() // No session, trace callback, clipboard, or CLI.
	if adapter == 0 {
		return nil, errors.New("cannot create native desktop adapter")
	}
	defer api.destroy(adapter)
	var pins runtime.Pinner
	defer pins.Unpin()
	cstring := func(value string) []byte {
		b := append([]byte(value), 0)
		pins.Pin(&b[0])
		return b
	}
	var output *byte
	pins.Pin(&output)
	switch r.Command {
	case "status":
		api.status(adapter, &output)
	case "snapshot":
		app := cstring(r.App)
		api.snapshot(adapter, &app[0], 0, uint8(r.MaxDepth), false, false, &output)
		runtime.KeepAlive(app)
	default:
		ref := cstring(r.Ref)
		action := nativeAction{}
		pins.Pin(&action)
		switch r.Command {
		case "setValue":
			action.Kind = 4
			text := cstring(*r.Text)
			defer clear(text) // Best effort; native code can retain other copies.
			action.Text = &text[0]
		case "clear":
			action.Kind = 18
		case "click":
			action.Kind = 0
		default:
			return nil, errors.New("unsupported native desktop action")
		}
		api.execute(adapter, &ref[0], &action, 0, 5000, &output)
		runtime.KeepAlive(ref)
		runtime.KeepAlive(action)
	}
	if output == nil {
		return nil, errors.New("native desktop response unavailable")
	}
	defer api.freeString(output)
	// Do not ask purego to convert an unbounded C string. Copy only after a NUL
	// is found within the output contract. A corrupt library remains isolated.
	for size := 0; size <= maxNativeOutput; size++ {
		if *(*byte)(unsafe.Add(unsafe.Pointer(output), size)) == 0 {
			return append([]byte(nil), unsafe.Slice(output, size)...), nil
		}
	}
	return nil, errors.New("native desktop response exceeds output limit")
}
