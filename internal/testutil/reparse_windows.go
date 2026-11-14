package testutil

import (
	"encoding/binary"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"golang.org/x/sys/windows"
)

// RequireReparsePointsEnv requires the local NTFS junction and AF_UNIX fixtures.
// It is separate from symlink authority and never provisions the machine.
const RequireReparsePointsEnv = "CONTROL_TEST_REQUIRE_REPARSE_POINTS"

const reparseTagUnix = 0x80000023 // IO_REPARSE_TAG_AF_UNIX

func requireReparseVolume(t testing.TB, path string, socket bool) {
	t.Helper()
	required := false
	if value := os.Getenv(RequireReparsePointsEnv); value != "" {
		var err error
		required, err = strconv.ParseBool(value)
		if err != nil {
			t.Fatalf("invalid %s=%q: %v", RequireReparsePointsEnv, value, err)
		}
	}
	unavailable := func(reason string) {
		t.Helper()
		if required {
			t.Fatalf("Windows reparse fixtures required by %s: %s", RequireReparsePointsEnv, reason)
		}
		t.Skipf("Windows reparse fixtures unavailable: %s; use a local NTFS temp directory", reason)
	}
	parent, err := windows.UTF16PtrFromString(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	volume := make([]uint16, 32768)
	if err := windows.GetVolumePathName(parent, &volume[0], uint32(len(volume))); err != nil {
		t.Fatal("inspect reparse fixture volume", err)
	}
	var flags uint32
	var filesystem [256]uint16
	if err := windows.GetVolumeInformation(&volume[0], nil, 0, nil, nil, &flags, &filesystem[0], uint32(len(filesystem))); err != nil {
		t.Fatal("inspect reparse fixture filesystem", err)
	}
	if windows.GetDriveType(&volume[0]) != windows.DRIVE_FIXED ||
		windows.UTF16ToString(filesystem[:]) != "NTFS" || flags&windows.FILE_SUPPORTS_REPARSE_POINTS == 0 {
		unavailable("fixture requires a fixed local NTFS volume with reparse-point support")
	}
	if socket {
		version := windows.RtlGetVersion()
		if version.MajorVersion < 10 || version.MajorVersion == 10 && version.BuildNumber < 17063 {
			unavailable("AF_UNIX requires Windows 10 build 17063 or newer")
		}
	}
}

// Junction creates an IO_REPARSE_TAG_MOUNT_POINT directory junction. It uses
// owned scratch paths and FSCTL_SET_REPARSE_POINT, not mklink, volume mounting,
// symlink privileges, or Developer Mode. Creation errors after preflight fail.
func Junction(t testing.TB, target, link string) {
	t.Helper()
	requireReparseVolume(t, link, false)
	target, err := filepath.Abs(target)
	if err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(target); err != nil || !info.IsDir() {
		t.Fatal("junction target must be an existing scratch directory", err)
	}
	substitute, err := windows.UTF16FromString(`\??\` + target)
	if err != nil {
		t.Fatal(err)
	}
	printName, err := windows.UTF16FromString(target)
	if err != nil {
		t.Fatal(err)
	}
	dataLength := 8 + 2*(len(substitute)+len(printName))
	if dataLength+8 > windows.MAXIMUM_REPARSE_DATA_BUFFER_SIZE {
		t.Fatal("junction target is too long")
	}
	buffer := make([]byte, dataLength+8)
	binary.LittleEndian.PutUint32(buffer[0:4], windows.IO_REPARSE_TAG_MOUNT_POINT)
	binary.LittleEndian.PutUint16(buffer[4:6], uint16(dataLength))
	binary.LittleEndian.PutUint16(buffer[10:12], uint16(2*(len(substitute)-1)))
	binary.LittleEndian.PutUint16(buffer[12:14], uint16(2*len(substitute)))
	binary.LittleEndian.PutUint16(buffer[14:16], uint16(2*(len(printName)-1)))
	for i, ch := range append(substitute, printName...) {
		binary.LittleEndian.PutUint16(buffer[16+2*i:], ch)
	}
	if err := os.Mkdir(link, 0700); err != nil {
		t.Fatal(err)
	}
	// Remove only the junction itself, even if fixture assertions fail.
	t.Cleanup(func() {
		info, err := os.Lstat(link)
		if errors.Is(err, os.ErrNotExist) {
			return
		}
		if err != nil {
			t.Error("inspect scratch junction for cleanup", err)
			return
		}
		// A cache may safely replace the entry itself with verified bytes.
		if info.Mode().IsRegular() {
			return
		}
		path, err := windows.UTF16PtrFromString(link)
		if err == nil {
			err = windows.RemoveDirectory(path)
		}
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Error("remove scratch junction", err)
		}
	})
	handle := openReparse(t, link, windows.GENERIC_WRITE)
	var returned uint32
	err = windows.DeviceIoControl(handle, windows.FSCTL_SET_REPARSE_POINT, &buffer[0], uint32(len(buffer)), nil, 0, &returned, nil)
	closeErr := windows.CloseHandle(handle)
	if err != nil || closeErr != nil {
		t.Fatal("create scratch junction", err, closeErr)
	}
	checkReparse(t, link, windows.IO_REPARSE_TAG_MOUNT_POINT)
	resolved, err := os.Stat(link)
	original, targetErr := os.Stat(target)
	if err != nil || targetErr != nil || !os.SameFile(original, resolved) {
		t.Fatal("junction did not resolve to its target", err, targetErr)
	}
}

// ReparseFile creates a real, non-symlink AF_UNIX reparse file. The temporary
// listener is closed before returning; no application or network peer is used.
// Binding under a short scratch name then renaming avoids sockaddr path limits.
func ReparseFile(t testing.TB, path string) {
	t.Helper()
	requireReparseVolume(t, path, true)
	scratch, err := os.MkdirTemp("", "cr-")
	if err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(scratch, "s")
	t.Cleanup(func() {
		for _, name := range []string{source, scratch} {
			if err := os.Remove(name); err != nil && !errors.Is(err, os.ErrNotExist) {
				t.Error("remove reparse scratch path", err)
			}
		}
	})
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: source, Net: "unix"})
	if err != nil {
		t.Fatal("create AF_UNIX reparse fixture", err)
	}
	listener.SetUnlinkOnClose(false)
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	from, err := windows.UTF16PtrFromString(source)
	if err != nil {
		t.Fatal(err)
	}
	to, err := windows.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	// MoveFile refuses to overwrite an existing destination.
	if err := windows.MoveFile(from, to); err != nil {
		t.Fatal("publish AF_UNIX reparse fixture", err)
	}
	t.Cleanup(func() {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Error("remove AF_UNIX reparse fixture", err)
		}
	})
	checkReparse(t, path, reparseTagUnix)
}

func openReparse(t testing.TB, path string, access uint32) windows.Handle {
	t.Helper()
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	handle, err := windows.CreateFile(name, access, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		t.Fatal("open scratch reparse point itself", err)
	}
	return handle
}

func checkReparse(t testing.TB, path string, tag uint32) {
	t.Helper()
	handle := openReparse(t, path, 0)
	buffer := make([]byte, windows.MAXIMUM_REPARSE_DATA_BUFFER_SIZE)
	var returned uint32
	err := windows.DeviceIoControl(handle, windows.FSCTL_GET_REPARSE_POINT, nil, 0, &buffer[0], uint32(len(buffer)), &returned, nil)
	closeErr := windows.CloseHandle(handle)
	if err != nil || closeErr != nil || returned < 8 || binary.LittleEndian.Uint32(buffer[:4]) != tag {
		t.Fatalf("wrong reparse tag, want %#x: bytes=%d err=%v close=%v", tag, returned, err, closeErr)
	}
	info, err := os.Lstat(path)
	if err != nil || info.IsDir() || info.Mode().IsRegular() {
		t.Fatal("fixture was not reported as a special reparse entry", info, err)
	}
}
