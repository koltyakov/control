//go:build !windows

package node

import (
	"os"

	"golang.org/x/sys/unix"
)

// O_NONBLOCK is ignored for regular files; no blocking open precedes fstat.
const regularReadFlags = os.O_RDONLY | unix.O_NONBLOCK
