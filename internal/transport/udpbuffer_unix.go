//go:build unix && !linux

package transport

import (
	"net"

	"golang.org/x/sys/unix"
)

const udpBufferHint = "raise kern.ipc.maxsockbuf"

// setUDPBuffers halves a rejected request: macOS fails, rather than clamps,
// sizes above kern.ipc.maxsockbuf.
func setUDPBuffers(conn *net.UDPConn, size int) int {
	for request := size; request >= 256<<10; request /= 2 {
		if conn.SetReadBuffer(request) == nil {
			break
		}
	}
	for request := size; request >= 256<<10; request /= 2 {
		if conn.SetWriteBuffer(request) == nil {
			break
		}
	}
	raw, err := conn.SyscallConn()
	if err != nil {
		return 0
	}
	effective := 0
	_ = raw.Control(func(fd uintptr) {
		if value, err := unix.GetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_RCVBUF); err == nil {
			effective = value
		}
	})
	return effective
}
