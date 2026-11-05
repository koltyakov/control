//go:build (darwin || linux || windows) && (amd64 || arm64)

package desktop

import (
	"bytes"
	"errors"
	"runtime"
	"strconv"
	"testing"
	"unsafe"

	"github.com/ebitengine/purego"
)

func TestDesktopNativeActionLayout(t *testing.T) {
	var a nativeAction
	if unsafe.Sizeof(a) != 96 || unsafe.Alignof(a) != 8 {
		t.Fatal("ABI 6/7 requires a 96-byte, 8-aligned AdAction")
	}
	for _, field := range []struct{ got, want uintptr }{
		{unsafe.Offsetof(a.Kind), 0}, {unsafe.Offsetof(a.Text), 8},
		{unsafe.Offsetof(a.Scroll), 16}, {unsafe.Offsetof(a.Key), 24},
		{unsafe.Offsetof(a.Modifiers), 32}, {unsafe.Offsetof(a.ModifierCount), 40},
		{unsafe.Offsetof(a.Drag), 48},
	} {
		if field.got != field.want {
			t.Fatalf("ABI field offset %d, want %d", field.got, field.want)
		}
	}
}

func nativeTestString(p *byte) string {
	if p == nil {
		return ""
	}
	for n := 0; n < 4097; n++ {
		if *(*byte)(unsafe.Add(unsafe.Pointer(p), n)) == 0 {
			return string(unsafe.Slice(p, n))
		}
	}
	return "unterminated"
}

// Go callbacks provide real C-ABI addresses. These tests exercise purego's
// binding and call paths without a compiler, installed library, or desktop.
func TestDesktopNativeBindings(t *testing.T) {
	text := "pipe-only-\"credential-秘密"
	for _, major := range []uint32{6, 7} {
		for _, command := range []string{"status", "snapshot", "setValue", "click", "clear"} {
			t.Run(strconv.Itoa(int(major))+"/"+command, func(t *testing.T) {
				runtime.LockOSThread()
				defer runtime.UnlockOSThread()
				response := []byte(`{"ok":false,"error":{"code":"STALE_REF","disposition":{"delivery":"not_delivered","retry":"safe"}}}`)
				if command == "snapshot" {
					// v1.2.0 returns successful partial observations, not TIMEOUT.
					response = []byte(`{"ok":true,"command":"snapshot","data":{"complete":false,"truncated":true,"nodes_observed":1,"snapshot_id":"stest","tree":{"ref":"@stest:e1","subtree_truncated":true}}}`)
				}
				storage := append(bytes.Clone(response), 0)
				var pins runtime.Pinner
				pins.Pin(&storage[0])
				defer pins.Unpin()
				created, destroyed, freed, invoked := 0, 0, 0, 0
				finish := func(adapter uintptr, out **byte) uintptr {
					runtime.GC() // Pinned inputs and out storage must survive native calls.
					if adapter != 17 || out == nil {
						t.Error("adapter or output pointer was not marshalled")
						return uintptr(^uint32(0))
					}
					invoked++
					*out = &storage[0]
					if command == "snapshot" {
						return 0
					}
					return uintptr(^uint32(0)) // Nonzero results must retain their error envelopes.
				}
				symbols := map[string]uintptr{
					"ad_abi_version": purego.NewCallback(func() uintptr { return uintptr(major) }),
					"ad_init": purego.NewCallback(func(expected uint32) uintptr {
						if expected != major {
							t.Error("wrong ABI major")
						}
						return 0
					}),
					"ad_action_size":    purego.NewCallback(func() uintptr { return 96 }),
					"ad_adapter_create": purego.NewCallback(func() uintptr { created++; return 17 }),
					"ad_adapter_destroy": purego.NewCallback(func(adapter uintptr) uintptr {
						if adapter != 17 {
							t.Error("wrong adapter destroyed")
						}
						destroyed++
						return 0
					}),
					"ad_free_string": purego.NewCallback(func(p *byte) uintptr {
						if p != &storage[0] {
							t.Error("wrong output freed")
						}
						freed++
						clear(storage)
						return 0
					}),
					"ad_status": purego.NewCallback(func(adapter uintptr, out **byte) uintptr {
						if command != "status" {
							t.Error("wrong entrypoint")
						}
						return finish(adapter, out)
					}),
					"ad_snapshot": purego.NewCallback(func(adapter uintptr, app *byte, surface int32, depth uint8, interactive, compact bool, out **byte) uintptr {
						runtime.GC()
						if command != "snapshot" || nativeTestString(app) != "Test app 秘密" || surface != 0 || depth != 32 || interactive || compact {
							t.Error("snapshot arguments were not marshalled")
						}
						return finish(adapter, out)
					}),
					"ad_execute_by_ref_timeout": purego.NewCallback(func(adapter uintptr, ref *byte, action *nativeAction, policy int32, timeout int64, out **byte) uintptr {
						runtime.GC()
						kind := map[string]int32{"setValue": 4, "click": 0, "clear": 18}[command]
						if nativeTestString(ref) != "@stest:e1" || action == nil || policy != 0 || timeout != 5000 {
							t.Error("action arguments or headless policy were not marshalled")
							return uintptr(^uint32(0))
						}
						want := nativeAction{Kind: kind}
						if command == "setValue" {
							want.Text = action.Text
							if nativeTestString(action.Text) != text {
								t.Error("private text buffer was not marshalled")
							}
						}
						if *action != want {
							t.Error("action layout or zero initialization changed")
						}
						return finish(adapter, out)
					}),
				}
				api, err := bindNative(func(name string) (uintptr, error) { return symbols[name], nil })
				if err != nil {
					t.Fatal(err)
				}
				r := Request{Command: command, App: "Test app 秘密", Ref: "@stest:e1", Text: &text, MaxDepth: 32}
				out, err := invokeNative(api, r)
				if err != nil || !bytes.Equal(out, response) || created != 1 || destroyed != 1 || freed != 1 || invoked != 1 {
					t.Fatal("native dispatch, ownership, copied output, or error envelope changed")
				}
			})
		}
	}
}

