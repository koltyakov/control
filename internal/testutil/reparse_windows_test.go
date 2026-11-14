package testutil

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWindowsJunctionFixture(t *testing.T) {
	base, target := t.TempDir(), t.TempDir()
	sentinel := filepath.Join(target, "keep")
	if err := os.WriteFile(sentinel, []byte("untouched"), 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "junction")
	Junction(t, target, link)
	if err := os.Remove(link); err != nil {
		t.Fatal("remove only junction", err)
	}
	if data, err := os.ReadFile(sentinel); err != nil || string(data) != "untouched" {
		t.Fatal("junction removal touched its target", err)
	}
}

func TestWindowsReparseFileFixture(t *testing.T) {
	path := filepath.Join(t.TempDir(), "opaque")
	ReparseFile(t, path)
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSocket == 0 {
		t.Fatal("AF_UNIX reparse file lost its socket type", info, err)
	}
}
