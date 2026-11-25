//go:build !windows

package node

import (
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestDirectoryExportRefusesSpecialFiles(t *testing.T) {
	t.Parallel()
	n := localNode(t, nil)
	writeTestFile(t, filepath.Join(n.Config.WorkDir, "tree", "file.txt"), "ok")
	if err := syscall.Mkfifo(filepath.Join(n.Config.WorkDir, "tree", "pipe"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := exportTree(t, n, "tree"); err == nil || !strings.Contains(err.Error(), "pipe is a symbolic link or special file") {
		t.Fatal("FIFO archived", err)
	}
}
