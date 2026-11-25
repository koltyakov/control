//go:build windows

package vaultops

import (
	"fmt"
	"os"
)

// ensurePrivateDir creates dir under the broker account. Windows access
// control is inherited from the dedicated account's profile or the service
// installer's ACL; this check only rejects links and non-directories.
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
	return nil
}
