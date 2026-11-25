//go:build !windows

package store

import (
	"fmt"
	"os"
	"path/filepath"
)

// publishNoReplace links tmp at path, which fails if path exists, then
// removes the temporary name. Filesystems without hard links are refused
// rather than falling back to a replacing rename.
func publishNoReplace(tmp, path string) error {
	if err := os.Link(tmp, path); err != nil {
		if os.IsExist(err) {
			return err
		}
		return fmt.Errorf("create %s without overwrite (requires hard-link support): %w", path, err)
	}
	if err := os.Remove(tmp); err != nil {
		return err
	}
	return syncDirectory(filepath.Dir(path))
}
