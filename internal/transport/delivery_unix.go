//go:build !windows

package transport

import "syscall"

// Socket failures of a connection that already existed. Refused or
// unreachable connects are not listed: those are setup outcomes classified
// where the setup fails.
var brokenSocketErrors = []error{syscall.ECONNRESET, syscall.ECONNABORTED, syscall.EPIPE, syscall.ENETRESET}
