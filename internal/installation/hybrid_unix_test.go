//go:build !windows

package installation

import (
	"os"
	"syscall"
	"testing"
)

type hybridPathInfo struct {
	os.FileInfo
	mode os.FileMode
	stat syscall.Stat_t
}

func (i hybridPathInfo) Mode() os.FileMode { return i.mode }
func (i hybridPathInfo) Sys() any          { return &i.stat }

func TestHybridPrivilegedPathOwnership(t *testing.T) {
	for _, test := range []struct {
		name    string
		uid     uint32
		mode    os.FileMode
		trusted bool
	}{
		{"root-private-file", 0, 0600, true},
		{"root-readable-file", 0, 0644, true},
		{"root-directory", 0, os.ModeDir | 0755, true},
		{"user-owned", 1000, 0600, false},
		{"group-writable", 0, 0660, false},
		{"world-writable", 0, 0602, false},
		{"symlink", 0, os.ModeSymlink | 0700, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			info := hybridPathInfo{mode: test.mode, stat: syscall.Stat_t{Uid: test.uid}}
			if got := trustedHybridPath(info); got != test.trusted {
				t.Fatalf("trusted=%v, want %v", got, test.trusted)
			}
		})
	}
}
