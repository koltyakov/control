//go:build !windows

package client

import (
	"errors"
	"syscall"
)

func connectionRefused(err error) bool {
	return errors.Is(err, syscall.ECONNREFUSED)
}
