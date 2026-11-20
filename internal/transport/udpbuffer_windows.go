package transport

import (
	"net"

	"golang.org/x/sys/windows"
)

const udpBufferHint = "Windows rejected the larger socket buffer request"

func setUDPBuffers(conn *net.UDPConn, size int) int {
	_ = conn.SetReadBuffer(size)
	_ = conn.SetWriteBuffer(size)
	raw, err := conn.SyscallConn()
	if err != nil {
		return 0
	}
	effective := 0
	_ = raw.Control(func(fd uintptr) {
		if value, err := windows.GetsockoptInt(windows.Handle(fd), windows.SOL_SOCKET, windows.SO_RCVBUF); err == nil {
			effective = value
		}
	})
	return effective
}
