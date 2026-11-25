package vaultstore

import (
	"errors"
	"fmt"

	"golang.org/x/sys/unix"
)

// A POSIX ACL with only the three base entries is equivalent to the mode
// bits. Any named entry or mask can grant another account access, and a
// default ACL grants it on files created later.
func checkACL(fd int, path string) error {
	buf := make([]byte, 1024)
	for _, attr := range []string{"system.posix_acl_access", "system.posix_acl_default"} {
		n, err := unix.Fgetxattr(fd, attr, buf)
		if errors.Is(err, unix.ENODATA) || errors.Is(err, unix.EOPNOTSUPP) {
			continue
		}
		if err != nil {
			return fmt.Errorf("cannot inspect ACL of %s: %w", path, err)
		}
		// Header (4 bytes) plus 8 bytes per entry.
		if attr == "system.posix_acl_default" || n > 4+3*8 {
			return fmt.Errorf("%s has an ACL granting access to other accounts; remove it with setfacl -b", path)
		}
	}
	return nil
}

// sameRunningImage confirms the verified file is the image this process runs.
func sameRunningImage(fd int) error {
	var verified, running unix.Stat_t
	if err := unix.Fstat(fd, &verified); err != nil {
		return err
	}
	if err := unix.Stat("/proc/self/exe", &running); err != nil {
		return fmt.Errorf("cannot identify the running vault binary: %w", err)
	}
	if verified.Dev != running.Dev || verified.Ino != running.Ino {
		return errors.New("running vault binary differs from its verified path")
	}
	return nil
}

// defaultRoot is created by an administrator and owned by the broker.
const defaultRoot = "/var/lib/control-vault"
