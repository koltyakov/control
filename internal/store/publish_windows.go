//go:build windows

package store

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

// publishNoReplace moves without MOVEFILE_REPLACE_EXISTING, so an existing
// destination fails the move, and with MOVEFILE_WRITE_THROUGH so the call
// returns only after the move is flushed.
func publishNoReplace(tmp, path string) error {
	src, err := extendedPath(tmp)
	if err != nil {
		return &os.LinkError{Op: "rename", Old: tmp, New: path, Err: err}
	}
	dst, err := extendedPath(path)
	if err != nil {
		return &os.LinkError{Op: "rename", Old: tmp, New: path, Err: err}
	}
	if err := windows.MoveFileEx(src, dst, windows.MOVEFILE_WRITE_THROUGH); err != nil {
		if errors.Is(err, windows.ERROR_ALREADY_EXISTS) || errors.Is(err, windows.ERROR_FILE_EXISTS) {
			return &os.LinkError{Op: "rename", Old: tmp, New: path, Err: os.ErrExist}
		}
		return &os.LinkError{Op: "rename", Old: tmp, New: path, Err: err}
	}
	return nil
}
