package shellpty

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// pipeBuffer is the requested kernel buffer for each pseudo-console pipe.
const pipeBuffer = 64 * 1024

// Terminal is a running shell attached to a pseudo console. Read returns
// terminal output, Write sends input, and both are safe to use concurrently
// with each other and with Resize, Wait, and Close.
type Terminal struct {
	input  *os.File
	output *os.File

	stopContext func() bool
	closeOnce   sync.Once
	closeErr    error

	mu      sync.Mutex
	console windows.Handle
	job     windows.Handle
	process windows.Handle
	exited  bool
	reason  error

	exitCh chan struct{}
	done   chan struct{}
	code   int
	err    error
}

// Start runs the named shell on a new pseudo console of cols by rows. A
// non-empty term sets TERM; empty leaves the inherited environment unchanged.
// dir is the absolute starting directory, or empty for the current process
// directory. The shell and every process it starts belong to a job object
// that is terminated when the shell exits or the terminal closes.
func Start(ctx context.Context, name string, cols, rows uint16, term, dir string) (*Terminal, error) {
	resolved, dir, err := validateStart(name, cols, rows, term, dir)
	if err != nil {
		return nil, err
	}
	if err := context.Cause(ctx); err != nil {
		return nil, err
	}
	var env []string
	if term != "" {
		env = withTerm(os.Environ(), term, true)
	}
	t := &Terminal{exitCh: make(chan struct{}), done: make(chan struct{})}
	ok := false
	defer func() {
		if !ok {
			t.release()
		}
	}()
	// The pseudo console duplicates its pipe ends, so the parent's copies
	// close after creation. The parent ends are overlapped, letting Close
	// cancel pending reads and writes through the runtime poller.
	var inputTheirs, outputTheirs windows.Handle
	if t.input, inputTheirs, err = pipePair(windows.PIPE_ACCESS_OUTBOUND, windows.GENERIC_READ, "input"); err != nil {
		return nil, err
	}
	defer func() { _ = windows.CloseHandle(inputTheirs) }()
	if t.output, outputTheirs, err = pipePair(windows.PIPE_ACCESS_INBOUND, windows.GENERIC_WRITE, "output"); err != nil {
		return nil, err
	}
	defer func() { _ = windows.CloseHandle(outputTheirs) }()
	if err := windows.CreatePseudoConsole(coord(cols, rows), inputTheirs, outputTheirs, 0, &t.console); err != nil {
		t.console = 0
		return nil, fmt.Errorf("create pseudo console: %w", err)
	}
	if t.job, err = newJob(); err != nil {
		return nil, err
	}
	if t.process, err = t.spawn(resolved, dir, env); err != nil {
		return nil, fmt.Errorf("start %s: %w", name, err)
	}
	ok = true
	go t.reap()
	t.stopContext = context.AfterFunc(ctx, func() { t.terminate(context.Cause(ctx)) })
	return t, nil
}

// spawn starts the shell suspended, assigns it to the job before it can run
// or create children, then resumes it.
func (t *Terminal) spawn(resolved shell, dir string, env []string) (windows.Handle, error) {
	attrs, err := windows.NewProcThreadAttributeList(1)
	if err != nil {
		return 0, err
	}
	defer attrs.Delete()
	// The attribute value is the HPCON itself, not a pointer to it.
	if err := attrs.Update(windows.PROC_THREAD_ATTRIBUTE_PSEUDOCONSOLE, *(*unsafe.Pointer)(unsafe.Pointer(&t.console)), unsafe.Sizeof(t.console)); err != nil {
		return 0, err
	}
	si := &windows.StartupInfoEx{ProcThreadAttributeList: attrs.List()}
	si.Cb = uint32(unsafe.Sizeof(*si))
	// Invalid standard handles keep a redirected node service's stdio, such
	// as a log file, from being passed to the shell instead of the console.
	si.Flags = windows.STARTF_USESTDHANDLES
	si.StdInput, si.StdOutput, si.StdErr = windows.InvalidHandle, windows.InvalidHandle, windows.InvalidHandle
	app, err := windows.UTF16PtrFromString(resolved.path)
	if err != nil {
		return 0, err
	}
	commandLine, err := windows.UTF16PtrFromString(windows.ComposeCommandLine(resolved.args))
	if err != nil {
		return 0, err
	}
	var dirPtr *uint16
	if dir != "" {
		if dirPtr, err = windows.UTF16PtrFromString(dir); err != nil {
			return 0, err
		}
	}
	envPtr, err := envBlock(env)
	if err != nil {
		return 0, err
	}
	flags := uint32(windows.CREATE_SUSPENDED | windows.EXTENDED_STARTUPINFO_PRESENT | windows.CREATE_UNICODE_ENVIRONMENT)
	var info windows.ProcessInformation
	if err := windows.CreateProcess(app, commandLine, nil, nil, false, flags, envPtr, dirPtr, &si.StartupInfo, &info); err != nil {
		return 0, err
	}
	defer func() { _ = windows.CloseHandle(info.Thread) }()
	if err := windows.AssignProcessToJobObject(t.job, info.Process); err != nil {
		_ = windows.TerminateProcess(info.Process, 1)
		_ = windows.CloseHandle(info.Process)
		return 0, fmt.Errorf("assign job: %w", err)
	}
	if _, err := windows.ResumeThread(info.Thread); err != nil {
		_ = windows.TerminateJobObject(t.job, 1)
		_ = windows.CloseHandle(info.Process)
		return 0, fmt.Errorf("resume: %w", err)
	}
	return info.Process, nil
}

