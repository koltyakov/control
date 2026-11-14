package pathutil

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/koltyakov/control/internal/testutil"
)

func TestResolveWindowsJunctions(t *testing.T) {
	base, target := t.TempDir(), t.TempDir()
	junction := filepath.Join(base, "alias")
	testutil.Junction(t, target, junction)
	// Exercise the final junction, an ancestor junction, Unicode, and a path
	// longer than the initial GetFinalPathNameByHandle buffer.
	leaf := filepath.Join("日本語", strings.Repeat("segment", 20), strings.Repeat("nested", 20))
	if err := os.MkdirAll(filepath.Join(target, leaf), 0700); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"", leaf} {
		expected, err := Resolve(filepath.Join(target, suffix))
		if err != nil {
			t.Fatal(err)
		}
		resolved, err := Resolve(filepath.Join(junction, suffix))
		if err != nil || resolved != expected {
			t.Fatal("junction not resolved to canonical target", resolved, expected, err)
		}
	}
}

func TestResolveWindowsPathForms(t *testing.T) {
	for _, entry := range []struct{ input, expected string }{
		{`\\?\C:\temp\file`, `C:\temp\file`},
		{`\\?\UNC\server\share\file`, `\\server\share\file`},
		{`\\?\Volume{fixture}\file`, `\\?\Volume{fixture}\file`},
		{`C:\temp\file`, `C:\temp\file`},
	} {
		if resolved := normalizeFinalPath(entry.input); resolved != entry.expected {
			t.Fatalf("normalize %q: got %q, want %q", entry.input, resolved, entry.expected)
		}
	}
}
