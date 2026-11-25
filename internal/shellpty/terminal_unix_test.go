//go:build linux || darwin

package shellpty

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func startBash(t *testing.T, ctx context.Context, cols, rows uint16, term, dir string) *Terminal {
	t.Helper()
	requireShell(t, "bash")
	home := t.TempDir()
	for _, rc := range []string{".bashrc", ".bash_profile", ".profile"} {
		if err := os.WriteFile(filepath.Join(home, rc), []byte("echo RC_SOURCED\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOME", home)
	t.Setenv("BASH_SILENCE_DEPRECATION_WARNING", "1")
	tty, err := Start(ctx, "bash", cols, rows, term, dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tty.Close() })
	return tty
}

func write(t *testing.T, term *Terminal, input string) {
	t.Helper()
	if _, err := term.Write([]byte(input)); err != nil {
		t.Fatal(err)
	}
}

func TestBashSessionTermSizeDirAndExitCode(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	tty := startBash(t, context.Background(), 101, 33, "screen-256color", dir)
	out := record(tty)
	write(t, tty, `printf 'T<%s>\n' "$TERM"; stty size; printf 'D<%s>\n' "$(pwd -P)"; exit 7`+"\n")
	got := out.eof(t)
	for _, want := range []string{"T<screen-256color>", "33 101", "D<" + dir + ">"} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "RC_SOURCED") {
		t.Errorf("startup files were sourced:\n%s", got)
	}
	if code, err := waitResult(t, tty); code != 7 || err != nil {
		t.Fatalf("Wait = %d, %v; want 7, nil", code, err)
	}
	// Wait is repeatable.
	if code, err := tty.Wait(); code != 7 || err != nil {
		t.Fatalf("second Wait = %d, %v", code, err)
	}
}

func TestBashDefaultTerm(t *testing.T) {
	t.Setenv("TERM", "dumb")
	tty := startBash(t, context.Background(), 80, 24, "", "")
	out := record(tty)
	write(t, tty, `printf 'T<%s>\n' "$TERM"; exit`+"\n")
	if got := out.eof(t); !strings.Contains(got, "T<"+DefaultTerm+">") {
		t.Fatalf("TERM not defaulted:\n%s", got)
	}
}

func TestResize(t *testing.T) {
	tty := startBash(t, context.Background(), 80, 24, "", "")
	out := record(tty)
	if err := tty.Resize(132, 43); err != nil {
		t.Fatal(err)
	}
	if err := tty.Resize(0, 43); err == nil {
		t.Fatal("Resize accepted zero columns")
	}
	write(t, tty, "stty size; exit\n")
	if got := out.eof(t); !strings.Contains(got, "43 132") {
		t.Fatalf("resized size not reported:\n%s", got)
	}
	if code, err := waitResult(t, tty); code != 0 || err != nil {
		t.Fatalf("Wait = %d, %v", code, err)
	}
}

func TestOutputDrainsAfterExit(t *testing.T) {
	const size = 300000
	tty := startBash(t, context.Background(), 80, 24, "", "")
	out := record(tty)
	write(t, tty, "head -c "+strconv.Itoa(size)+" /dev/zero | tr '\\0' 'z'; exit 3\n")
	got := out.eof(t)
	if n := strings.Count(got, "z"); n < size {
		t.Fatalf("drained %d of %d bytes", n, size)
	}
	if code, err := waitResult(t, tty); code != 3 || err != nil {
		t.Fatalf("Wait = %d, %v", code, err)
	}
}

func TestSignalledShellReportsShellStyleCode(t *testing.T) {
	tty := startBash(t, context.Background(), 80, 24, "", "")
	out := record(tty)
	write(t, tty, "kill -KILL $$\n")
	out.eof(t)
	if code, err := waitResult(t, tty); code != 128+int(unix.SIGKILL) || err != nil {
		t.Fatalf("Wait = %d, %v", code, err)
	}
}

func TestContextCancellationTerminatesSession(t *testing.T) {
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	tty := startBash(t, ctx, 80, 24, "", "")
	out := record(tty)
	write(t, tty, "echo START''ED; sleep 10\n")
	out.waitFor(t, "STARTED")
	cause := errors.New("client disconnected")
	cancel(cause)
	out.ended(t)
	if code, err := waitResult(t, tty); code != -1 || !errors.Is(err, cause) {
		t.Fatalf("Wait = %d, %v; want -1, cause", code, err)
	}
}

func TestCloseTerminatesBusyShell(t *testing.T) {
	tty := startBash(t, context.Background(), 80, 24, "", "")
	out := record(tty)
	write(t, tty, "echo READY; sleep 1000\n")
	out.waitFor(t, "READY\r\n")
	closeWithin(t, tty)
	out.ended(t)
	if code, err := waitResult(t, tty); code != -1 || !errors.Is(err, ErrClosed) {
		t.Fatalf("Wait = %d, %v; want -1, ErrClosed", code, err)
	}
	if _, err := tty.Write([]byte("x")); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("Write after Close = %v", err)
	}
	if err := tty.Resize(80, 24); err == nil {
		t.Fatal("Resize after Close succeeded")
	}
	if err := tty.Close(); err != nil {
		t.Fatalf("second Close = %v", err)
	}
}

// macOS holds a closing terminal until its output is read, so an unread
// shell may not finish exiting; Close must still be bounded.
func TestCloseWithoutReadsIsBounded(t *testing.T) {
	tty := startBash(t, context.Background(), 80, 24, "", "")
	write(t, tty, "exit 4\n")
	time.Sleep(200 * time.Millisecond)
	closeWithin(t, tty)
	code, err := waitResult(t, tty)
	exited := code == 4 && err == nil
	closed := code == -1 && errors.Is(err, ErrClosed)
	if !exited && !closed {
		t.Fatalf("Wait = %d, %v; want 4, nil or -1, ErrClosed", code, err)
	}
}

func TestStartWithCancelledContext(t *testing.T) {
	requireShell(t, "bash")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if tty, err := Start(ctx, "bash", 80, 24, "", ""); !errors.Is(err, context.Canceled) {
		if tty != nil {
			_ = tty.Close()
		}
		t.Fatalf("Start = %v, want context.Canceled", err)
	}
}

func TestPwshSession(t *testing.T) {
	requireShell(t, "pwsh")
	t.Setenv("HOME", t.TempDir())
	tty, err := Start(context.Background(), "pwsh", 100, 30, "", "")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tty.Close() }()
	out := record(tty)
	write(t, tty, "exit 5\r")
	out.eof(t)
	if code, err := waitResult(t, tty); code != 5 || err != nil {
		t.Fatalf("Wait = %d, %v", code, err)
	}
}
