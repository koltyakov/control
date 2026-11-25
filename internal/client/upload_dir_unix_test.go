//go:build !windows

package client

import (
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestScanLocalTreeRefusesSpecialFiles(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{"ok.txt": "ok"})
	if err := syscall.Mkfifo(filepath.Join(root, "pipe"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := scanLocalTree(t.Context(), root, false, dirLimits{files: 10, dirs: 10}); err == nil || !strings.Contains(err.Error(), "pipe (special file)") {
		t.Fatal("FIFO not refused", err)
	}
	tree, err := scanLocalTree(t.Context(), root, true, dirLimits{files: 10, dirs: 10})
	if err != nil || len(tree.Files) != 1 || strings.Join(tree.Skipped, ",") != "pipe" {
		t.Fatal("FIFO not skipped", tree, err)
	}
}
