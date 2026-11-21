//go:build !windows

package store

import "os"

func syncDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = dir.Close() }()
	return dir.Sync()
}

func replaceFile(from, to string) error { return os.Rename(from, to) }
