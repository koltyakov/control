//go:build windows

package node

import "golang.org/x/sys/windows"

// availableDiskBytes returns the free bytes available to this process's
// account, which honours per-user NTFS quotas, unlike the volume total.
func availableDiskBytes(path string) (uint64, error) {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	var available, total, free uint64
	if err = windows.GetDiskFreeSpaceEx(name, &available, &total, &free); err != nil {
		return 0, err
	}
	return available, nil
}
