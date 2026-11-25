package store

import (
	"errors"
	"fmt"
	"os"
)

// PublishNew atomically publishes a synced temporary file at path, which must
// not exist. Unlike RenameInDirectory it never replaces an existing entry,
// including one created concurrently after an earlier check; the error then
// wraps os.ErrExist and tmp is left for the caller to remove. tmp and path
// must be in the same directory. A directory sync failure is returned even
// though the new entry may already be visible.
func PublishNew(tmp, path string) error {
	if err := publishNoReplace(tmp, path); err != nil {
		if errors.Is(err, os.ErrExist) {
			return fmt.Errorf("%s already exists: %w", path, os.ErrExist)
		}
		return err
	}
	return nil
}
