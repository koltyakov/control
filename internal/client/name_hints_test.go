package client

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/koltyakov/control/internal/model"
)

func TestNameHintsPersistFirstDirectoryMatch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "names.json")
	a, b := strings.Repeat("a", 64), strings.Repeat("b", 64)
	first := &nameHints{path: path}
	if first.Hint("worker") != "" {
		t.Fatal("missing file produced a hint")
	}
	first.Remember([]model.Node{{ID: a, Name: "worker"}, {ID: b, Name: "worker"}, {ID: "short", Name: "bad"}, {ID: b, Name: ""}})
	second := &nameHints{path: path}
	if second.Hint("worker") != a || second.Hint("bad") != "" || second.Hint("") != "" {
		t.Fatal("hints did not persist the directory's first valid match")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		t.Fatal("hint file is readable by other users", info.Mode())
	}
	// An unchanged directory does not rewrite the file.
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	second.Remember([]model.Node{{ID: a, Name: "worker"}})
	if info, err = os.Stat(path); err != nil || !info.ModTime().Equal(old) {
		t.Fatal("unchanged hints were rewritten", err)
	}
}

func TestNameHintsIgnoreCorruptFiles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "names.json")
	if err := os.WriteFile(path, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	h := &nameHints{path: path}
	if h.Hint("worker") != "" {
		t.Fatal("corrupt hint file produced a hint")
	}
	h.Remember([]model.Node{{ID: strings.Repeat("c", 64), Name: "worker"}})
	if (&nameHints{path: path}).Hint("worker") == "" {
		t.Fatal("corrupt hint file was not replaced")
	}
}
