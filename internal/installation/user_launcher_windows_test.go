package installation

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestWindowsGUIStartupHasNoConsole(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	dir := t.TempDir()
	source := filepath.Join(dir, "probe.go")
	code := `package main
import ("os"; "syscall")
func main() {
 window, _, _ := syscall.NewLazyDLL("kernel32.dll").NewProc("GetConsoleWindow").Call()
 if window != 0 { os.Exit(99) }
 os.Exit(17)
}`
	if err := os.WriteFile(source, []byte(code), 0600); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(dir, "console.exe")
	if output, err := exec.CommandContext(ctx, "go", "build", "-o", binary, source).CombinedOutput(); err != nil {
		t.Fatalf("build console probe: %v: %s", err, output)
	}
	launcher, err := prepareUserLauncher(binary, dir)
	if err != nil {
		t.Fatal(err)
	}
	// Do not set HideWindow or CREATE_NO_WINDOW here. The scheduled task does
	// not control creation flags; the PE GUI subsystem must prevent the console.
	err = exec.CommandContext(ctx, launcher).Run()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 17 {
		t.Fatalf("GUI startup acquired a console or lost its exit status: %v", err)
	}
}
