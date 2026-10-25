//go:build !windows

package processutil

import "os"

// RunAs reports "system" for a root process, such as a systemd system unit or
// launchd daemon, and "user" otherwise.
func RunAs() string {
	if os.Geteuid() == 0 {
		return "system"
	}
	return "user"
}
