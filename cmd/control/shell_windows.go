package main

import (
	"context"
	"errors"
	"io"
	"os"
	"runtime"
	"syscall"
	"time"
	"unicode/utf16"

	"golang.org/x/sys/windows"
)

var shellSignals = []os.Signal{os.Interrupt, syscall.SIGTERM}

var cancelConsoleRead = windows.NewLazySystemDLL("kernel32.dll").NewProc("CancelSynchronousIo")

func prepareShellTerminal(in, out int) (func() error, error) {
	input, output := windows.Handle(in), windows.Handle(out)
	var inputMode, outputMode uint32
	if err := windows.GetConsoleMode(input, &inputMode); err != nil {
		return nil, err
	}
	if err := windows.GetConsoleMode(output, &outputMode); err != nil {
		return nil, err
	}
	raw := inputMode &^ (windows.ENABLE_PROCESSED_INPUT | windows.ENABLE_LINE_INPUT | windows.ENABLE_ECHO_INPUT | windows.ENABLE_QUICK_EDIT_MODE)
	if err := windows.SetConsoleMode(input, raw|windows.ENABLE_EXTENDED_FLAGS|windows.ENABLE_VIRTUAL_TERMINAL_INPUT); err != nil {
		return nil, err
	}
	if err := windows.SetConsoleMode(output, outputMode|windows.ENABLE_PROCESSED_OUTPUT|windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING|windows.DISABLE_NEWLINE_AUTO_RETURN); err != nil {
		return nil, errors.Join(err, windows.SetConsoleMode(input, inputMode))
	}
	return func() error {
		return errors.Join(windows.SetConsoleMode(output, outputMode), windows.SetConsoleMode(input, inputMode))
	}, nil
}

func copyShellInput(ctx context.Context, in int, out io.Writer) error {
	// ReadConsoleW preserves Unicode and Ctrl+Z (os.Stdin.Read treats the
	// latter as EOF). Cancel the synchronous console read on its own thread.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	thread, err := windows.OpenThread(windows.THREAD_TERMINATE, false, windows.GetCurrentThreadId())
	if err != nil {
		return err
	}
	defer func() { _ = windows.CloseHandle(thread) }()
	done, stopped := make(chan struct{}), make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		defer close(stopped)
		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()
		for {
			// Cancellation may arrive between checking ctx and entering the
			// syscall; repeat until the reader has actually returned.
			_, _, _ = cancelConsoleRead.Call(uintptr(thread))
			select {
			case <-done:
				return
			case <-ticker.C:
			}
		}
	})
	defer func() {
		close(done)
		if !stop() {
			<-stopped
		}
	}()
	var buf [2048]uint16
	pending := 0
	for ctx.Err() == nil {
		var n uint32
		if err := windows.ReadConsole(windows.Handle(in), &buf[pending], uint32(len(buf)-pending), &n, nil); err != nil {
			return err
		}
		if n == 0 {
			return io.EOF
		}
		end := pending + int(n)
		pending = 0
		if buf[end-1] >= 0xd800 && buf[end-1] <= 0xdbff {
			pending = 1 // A UTF-16 surrogate pair may straddle two reads.
		}
		if end > pending {
			if _, err := io.WriteString(out, string(utf16.Decode(buf[:end-pending]))); err != nil {
				return err
			}
		}
		if pending != 0 {
			buf[0] = buf[end-1]
		}
	}
	return ctx.Err()
}
