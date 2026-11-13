//go:build !windows

package node

import (
	"os"

	"golang.org/x/sys/unix"
)

// O_NONBLOCK is ignored for regular files; no blocking open precedes fstat.
const (
	regularReadFlags   = os.O_RDONLY | unix.O_NONBLOCK
	directoryReadFlags = os.O_RDONLY | unix.O_NONBLOCK | unix.O_DIRECTORY
	regularWriteFlags  = os.O_WRONLY | os.O_CREATE | unix.O_NONBLOCK | unix.O_NOCTTY
)