func TestDesktopNativeFailures(t *testing.T) {
	for _, tc := range []string{"abi", "size", "create", "nil-output", "oversized", "at-limit"} {
		t.Run(tc, func(t *testing.T) {
			created, destroyed, freed := 0, 0, 0
			storage := bytes.Repeat([]byte{'x'}, maxNativeOutput+2)
			if tc == "at-limit" {
				storage[maxNativeOutput] = 0
			}
			api := nativeAPI{
				abiVersion: func() uint32 { return 7 },
				init: func(uint32) int32 {
					if tc == "abi" {
						return -1
					}
					return 0
				},
				actionSize: func() uintptr {
					if tc == "size" {
						return 8
					}
					return 96
				},
				create: func() uintptr {
					created++
					if tc == "create" {
						return 0
					}
					return 17
				},
				destroy: func(uintptr) { destroyed++ }, freeString: func(*byte) { freed++ },
				status: func(_ uintptr, out **byte) int32 {
					if tc != "nil-output" {
						*out = &storage[0]
					}
					return 0
				},
			}
			out, err := invokeNative(api, Request{Command: "status"})
			if tc == "at-limit" {
				if err != nil || len(out) != maxNativeOutput {
					t.Fatal("exact output limit refused")
				}
			} else if err == nil {
				t.Fatal("native infrastructure failure accepted")
			}
			wantCreated, wantDestroyed, wantFreed := 1, 1, 0
			switch tc {
			case "abi", "size":
				wantCreated, wantDestroyed = 0, 0
			case "create":
				wantDestroyed = 0
			case "oversized", "at-limit":
				wantFreed = 1
			}
			if created != wantCreated || destroyed != wantDestroyed || freed != wantFreed {
				t.Fatal("native resources leaked")
			}
		})
	}
	for _, symbol := range []func(string) (uintptr, error){
		func(string) (uintptr, error) { return 0, nil },
		func(string) (uintptr, error) { return 0, errors.New("private loader diagnostics") },
	} {
		if _, err := bindNative(symbol); err == nil || err.Error() != "native desktop symbol unavailable" {
			t.Fatal("symbol failure exposed diagnostics")
		}
	}
}

func TestDesktopNativeRejectsUnknownABI(t *testing.T) {
	for _, major := range []uint32{0, 5, 8, ^uint32(0)} {
		t.Run(strconv.Itoa(int(major)), func(t *testing.T) {
			unexpected := func() { t.Fatal("unsupported ABI reached initialization or dispatch") }
			api := nativeAPI{
				abiVersion: func() uint32 { return major },
				init:       func(uint32) int32 { unexpected(); return 0 },
				actionSize: func() uintptr { unexpected(); return 96 },
				create:     func() uintptr { unexpected(); return 17 },
			}
			out, err := invokeNative(api, Request{Command: "status"})
			if out != nil || err == nil || err.Error() != "native desktop ABI mismatch" {
				t.Fatal("unknown ABI accepted or exposed native diagnostics")
			}
		})
	}
}
