package update

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPruneUpdateTempsRemovesOnlyStaleTemporaries(t *testing.T) {
	dir := t.TempDir()
	version := filepath.Join(dir, "version")
	if err := os.Mkdir(version, 0700); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * time.Hour)
	for name, stale := range map[string]bool{".update-old": true, ".update-active": false, "control": true} {
		path := filepath.Join(version, name)
		if err := os.WriteFile(path, nil, 0600); err != nil {
			t.Fatal(err)
		}
		if stale {
			if err := os.Chtimes(path, old, old); err != nil {
				t.Fatal(err)
			}
		}
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	if err := pruneUpdateTemps(root, "version", time.Now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	for name, kept := range map[string]bool{".update-old": false, ".update-active": true, "control": true} {
		if _, err := os.Stat(filepath.Join(version, name)); (err == nil) != kept {
			t.Fatal("unexpected retention", name, err)
		}
	}
}