// Read returns terminal output. It reports io.EOF after the shell has exited,
// its job has been terminated, and the pseudo console's final output is
// drained.
func (t *Terminal) Read(p []byte) (int, error) {
	return t.output.Read(p)
}

// Write sends input to the shell. Enter is a carriage return.
func (t *Terminal) Write(p []byte) (int, error) {
	n, err := t.input.Write(p)
	if errors.Is(err, windows.ERROR_NO_DATA) || errors.Is(err, windows.ERROR_BROKEN_PIPE) {
		err = io.ErrClosedPipe
	}
	return n, err
}

// Resize changes the pseudo console size. It fails once the shell has exited
// or the terminal is closed.
func (t *Terminal) Resize(cols, rows uint16) error {
	if err := ValidateSize(cols, rows); err != nil {
		return err
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.console == 0 {
		return fmt.Errorf("resize terminal: %w", os.ErrClosed)
	}
	if err := windows.ResizePseudoConsole(t.console, coord(cols, rows)); err != nil {
		return fmt.Errorf("resize terminal: %w", err)
	}
	return nil
}

// Wait blocks until the shell has exited and its job has been terminated. A
// shell exit, including a nonzero one, returns its code and a nil error.
// Termination by Close returns ErrClosed, and by context cancellation the
// context's cause, with code -1. Wait does not wait for output to be read and
// may be called repeatedly and concurrently.
func (t *Terminal) Wait() (int, error) {
	<-t.exitCh
	return t.code, t.err
}

// Done closes when the shell exit status is available, independently of EOF.
func (t *Terminal) Done() <-chan struct{} { return t.exitCh }

// Close terminates the shell and its job, then releases the pseudo console.
// Pending reads and writes return errors wrapping os.ErrClosed. Close waits
// at most a few seconds for the pseudo console to shut down.
func (t *Terminal) Close() error {
	t.closeOnce.Do(func() {
		t.stopContext()
		t.terminate(ErrClosed)
		// Closing the parent's pipe ends lets ClosePseudoConsole finish even
		// when nobody drains the final frame.
		t.closeErr = errors.Join(t.input.Close(), t.output.Close())
		select {
		case <-t.done:
		case <-time.After(closeTimeout):
		}
	})
	return t.closeErr
}

// reap records the shell's exit, terminates its remaining job processes, and
// then closes the pseudo console. ClosePseudoConsole can block until its
// final frame is read, which is why Wait is released first and Close closes
// the output pipe before waiting.
func (t *Terminal) reap() {
	var code uint32
	_, err := windows.WaitForSingleObject(t.process, windows.INFINITE)
	if err == nil {
		err = windows.GetExitCodeProcess(t.process, &code)
	}
	t.mu.Lock()
	t.exited = true
	reason := t.reason
	_ = windows.TerminateJobObject(t.job, 1)
	console := t.console
	t.console = 0
	t.mu.Unlock()
	switch {
	case reason != nil:
		t.code, t.err = -1, reason
	case err != nil:
		t.code, t.err = -1, fmt.Errorf("wait for shell: %w", err)
	default:
		t.code = int(code)
	}
	close(t.exitCh)
	windows.ClosePseudoConsole(console)
	t.mu.Lock()
	_ = windows.CloseHandle(t.process)
	_ = windows.CloseHandle(t.job)
	t.process, t.job = 0, 0
	t.mu.Unlock()
	close(t.done)
}

// terminate kills the job. The first reason recorded before the shell exits
// is reported by Wait.
func (t *Terminal) terminate(reason error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.exited && t.reason == nil {
		t.reason = reason
	}
	if t.job != 0 {
		_ = windows.TerminateJobObject(t.job, 1)
	}
}

// release frees resources after a failed Start, before reap owns them.
func (t *Terminal) release() {
	if t.process != 0 {
		_ = windows.TerminateProcess(t.process, 1)
		_ = windows.CloseHandle(t.process)
	}
	if t.job != 0 {
		_ = windows.CloseHandle(t.job)
	}
	// Close the output pipe first so ClosePseudoConsole cannot wait on it.
	if t.output != nil {
		_ = t.output.Close()
	}
	if t.console != 0 {
		windows.ClosePseudoConsole(t.console)
	}
	if t.input != nil {
		_ = t.input.Close()
	}
}

func coord(cols, rows uint16) windows.Coord {
	return windows.Coord{X: int16(cols), Y: int16(rows)}
}

func newJob() (windows.Handle, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return 0, fmt.Errorf("create job: %w", err)
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation, uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		_ = windows.CloseHandle(job)
		return 0, fmt.Errorf("configure job: %w", err)
	}
	return job, nil
}

