//go:build !windows

package installation

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

func checkHybridSystem(_ context.Context, binary, config, mode string) error {
	if os.Geteuid() != 0 || mode != "system" {
		return errors.New("hybrid system startup requires root and system mode")
	}
	for _, path := range []string{binary, config} {
		for {
			info, err := os.Lstat(path)
			if err != nil {
				return err
			}
			if !trustedHybridPath(info) {
				return fmt.Errorf("hybrid system path must be root-owned, not a symlink, and not writable by group or others: %s", path)
			}
			parent := filepath.Dir(path)
			if parent == path {
				break
			}
			path = parent
		}
	}
	return nil
}

func trustedHybridPath(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Uid == 0 && info.Mode()&os.ModeSymlink == 0 && info.Mode().Perm()&0022 == 0
}
