package shellpty

import (
	"context"
	"errors"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

func startShell(t *testing.T, ctx context.Context, name string, cols, rows uint16, term, dir string) (*Terminal, *recorder) {
	t.Helper()
	requireConPTY(t)
	requireShell(t, name)
	tty, err := Start(ctx, name, cols, rows, term, dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tty.Close() })
	return tty, record(tty)
}

func requireConPTY(t *testing.T) {
	t.Helper()
	if err := conPTYSupport(); err != nil {
		t.Skip(err)
	}
}

func TestStartRejectsMissingConPTYExport(t *testing.T) {
	for _, missing := range []string{"CreatePseudoConsole", "ResizePseudoConsole", "ClosePseudoConsole"} {
		t.Run(missing, func(t *testing.T) {
			probe := func() error {
				return checkConPTYExports(func(name string) error {
					if name == missing {
						return windows.ERROR_PROC_NOT_FOUND
					}
					return nil
				})
			}
			term, err := startWithConPTYProbe(t.Context(), "cmd", 80, 24, "", "", probe)
			if term != nil {
				_ = term.Close()
				t.Fatal("started a shell without all required ConPTY exports")
			}
			if !errors.Is(err, ErrUnsupportedPlatform) || !strings.Contains(err.Error(), missing) || !strings.Contains(err.Error(), "control exec") {
				t.Fatalf("missing %s: got %v, want an actionable unsupported-platform error", missing, err)
			}
		})
	}
}

func TestStartOnWindowsWithoutConPTY(t *testing.T) {
	// Read the export independently of Start's probe. On Server 2016 this
	// exercises the same public entry point a remote shell request uses.
	kernel := windows.NewLazySystemDLL("kernel32.dll")
	if kernel.NewProc("CreatePseudoConsole").Find() == nil {
		t.Skip("this host exports CreatePseudoConsole")
	}
	for _, name := range []string{"cmd", "powershell"} {
		term, err := Start(t.Context(), name, 80, 24, "", "")
		if term != nil {
			_ = term.Close()
			t.Fatalf("Start(%q) created a shell without ConPTY", name)
		}
		if !errors.Is(err, ErrUnsupportedPlatform) || !strings.Contains(err.Error(), "CreatePseudoConsole") {
			t.Fatalf("Start(%q) = %v, want an unsupported-platform error", name, err)
		}
	}
}

func send(t *testing.T, tty *Terminal, line string) {
	t.Helper()
	if _, err := tty.Write([]byte(line + "\r")); err != nil {
		t.Fatal(err)
	}
}

func TestWindowsShellsAreFixed(t *testing.T) {
	if got := strings.Join(Names(), ","); got != "powershell,cmd,pwsh" {
		t.Fatalf("Names() = %s", got)
	}
	for _, name := range []string{"bash", "wsl", "cmd.exe", "powershell.exe", "PowerShell"} {
		if _, err := Start(context.Background(), name, 80, 24, "", ""); !errors.Is(err, ErrUnsupportedShell) {
			t.Errorf("Start(%q) = %v", name, err)
		}
	}
}

func TestCmdExitCodeAndDir(t *testing.T) {
	dir := t.TempDir()
	tty, out := startShell(t, context.Background(), "cmd", 100, 30, "", dir)
	// The caret keeps the echoed command line from matching the output.
	send(t, tty, "echo MARK^ER & cd & exit 7")
	got := out.eof(t)
	if !strings.Contains(got, "MARKER") {
		t.Errorf("output missing marker:\n%s", got)
	}
	if !strings.Contains(strings.ToLower(got), strings.ToLower(dir)) {
		t.Errorf("output missing starting directory %s:\n%s", dir, got)
	}
	if code, err := waitResult(t, tty); code != 7 || err != nil {
		t.Fatalf("Wait = %d, %v; want 7, nil", code, err)
	}
}

func TestCmdSkipsAutoRun(t *testing.T) {
	// cmd /D ignores AutoRun; an inherited environment variable cannot
	// enable it, so this checks the fixed argument vector.
	resolved, err := resolveShell("cmd")
	if err != nil {
		t.Fatal(err)
	}
	if len(resolved.args) != 2 || resolved.args[1] != "/D" {
		t.Fatalf("cmd args = %q", resolved.args)
	}
}

