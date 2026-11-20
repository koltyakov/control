package transport

import (
	"net"

	"golang.org/x/sys/unix"
)

const udpBufferHint = "raise net.core.rmem_max and net.core.wmem_max, or run the node with CAP_NET_ADMIN"

// setUDPBuffers uses the privileged FORCE options when available, so a root
// or CAP_NET_ADMIN node is not capped by net.core.rmem_max. Otherwise the
// kernel silently clamps the ordinary request to that limit. It returns the
// effective receive size; Linux reports twice the usable bytes.
func setUDPBuffers(conn *net.UDPConn, size int) int {
	raw, err := conn.SyscallConn()
	if err != nil {
		return 0
	}
	effective := 0
	_ = raw.Control(func(fd uintptr) {
		socket := int(fd)
		if unix.SetsockoptInt(socket, unix.SOL_SOCKET, unix.SO_RCVBUFFORCE, size) != nil {
			_ = unix.SetsockoptInt(socket, unix.SOL_SOCKET, unix.SO_RCVBUF, size)
		}
		if unix.SetsockoptInt(socket, unix.SOL_SOCKET, unix.SO_SNDBUFFORCE, size) != nil {
			_ = unix.SetsockoptInt(socket, unix.SOL_SOCKET, unix.SO_SNDBUF, size)
		}
		if value, err := unix.GetsockoptInt(socket, unix.SOL_SOCKET, unix.SO_RCVBUF); err == nil {
			effective = value / 2
		}
	})
	return effective
}
