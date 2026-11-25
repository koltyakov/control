//go:build !windows

package vaultops

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// ensurePrivateDir creates dir for the broker's OS user and rejects one that
// another user owns or that grants group or other access.
func ensurePrivateDir(dir string) error {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is not a directory", dir)
	}
	if info.Mode().Perm()&0077 != 0 {
		return fmt.Errorf("%s must not grant group or other access", dir)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return errors.New("cannot verify broker directory ownership")
	}
	if int(stat.Uid) != os.Geteuid() {
		return fmt.Errorf("%s is owned by another user", dir)
	}
	return nil
}
