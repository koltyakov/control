//go:build !windows

package testutil

import (
	"fmt"
	"os"
	"syscall"
	"testing"
)

func missingSymlinkCapability() error { return syscall.EOPNOTSUPP }

func TestUnixSymlinkUnavailable(t *testing.T) {
	for _, err := range []error{syscall.EPERM, syscall.ENOSYS, syscall.EOPNOTSUPP} {
		if !symlinkUnavailable(&os.LinkError{Op: "symlink", Err: fmt.Errorf("wrapped: %w", err)}) {
			t.Fatal("missing capability was not recognized", err)
		}
	}
	for _, err := range []error{nil, syscall.EACCES, syscall.EEXIST, syscall.ENOENT, syscall.ENOTDIR} {
		if symlinkUnavailable(err) {
			t.Fatal("unexpected failure was treated as missing capability", err)
		}
	}
}
