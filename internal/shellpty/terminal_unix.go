//go:build linux || darwin

package shellpty

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// Terminal is a running shell attached to a pseudo-terminal. Read returns
// terminal output, Write sends input, and both are safe to use concurrently
// with each other and with Resize, Wait, and Close.
type Terminal struct {
	master *os.File
	cmd    *exec.Cmd

	stopContext func() bool
	closeOnce   sync.Once
	closeErr    error

	mu     sync.Mutex
	reaped bool
	reason error

	exited chan struct{}
	code   int
	err    error
}

// Start runs the named shell on a new pseudo-terminal of cols by rows. term
// sets TERM, defaulting to DefaultTerm when empty; dir is the absolute
// starting directory, or empty for the current process directory. The shell
// leads a new session with the terminal as its controlling terminal.
func Start(ctx context.Context, name string, cols, rows uint16, term, dir string) (*Terminal, error) {
	resolved, dir, err := validateStart(name, cols, rows, term, dir)
	if err != nil {
		return nil, err
	}
	if err := context.Cause(ctx); err != nil {
		return nil, err
	}
	if term == "" {
		term = DefaultTerm
	}
	master, slave, err := openPTY()
	if err != nil {
		return nil, err
	}
	// The child holds its own copies; the parent's slave must close so that
	// master reads reach EOF once every session process has exited.
	defer func() { _ = slave.Close() }()
	if err := setSize(master, cols, rows); err != nil {
		_ = master.Close()
		return nil, err
	}
	enableUTF8(slave)
	cmd := &exec.Cmd{
		Path:   resolved.path,
		Args:   resolved.args,
		Env:    withTerm(os.Environ(), term, false),
		Dir:    dir,
		Stdin:  slave,
		Stdout: slave,
		Stderr: slave,
		// Ctty names the child's descriptor 0, which is the slave.
		SysProcAttr: &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0},
	}
	if err := cmd.Start(); err != nil {
		_ = master.Close()
		return nil, fmt.Errorf("start %s: %w", name, err)
	}
	t := &Terminal{
		master: master,
		cmd:    cmd,
		exited: make(chan struct{}),
	}
	go t.reap()
	t.stopContext = context.AfterFunc(ctx, func() {
		t.terminate(context.Cause(ctx))
		_ = t.master.Close()
	})
	return t, nil
}

// Read returns terminal output. It reports io.EOF when the slave closes and
// buffered output is drained.
func (t *Terminal) Read(p []byte) (int, error) {
	n, err := t.master.Read(p)
	// Linux reports EIO once the last slave descriptor is closed.
	if errors.Is(err, syscall.EIO) {
		err = io.EOF
	}
	return n, err
}

// Write sends input to the shell.
func (t *Terminal) Write(p []byte) (int, error) {
	n, err := t.master.Write(p)
	if errors.Is(err, syscall.EIO) {
		err = io.ErrClosedPipe
	}
	return n, err
}

// Resize changes the terminal size; the foreground process group receives
// SIGWINCH.
func (t *Terminal) Resize(cols, rows uint16) error {
	if err := ValidateSize(cols, rows); err != nil {
		return err
	}
	return setSize(t.master, cols, rows)
}

// Wait blocks until the shell has exited and process cleanup has finished.
// A shell exit, including a nonzero one, returns its code and a nil
// error; a shell killed by a signal it did not receive from Close or context
// cancellation reports 128 plus the signal number. Termination by Close
// returns ErrClosed, and by context cancellation the context's cause, with
// code -1. Wait may be called repeatedly and concurrently.
func (t *Terminal) Wait() (int, error) {
	<-t.exited
	return t.code, t.err
}

// Done closes when the shell exit status is available, independently of EOF.
func (t *Terminal) Done() <-chan struct{} { return t.exited }

// Close terminates the shell and its session, then releases the terminal.
// Pending reads and writes return errors wrapping os.ErrClosed. Close waits
// at most a few seconds for the shell to be reaped.
func (t *Terminal) Close() error {
	t.closeOnce.Do(func() {
		t.stopContext()
		t.terminate(ErrClosed)
		t.closeErr = t.master.Close()
		if errors.Is(t.closeErr, os.ErrClosed) {
			t.closeErr = nil
		}
		select {
		case <-t.exited:
		case <-time.After(closeTimeout):
		}
	})
	return t.closeErr
}

func (t *Terminal) reap() {
	waitAndCleanSession(t.cmd.Process.Pid)
	err := t.cmd.Wait()
	t.mu.Lock()
	t.reaped = true
	reason := t.reason
	t.mu.Unlock()
	if reason != nil {
		t.code, t.err = -1, reason
	} else {
		t.code, t.err = exitStatus(err)
	}
	close(t.exited)
}

// terminate signals only the child owned by os.Process, which synchronizes
// signalling with Wait. The first reason before reap is reported by Wait.
func (t *Terminal) terminate(reason error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.reaped || t.reason != nil {
		return
	}
	t.reason = reason
	_ = t.cmd.Process.Kill()
}

func exitStatus(err error) (int, error) {
	if err == nil {
		return 0, nil
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		return -1, err
	}
	if status, ok := exitErr.Sys().(syscall.WaitStatus); ok && status.Signaled() {
		return 128 + int(status.Signal()), nil
	}
	return exitErr.ExitCode(), nil
}

func setSize(master *os.File, cols, rows uint16) error {
	conn, err := master.SyscallConn()
	if err != nil {
		return err
	}
	var ioctlErr error
	if err := conn.Control(func(fd uintptr) {
		ioctlErr = unix.IoctlSetWinsize(int(fd), unix.TIOCSWINSZ, &unix.Winsize{Row: rows, Col: cols})
	}); err != nil {
		return err
	}
	if ioctlErr != nil {
		return fmt.Errorf("resize terminal: %w", ioctlErr)
	}
	return nil
}

// enableUTF8 lets the line discipline erase whole UTF-8 characters. Failure
// leaves the kernel's default modes, which remain usable.
func enableUTF8(slave *os.File) {
	fd := int(slave.Fd())
	termios, err := unix.IoctlGetTermios(fd, getTermios)
	if err != nil {
		return
	}
	termios.Iflag |= unix.IUTF8
	_ = unix.IoctlSetTermios(fd, setTermios, termios)
}
