package processutil

import (
	"context"
	"os"
	"os/exec"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

func TestBackgroundChildHasNoConsole(t *testing.T) {
	if os.Getenv("CONTROL_TEST_NO_CONSOLE") == "1" {
		window, _, _ := windows.NewLazySystemDLL("kernel32.dll").NewProc("GetConsoleWindow").Call()
		if window != 0 {
			os.Exit(2)
		}
		os.Exit(0)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(ctx, exe, "-test.run=^TestBackgroundChildHasNoConsole$")
	cmd.Env = append(os.Environ(), "CONTROL_TEST_NO_CONSOLE=1")
	HideWindow(cmd)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("background child acquired a console: %v: %s", err, output)
	}
}