// pipePair creates a single-instance named pipe whose server end, returned as
// an overlapped *os.File, belongs to this process, and whose synchronous
// client end is handed to the pseudo console. The random name, first-instance
// flag, single instance, local-only clients, and an owner-only DACL keep other
// processes from creating or connecting to the pipe.
func pipePair(direction, clientAccess uint32, label string) (*os.File, windows.Handle, error) {
	var suffix [16]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return nil, 0, err
	}
	name := `\\.\pipe\control-shellpty-` + hex.EncodeToString(suffix[:])
	name16, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return nil, 0, err
	}
	sa, err := ownerOnly()
	if err != nil {
		return nil, 0, err
	}
	server, err := windows.CreateNamedPipe(name16,
		direction|windows.FILE_FLAG_OVERLAPPED|windows.FILE_FLAG_FIRST_PIPE_INSTANCE,
		windows.PIPE_TYPE_BYTE|windows.PIPE_READMODE_BYTE|windows.PIPE_WAIT|windows.PIPE_REJECT_REMOTE_CLIENTS,
		1, pipeBuffer, pipeBuffer, 0, sa)
	if err != nil {
		return nil, 0, fmt.Errorf("create %s pipe: %w", label, err)
	}
	// A client that connects before ConnectNamedPipe leaves the instance
	// connected, so no separate connect step is needed.
	client, err := windows.CreateFile(name16, clientAccess, 0, sa, windows.OPEN_EXISTING,
		windows.SECURITY_SQOS_PRESENT|windows.SECURITY_IDENTIFICATION, 0)
	if err != nil {
		_ = windows.CloseHandle(server)
		return nil, 0, fmt.Errorf("connect %s pipe: %w", label, err)
	}
	return os.NewFile(uintptr(server), "shellpty-"+label), client, nil
}

// ownerOnly returns non-inheritable security attributes granting access only
// to the current process user.
func ownerOnly() (*windows.SecurityAttributes, error) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return nil, fmt.Errorf("current user: %w", err)
	}
	sd, err := windows.SecurityDescriptorFromString("D:P(A;;GA;;;" + user.User.Sid.String() + ")")
	if err != nil {
		return nil, fmt.Errorf("pipe security: %w", err)
	}
	sa := &windows.SecurityAttributes{SecurityDescriptor: sd}
	sa.Length = uint32(unsafe.Sizeof(*sa))
	return sa, nil
}

// envBlock encodes env as a CREATE_UNICODE_ENVIRONMENT block. A nil env
// inherits the parent environment.
func envBlock(env []string) (*uint16, error) {
	if env == nil {
		return nil, nil
	}
	var block []uint16
	for _, entry := range env {
		if strings.IndexByte(entry, 0) >= 0 {
			return nil, errors.New("environment contains a NUL byte")
		}
		encoded, err := windows.UTF16FromString(entry)
		if err != nil {
			return nil, err
		}
		block = append(block, encoded...)
	}
	// An empty block still needs its double terminator.
	block = append(block, 0)
	if len(env) == 0 {
		block = append(block, 0)
	}
	return &block[0], nil
}
