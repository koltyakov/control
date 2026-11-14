package testutil

import (
	"fmt"
	"os"
	"testing"

	"golang.org/x/sys/windows"
)

func missingSymlinkCapability() error { return windows.ERROR_PRIVILEGE_NOT_HELD }

func TestWindowsSymlinkUnavailable(t *testing.T) {
	for _, err := range []error{
		windows.ERROR_PRIVILEGE_NOT_HELD, windows.ERROR_NOT_SUPPORTED,
		windows.ERROR_INVALID_FUNCTION, windows.ERROR_CALL_NOT_IMPLEMENTED,
	} {
		if !symlinkUnavailable(&os.LinkError{Op: "symlink", Err: fmt.Errorf("wrapped: %w", err)}) {
			t.Fatal("missing capability was not recognized", err)
		}
	}
	for _, err := range []error{
		nil, windows.ERROR_ACCESS_DENIED, windows.ERROR_ALREADY_EXISTS,
		windows.ERROR_PATH_NOT_FOUND, windows.ERROR_INVALID_NAME,
	} {
		if symlinkUnavailable(err) {
			t.Fatal("unexpected failure was treated as missing capability", err)
		}
	}
}
