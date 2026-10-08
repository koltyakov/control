package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows/svc"
)

func TestWindowsServiceWaitsForGracefulShutdown(t *testing.T) {
	for _, command := range []svc.Cmd{svc.Stop, svc.Shutdown} {
		t.Run(commandName(command), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			cancelled := make(chan struct{})
			release := make(chan struct{}, 1)
			defer close(release)
			handler := &windowsNodeService{ctx: ctx, run: func(ctx context.Context) error {
				<-ctx.Done()
				close(cancelled)
				select {
				case <-release:
				case <-time.After(4 * time.Second):
				}
				return nil
			}}
			requests := make(chan svc.ChangeRequest, 2)
			statuses := make(chan svc.Status, 16)
			done := make(chan uint32, 1)
			go func() { _, code := handler.Execute(nil, requests, statuses); done <- code }()
			awaitServiceState(t, ctx, statuses, svc.Running)
			requests <- svc.ChangeRequest{Cmd: svc.Interrogate}
			awaitServiceState(t, ctx, statuses, svc.Running)
			requests <- svc.ChangeRequest{Cmd: command}
			awaitServiceState(t, ctx, statuses, svc.StopPending)
			select {
			case <-cancelled:
			case <-ctx.Done():
				t.Fatal("shutdown did not cancel the supervisor")
			}
			select {
			case <-done:
				t.Fatal("SCM handler returned before the supervisor stopped")
			default:
			}
			// Release the callback without closing the channel twice on failure.
			release <- struct{}{}
			select {
			case code := <-done:
				if code != 0 {
					t.Fatalf("graceful stop returned %d", code)
				}
			case <-ctx.Done():
				t.Fatal("service did not finish")
			}
		})
	}
}

func TestWindowsServiceReportsFailureForRecovery(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	handler := &windowsNodeService{ctx: ctx, run: func(context.Context) error {
		return errors.New("supervisor failed")
	}}
	specific, code := handler.Execute(nil, make(chan svc.ChangeRequest), make(chan svc.Status, 16))
	if !specific || code == 0 {
		t.Fatal("service failure must trigger SCM recovery")
	}
}

func TestWindowsUserBootstrapStartsChildWithoutConsole(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	dir := filepath.Join(t.TempDir(), "user's profile with spaces")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(dir, "probe.go")
	code := `package main
import ("fmt"; "os"; "syscall")
func main() {
 window, _, _ := syscall.NewLazyDLL("kernel32.dll").NewProc("GetConsoleWindow").Call()
 if window != 0 { os.Exit(99) }
 if len(os.Args) != 3 || os.Args[1] != "__user" { os.Exit(98) }
 fmt.Println("no-console child:", os.Args[2])
 if os.Getenv("CONTROL_TEST_BOOTSTRAP_FAILURE") == "1" { os.Exit(1) }
}`
	if err := os.WriteFile(source, []byte(code), 0600); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(dir, "console probe.exe")
	if output, err := exec.CommandContext(ctx, "go", "build", "-o", binary, source).CombinedOutput(); err != nil {
		t.Fatalf("build console probe: %v: %s", err, output)
	}
	config := filepath.Join(dir, "node.json")
	for _, fail := range []bool{false, true} {
		t.Setenv("CONTROL_TEST_BOOTSTRAP_FAILURE", map[bool]string{false: "0", true: "1"}[fail])
		handled, err := runPlatformService(ctx, []string{"__user-launch", config, binary})
		if !handled || (err != nil) != fail {
			t.Fatalf("bootstrap did not wait for its supervisor: handled=%v, error=%v", handled, err)
		}
		if fail {
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
				t.Fatal("bootstrap lost the supervisor's failure", err)
			}
		}
	}
	log, err := os.ReadFile(filepath.Join(dir, "node.log"))
	if err != nil || strings.Count(string(log), "no-console child: "+config) != 2 {
		t.Fatalf("bootstrap did not preserve log output and quoted paths: %s, %v", log, err)
	}
}

func awaitServiceState(t *testing.T, ctx context.Context, statuses <-chan svc.Status, want svc.State) {
	t.Helper()
	for {
		select {
		case status := <-statuses:
			if status.State == want {
				return
			}
		case <-ctx.Done():
			t.Fatalf("service did not reach state %d", want)
		}
	}
}

func commandName(cmd svc.Cmd) string {
	if cmd == svc.Stop {
		return "stop"
	}
	return "shutdown"
}
