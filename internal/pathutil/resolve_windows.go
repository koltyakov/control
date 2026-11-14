// Package pathutil resolves existing filesystem paths for path-policy checks.
// These checks do not replace rooted I/O or sandbox unrestricted processes.
package pathutil

import (
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

// Resolve returns the absolute path after following existing filesystem links.
// EvalSymlinks does not follow junctions reported as ModeIrregular by modern Go.
// Ask Windows for the final path of an opened handle instead.
func Resolve(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	extended := absolute
	if !strings.HasPrefix(extended, `\\?\`) && !strings.HasPrefix(extended, `\\.\`) {
		if strings.HasPrefix(extended, `\\`) {
			extended = `\\?\UNC\` + extended[2:]
		} else {
			extended = `\\?\` + extended
		}
	}
	name, err := windows.UTF16PtrFromString(extended)
	if err != nil {
		return "", err
	}
	handle, err := windows.CreateFile(name, 0, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return "", &os.PathError{Op: "resolve", Path: path, Err: err}
	}
	defer func() { _ = windows.CloseHandle(handle) }()
	buffer := make([]uint16, 256)
	for {
		n, err := windows.GetFinalPathNameByHandle(handle, &buffer[0], uint32(len(buffer)), 0)
		if err != nil {
			return "", &os.PathError{Op: "resolve", Path: path, Err: err}
		}
		if n >= uint32(len(buffer)) {
			if n >= 32768 {
				return "", &os.PathError{Op: "resolve", Path: path, Err: windows.ERROR_FILENAME_EXCED_RANGE}
			}
			buffer = make([]uint16, n+1)
			continue
		}
		return normalizeFinalPath(windows.UTF16ToString(buffer[:n])), nil
	}
}

func normalizeFinalPath(path string) string {
	if strings.HasPrefix(path, `\\?\UNC\`) {
		return `\\` + path[8:]
	}
	if strings.HasPrefix(path, `\\?\`) && len(path) >= 7 && path[5] == ':' && path[6] == '\\' {
		return path[4:]
	}
	// Volume-GUID paths need their extended prefix to remain usable.
	return path
}
