//go:build !windows

package main

import (
	"context"
	"errors"
	"io"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
	"golang.org/x/term"
)

var shellSignals = []os.Signal{os.Interrupt, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT, syscall.SIGTSTP}

func prepareShellTerminal(in, _ int) (func() error, error) {
	state, err := term.MakeRaw(in)
	if err != nil {
		return nil, err
	}
	return func() error { return term.Restore(in, state) }, nil
}

func copyShellInput(ctx context.Context, in int, out io.Writer) error {
	poll := []unix.PollFd{{Fd: int32(in), Events: unix.POLLIN}}
	var buf [8192]byte
	for ctx.Err() == nil {
		n, err := unix.Poll(poll, 100)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return err
		}
		if n == 0 || ctx.Err() != nil {
			continue
		}
		n, err = unix.Read(in, buf[:])
		if errors.Is(err, unix.EINTR) || errors.Is(err, unix.EAGAIN) {
			continue
		}
		if err != nil {
			return err
		}
		if n == 0 {
			return io.EOF
		}
		if _, err = out.Write(buf[:n]); err != nil {
			return err
		}
	}
	return ctx.Err()
}
