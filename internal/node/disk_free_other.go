//go:build !windows

package node

import "github.com/shirou/gopsutil/v4/disk"

// availableDiskBytes returns the free bytes available to unprivileged
// processes (statfs f_bavail), excluding blocks reserved for the superuser.
func availableDiskBytes(path string) (uint64, error) {
	usage, err := disk.Usage(path)
	if err != nil {
		return 0, err
	}
	return usage.Free, nil
}
