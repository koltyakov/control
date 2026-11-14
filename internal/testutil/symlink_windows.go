package testutil

import (
	"errors"

	"golang.org/x/sys/windows"
)

func symlinkUnavailable(err error) bool {
	// ACCESS_DENIED can be a broken fixture ACL, not a missing symlink right.
	// Do not hide it, invalid paths or collisions behind a capability skip.
	return errors.Is(err, windows.ERROR_PRIVILEGE_NOT_HELD) ||
		errors.Is(err, windows.ERROR_NOT_SUPPORTED) ||
		errors.Is(err, windows.ERROR_INVALID_FUNCTION) ||
		errors.Is(err, windows.ERROR_CALL_NOT_IMPLEMENTED)
}
