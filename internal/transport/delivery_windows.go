//go:build windows

package transport

import "syscall"

// Winsock reports broken established connections with its own error numbers,
// which do not match the portable syscall constants used on Unix.
var brokenSocketErrors = []error{syscall.WSAECONNRESET, syscall.WSAECONNABORTED, syscall.ECONNRESET, syscall.ECONNABORTED, syscall.EPIPE}
