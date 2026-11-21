//go:build windows

package store

import (
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

// Windows cannot fsync a directory handle opened with os.Open. replaceFile
// makes the rename itself durable instead.
func syncDirectory(path string) error { return nil }

// replaceFile renames with MOVEFILE_WRITE_THROUGH, so the call returns only
// after the move is flushed. Plain os.Rename leaves an acknowledged metadata
// record exposed to loss on power failure.
func replaceFile(from, to string) error {
	src, err := extendedPath(from)
	if err != nil {
		return &os.LinkError{Op: "rename", Old: from, New: to, Err: err}
	}
	dst, err := extendedPath(to)
	if err != nil {
		return &os.LinkError{Op: "rename", Old: from, New: to, Err: err}
	}
	if err := windows.MoveFileEx(src, dst, windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH); err != nil {
		return &os.LinkError{Op: "rename", Old: from, New: to, Err: err}
	}
	return nil
}

// extendedPath converts an absolute path to the \\?\ form so long paths work
// as they do through the os package.
func extendedPath(path string) (*uint16, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	if !strings.HasPrefix(abs, `\\`) {
		abs = `\\?\` + abs
	} else if !strings.HasPrefix(abs, `\\?\`) {
		abs = `\\?\UNC\` + strings.TrimPrefix(abs, `\\`)
	}
	return windows.UTF16PtrFromString(abs)
}
