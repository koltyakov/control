package update

import (
	"errors"
	"os"
	"path/filepath"
	"unsafe"

	"golang.org/x/sys/windows"
)

func machineCachePath() (string, error) {
	base, err := windows.KnownFolderPath(windows.FOLDERID_ProgramData, 0)
	return filepath.Join(base, "Control Update Cache v1"), err
}

func mkdirMachineCache(parent *os.Root, name string) (bool, error) {
	// These are deliberately untrusted transport bytes. Authenticated users,
	// LocalService and SYSTEM all need access regardless of who downloads first.
	sd, err := windows.SecurityDescriptorFromString("D:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)(A;OICI;FA;;;AU)(A;OICI;FA;;;LS)")
	if err != nil {
		return false, err
	}
	path, err := windows.UTF16PtrFromString(filepath.Join(parent.Name(), name))
	if err != nil {
		return false, err
	}
	attrs := windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: sd}
	err = windows.CreateDirectory(path, &attrs)
	if errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
		return false, nil
	}
	return err == nil, err
}

func shareMachineCache(*os.Root) error { return nil } // DACL set atomically at creation.
func cacheReadFlags() int              { return os.O_RDONLY }

func tryMachineCacheLock(f *os.File) (bool, error) {
	err := windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &windows.Overlapped{})
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) || errors.Is(err, windows.ERROR_IO_PENDING) {
		return false, nil
	}
	return err == nil, err
}
