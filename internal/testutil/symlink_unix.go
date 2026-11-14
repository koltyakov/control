//go:build !windows

package testutil

import (
	"errors"
	"syscall"
)

func symlinkUnavailable(err error) bool {
	return errors.Is(err, syscall.EPERM) ||
		errors.Is(err, syscall.ENOSYS) ||
		errors.Is(err, syscall.EOPNOTSUPP)
}
