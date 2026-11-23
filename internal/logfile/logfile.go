// Package logfile bounds service log files that the platform service manager
// or a parent process holds open for appending.
package logfile

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"time"
)

// MaxBytes is the size above which a log is rotated, keeping one previous file.
const MaxBytes = 16 << 20

const checkInterval = 10 * time.Minute

// Trim copies path to path.1, replacing an older previous file, and truncates
// path when it exceeds MaxBytes. Rotation copies and truncates instead of
// renaming because launchd, Windows supervisors, and parent processes keep their
// own append-mode handles; their later writes land at the new end of the file.
// A line written between the copy and the truncation can be lost.
func Trim(path string) error {
	return trim(path, MaxBytes)
}

func trim(path string, limit int64) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil || !info.Mode().IsRegular() || info.Size() <= limit {
		return err
	}
	source, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = source.Close() }()
	temp, err := os.CreateTemp(filepath.Dir(path), ".log-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(temp.Name()) }()
	_, err = io.Copy(temp, source)
	if closeErr := temp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err = os.Rename(temp.Name(), path+".1"); err != nil {
		return err
	}
	return os.Truncate(path, 0)
}

// Maintain trims path now and periodically until ctx ends.
func Maintain(ctx context.Context, path string) {
	ticker := time.NewTicker(checkInterval)
	defer ticker.Stop()
	for {
		_ = Trim(path)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