func TestPowerShellTermResizeAndExitCode(t *testing.T) {
	tty, out := startShell(t, context.Background(), "powershell", 80, 24, "xterm-test", "")
	send(t, tty, "Write-Output ('T<'+$env:TERM+'>')")
	out.waitFor(t, "T<xterm-test>")
	if err := tty.Resize(132, 43); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	send(t, tty, "Write-Output ('W<'+[Console]::WindowWidth+'x'+[Console]::WindowHeight+'>')")
	out.waitFor(t, "W<132x43>")
	send(t, tty, "Write-Output ('EN'+'D'); exit 5")
	if got := out.eof(t); !strings.Contains(got, "END") {
		t.Errorf("final output was not drained:\n%s", got)
	}
	if code, err := waitResult(t, tty); code != 5 || err != nil {
		t.Fatalf("Wait = %d, %v; want 5, nil", code, err)
	}
}

var pidPattern = regexp.MustCompile(`PID<(\d+)>`)

func requireExited(t *testing.T, output string) {
	t.Helper()
	match := pidPattern.FindStringSubmatch(output)
	if match == nil {
		t.Fatalf("no child PID in output:\n%s", output)
	}
	pid, _ := strconv.ParseUint(match[1], 10, 32)
	process, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		return // already gone
	}
	defer func() { _ = windows.CloseHandle(process) }()
	if event, _ := windows.WaitForSingleObject(process, 5000); event != windows.WAIT_OBJECT_0 {
		t.Fatalf("child %d still running", pid)
	}
}

const startChild = "$p = Start-Process -PassThru -NoNewWindow -FilePath ping.exe -ArgumentList '-n','1000','127.0.0.1'; Write-Output ('PID<'+$p.Id+'>')"

func TestExitTerminatesJobDescendants(t *testing.T) {
	tty, out := startShell(t, context.Background(), "powershell", 80, 24, "", "")
	send(t, tty, startChild)
	got := out.waitFor(t, ">\r")
	send(t, tty, "exit 0")
	out.eof(t)
	requireExited(t, got)
	if code, err := waitResult(t, tty); code != 0 || err != nil {
		t.Fatalf("Wait = %d, %v", code, err)
	}
}

func TestContextCancellationTerminatesJob(t *testing.T) {
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	tty, out := startShell(t, ctx, "powershell", 80, 24, "", "")
	send(t, tty, startChild)
	got := out.waitFor(t, ">\r")
	cause := errors.New("client disconnected")
	cancel(cause)
	out.eof(t)
	if code, err := waitResult(t, tty); code != -1 || !errors.Is(err, cause) {
		t.Fatalf("Wait = %d, %v; want -1, cause", code, err)
	}
	requireExited(t, got)
}

func TestCloseTerminatesBusyShell(t *testing.T) {
	tty, out := startShell(t, context.Background(), "cmd", 80, 24, "", "")
	send(t, tty, "echo READ^Y & ping -n 1000 127.0.0.1 >NUL")
	out.waitFor(t, "READY")
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
}

// Before Windows 11 24H2, ClosePseudoConsole waits for its final frame to be
// read. An unread terminal must still exit and close within the bound.
func TestWaitAndCloseWithoutReads(t *testing.T) {
	requireConPTY(t)
	requireShell(t, "cmd")
	tty, err := Start(context.Background(), "cmd", 80, 24, "", "")
	if err != nil {
		t.Fatal(err)
	}
	send(t, tty, "exit 4")
	if code, err := waitResult(t, tty); code != 4 || err != nil {
		t.Fatalf("Wait = %d, %v", code, err)
	}
	closeWithin(t, tty)
}

func TestPwshSession(t *testing.T) {
	tty, out := startShell(t, context.Background(), "pwsh", 100, 30, "", "")
	send(t, tty, "exit 6")
	out.eof(t)
	if code, err := waitResult(t, tty); code != 6 || err != nil {
		t.Fatalf("Wait = %d, %v", code, err)
	}
}
