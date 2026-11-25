package shellpty

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"sync"

	"github.com/ebitengine/purego"
	"golang.org/x/sys/unix"
)

const (
	getTermios = unix.TIOCGETA
	setTermios = unix.TIOCSETA
)

// openPTY returns a non-blocking master, which the runtime poller can
// interrupt on Close, and its blocking slave. Both are close-on-exec. These
// ioctls are the CGO-free equivalents of grantpt and unlockpt.
func openPTY() (*os.File, *os.File, error) {
	fd, err := unix.Open("/dev/ptmx", unix.O_RDWR|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("open pseudo-terminal: %w", err)
	}
	fail := func(op string, err error) (*os.File, *os.File, error) {
		_ = unix.Close(fd)
		return nil, nil, fmt.Errorf("%s: %w", op, err)
	}
	if err := unix.IoctlSetInt(fd, unix.TIOCPTYGRANT, 0); err != nil {
		return fail("grant pseudo-terminal", err)
	}
	if err := unix.IoctlSetInt(fd, unix.TIOCPTYUNLK, 0); err != nil {
		return fail("unlock pseudo-terminal", err)
	}
	path, err := ptsname(fd)
	if err != nil {
		return fail("pseudo-terminal name", err)
	}
	sfd, err := unix.Open(path, unix.O_RDWR|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
	if err != nil {
		return fail("open "+path, err)
	}
	if err := unix.SetNonblock(fd, true); err != nil {
		_ = unix.Close(sfd)
		return fail("pseudo-terminal mode", err)
	}
	return os.NewFile(uintptr(fd), "/dev/ptmx"), os.NewFile(uintptr(sfd), path), nil
}

// macOS has no pidfd. Rely on the kernel's terminal hangup rather than
// signalling other processes by a PID that could have been reused.
func waitAndCleanSession(int) {}

// libSystem's ptsname_r replaces the TIOCPTYGNAME ioctl, because macOS does
// not support direct system calls. purego calls it without CGO.
var ptsnameR = sync.OnceValues(func() (func(int32, *byte, uintptr) int32, error) {
	lib, err := purego.Dlopen("/usr/lib/libSystem.B.dylib", purego.RTLD_NOW|purego.RTLD_GLOBAL)
	if err != nil {
		return nil, err
	}
	var fn func(int32, *byte, uintptr) int32
	symbol, err := purego.Dlsym(lib, "ptsname_r")
	if err != nil {
		return nil, err
	}
	purego.RegisterFunc(&fn, symbol)
	return fn, nil
})

func ptsname(fd int) (string, error) {
	fn, err := ptsnameR()
	if err != nil {
		return "", err
	}
	buf := make([]byte, 128)
	if fn(int32(fd), &buf[0], uintptr(len(buf))) != 0 {
		return "", errors.New("ptsname_r failed")
	}
	end := bytes.IndexByte(buf, 0)
	if end <= 0 {
		return "", unix.EINVAL
	}
	return string(buf[:end]), nil
}
