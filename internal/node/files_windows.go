package node

import "os"

const (
	regularReadFlags   = os.O_RDONLY
	directoryReadFlags = os.O_RDONLY
	regularWriteFlags  = os.O_WRONLY | os.O_CREATE
)
