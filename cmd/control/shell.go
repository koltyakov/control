package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/signal"
	"strings"
	"sync"
	"time"

	"github.com/koltyakov/control/internal/client"
	"github.com/koltyakov/control/internal/model"
	"golang.org/x/term"
)

const shellUsage = "usage: control shell NODE [--] bash|cmd|powershell|pwsh"

// main applies this status only after run has released the client and terminal.
type shellExitStatus int

func (e shellExitStatus) Error() string { return fmt.Sprintf("remote shell exited with status %d", e) }

func parseShell(args []string) (node, shell string, err error) {
	if len(args) == 3 && args[1] == "--" {
		node, shell = args[0], args[2]
	} else if len(args) == 2 {
		node, shell = args[0], args[1]
	}
	if strings.TrimSpace(node) == "" || strings.HasPrefix(node, "-") || !model.ValidShellName(shell) {
		return "", "", errors.New(shellUsage)
	}
	return node, shell, nil
}

type shellSize struct{ cols, rows uint16 }

func terminalSize(fd int) (shellSize, error) {
	cols, rows, err := term.GetSize(fd)
	if err != nil {
		return shellSize{}, fmt.Errorf("read terminal size: %w", err)
	}
	if cols < 1 || rows < 1 || cols > math.MaxInt16 || rows > math.MaxInt16 {
		return shellSize{}, fmt.Errorf("invalid terminal size %dx%d", cols, rows)
	}
	return shellSize{uint16(cols), uint16(rows)}, nil
}

func shellCLI(ctx context.Context, c client.Client, args []string) (result error) {
	node, shell, err := parseShell(args)
	if err != nil {
		return err
	}
	in, out := int(os.Stdin.Fd()), int(os.Stdout.Fd())
	if !term.IsTerminal(in) || !term.IsTerminal(out) {
		return errors.New("shell requires a terminal on both stdin and stdout")
	}
	size, err := terminalSize(out)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(ctx, shellSignals...)
	defer stop()
	restoreLogs := suspendTerminalLogs(term.IsTerminal(int(os.Stderr.Fd())))
	defer restoreLogs()
	defer func() { _ = c.Close() }()
	request := model.ShellRequest{Shell: shell, Term: env("TERM", "xterm-256color"), Cols: size.cols, Rows: size.rows}
	remote, err := c.OpenShell(ctx, node, request)
	if err != nil {
		return err
	}
	// Keep Ctrl+C usable locally while connecting. Once raw, the worker's PTY
	// receives it as input and decides which foreground process to interrupt.
	restore, err := prepareShellTerminal(in, out)
	if err != nil {
		_ = remote.Close()
		return fmt.Errorf("prepare terminal: %w", err)
	}
	defer func() {
		if err := restore(); err != nil {
			result = fmt.Errorf("restore terminal: %w (shell result: %v)", err, result)
		}
	}()
	return relayShell(ctx, remote, os.Stdout, func(ctx context.Context, w io.Writer) error {
		return copyShellInput(ctx, in, w)
	}, size, func() (shellSize, error) { return terminalSize(out) })
}

type shellStream interface {
	io.ReadWriteCloser
	Resize(cols, rows uint16) error
	ExitCode() (int, bool)
}

var errShellInputWrite = errors.New("could not write to remote shell")

type shellInputWriter struct{ io.Writer }

func (w shellInputWriter) Write(p []byte) (int, error) {
	n, err := w.Writer.Write(p)
	if err != nil {
		err = errors.Join(errShellInputWrite, err)
	}
	return n, err
}

// relayShell owns remote. Closing it interrupts reads, writes and resizes;
// local input also observes cancellation even when no key is pressed.
func relayShell(ctx context.Context, remote shellStream, out io.Writer, input func(context.Context, io.Writer) error, size shellSize, getSize func() (shellSize, error)) error {
	ctx, cancel := context.WithCancel(ctx)
	var pumps sync.WaitGroup
	result := make(chan error, 2)
	pumps.Go(func() {
		_, err := io.Copy(out, remote)
		if err == nil {
			if code, ok := remote.ExitCode(); !ok {
				err = errors.New("remote shell closed without an exit status")
			} else if code != 0 {
				err = shellExitStatus(code)
			}
		}
		result <- err
	})
	pumps.Go(func() {
		if err := input(ctx, shellInputWriter{remote}); !errors.Is(err, errShellInputWrite) {
			result <- err
		}
		// The worker can stop accepting input before the output pump
		// consumes its final bytes and status. Let that pump finish.
	})
	pumps.Go(func() {
		ticker := time.NewTicker(150 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				next, err := getSize()
				// A minimized or detaching terminal can briefly have no size.
				if err != nil || next == size {
					continue
				}
				if err := remote.Resize(next.cols, next.rows); err != nil {
					return
				}
				size = next
			}
		}
	})
	defer func() {
		cancel()
		_ = remote.Close()
		pumps.Wait()
	}()
	select {
	case err := <-result:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}
