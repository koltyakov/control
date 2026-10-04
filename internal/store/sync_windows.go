//go:build windows

package store

// Windows does not support fsync on a directory handle opened with os.Open.
func syncDirectory(path string) error { return nil }
