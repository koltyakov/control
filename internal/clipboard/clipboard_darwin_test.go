package clipboard

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// This opt-in test exercises AppKit without reading or replacing the user's clipboard.
func TestClipboardDarwinPrivatePasteboard(t *testing.T) {
	if os.Getenv("CONTROL_TEST_MACOS_CLIPBOARD") != "1" {
		t.Skip("set CONTROL_TEST_MACOS_CLIPBOARD=1 for private-pasteboard smoke tests")
	}
	quote := func(s string) string { b, _ := json.Marshal(s); return string(b) }
	paths := []string{filepath.Join(t.TempDir(), "file with spaces.txt"), filepath.Join(t.TempDir(), "世界.txt")}
	for _, path := range paths {
		if err := os.WriteFile(path, []byte("test file"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	text := "Clipboard test 世界\nsecond line"
	files := "var items = $.NSMutableArray.array;\n"
	for _, path := range paths {
		files += "items.addObject($.NSURL.fileURLWithPath(" + quote(path) + "));\n"
	}
	files += "p.writeObjects(items);"
	for _, test := range []struct {
		name    string
		seed    string
		want    Value
		wantErr bool
	}{
		{"text", "p.setStringForType(" + quote(text) + ", $.NSPasteboardTypeString);", Value{Text: text}, false},
		{"files", files, Value{Paths: paths}, false},
		{"empty text", "p.setStringForType('', $.NSPasteboardTypeString);", Value{}, false},
		{"unsupported", "", Value{}, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			read := strings.Replace(clipboardReadScript, "var p = $.NSPasteboard.generalPasteboard;", "", 1)
			read = strings.Replace(read, "JSON.stringify({paths:", "return JSON.stringify({paths:", 1)
			script := "ObjC.import('AppKit'); var p = $.NSPasteboard.pasteboardWithUniqueName; p.clearContents;\n" + test.seed + "\nvar result; try { result = (function(){\n" + read + "\n})(); } finally { p.releaseGlobally; } result;"
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			value, err := readJSON(exec.CommandContext(ctx, "osascript", "-l", "JavaScript", "-e", script))
			if test.wantErr {
				if err == nil {
					t.Fatal("unsupported clipboard accepted")
				}
				return
			}
			if err != nil || value.Text != test.want.Text || !slices.Equal(value.Paths, test.want.Paths) {
				t.Fatal("private pasteboard read differs", value, err)
			}
		})
	}
}
