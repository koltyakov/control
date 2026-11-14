package pathutil

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestResolveExistingPaths(t *testing.T) {
	base := t.TempDir()
	path := filepath.Join(base, "file")
	if err := os.WriteFile(path, []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{base, path} {
		resolved, err := Resolve(path)
		if err != nil || !filepath.IsAbs(resolved) {
			t.Fatal("existing path not resolved", resolved, err)
		}
		original, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		actual, err := os.Stat(resolved)
		if err != nil || !os.SameFile(original, actual) {
			t.Fatal("resolution changed file identity", err)
		}
	}
	if _, err := Resolve(filepath.Join(base, "missing")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("missing path was not reported", err)
	}
}
