package installation

import (
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/koltyakov/control/internal/store"
)

func TestUserPowerShellOutput(t *testing.T) {
	for _, tc := range []struct {
		name, script, want, failure string
		exitCode                    int
	}{
		{"empty", "", "", "", 0},
		{"text output", "Write-Output 'Running'", "Running", "", 0},
		{"progress", "Write-Progress -Activity 'Loading' -Status 'Working'; Write-Output 'Ready'", "Ready", "", 0},
		{"throw", "throw 'Startup failed'", "", "Startup failed", 1},
		{"cmdlet error", "Write-Error 'Startup failed'; Write-Output 'unexpected success'", "", "Startup failed", 1},
		{"output before failure", "Write-Output 'Before'; throw 'Startup failed'", "", "Startup failed", 1},
		{"explicit exit", "[Console]::Error.WriteLine('Startup failed'); exit 7", "", "Startup failed", 7},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			output, err := userPowerShellOutput(ctx, tc.script)
			if tc.failure == "" {
				if err != nil || output != tc.want {
					t.Fatalf("expected %q, got %q, %v", tc.want, output, err)
				}
				return
			}
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) || exitErr.ExitCode() != tc.exitCode {
				t.Fatalf("expected exit code %d, got %v", tc.exitCode, err)
			}
			if output != "" || !strings.Contains(err.Error(), tc.failure) || strings.Contains(err.Error(), "CLIXML") || strings.Contains(err.Error(), "unexpected success") {
				t.Fatalf("expected readable failure %q, got %q, %v", tc.failure, output, err)
			}
		})
	}
}

func TestUserTaskLookup(t *testing.T) {
	config := filepath.Join(t.TempDir(), "node.json")
	for _, tc := range []struct {
		name, lookup, want, failure string
	}{
		{"empty scheduler", "", "missing", ""},
		{"unrelated task", "[PSCustomObject]@{ TaskPath = '\\'; TaskName = 'Other' }", "missing", ""},
		{"other folder", "[PSCustomObject]@{ TaskPath = '\\Other\\'; TaskName = " + powershellLiteral(userTaskName(config)) + " }", "missing", ""},
		{"owned task", "[PSCustomObject]@{ TaskPath = '\\'; TaskName = $name; Description = $description; Principal = [PSCustomObject]@{ UserId = $sid } }", "owned", ""},
		{"foreign profile", "[PSCustomObject]@{ TaskPath = '\\'; TaskName = $name; Description = 'Other'; Principal = [PSCustomObject]@{ UserId = $sid } }", "", "belongs to another profile or user"},
		{"scheduler error", "throw 'Scheduler access denied'", "", "Scheduler access denied"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			// Exercise PowerShell filtering and exit status without touching tasks.
			script := "function Get-ScheduledTask { param($ErrorAction) " + tc.lookup + " }\n" + userTaskScript(config, "if ($task) { Write-Output 'owned' } else { Write-Output 'missing' }")
			output, err := userPowerShellOutput(ctx, script)
			if tc.failure != "" {
				if err == nil || !strings.Contains(err.Error(), tc.failure) || strings.Contains(err.Error(), "CLIXML") {
					t.Fatalf("expected readable failure %q, got %q, %v", tc.failure, output, err)
				}
			} else if err != nil || output != tc.want {
				t.Fatalf("expected %q, got %q, %v", tc.want, output, err)
			}
		})
	}
}

func TestMissingUserTaskNoOp(t *testing.T) {
	config := filepath.Join(t.TempDir(), "node.json")
	for _, operation := range []string{
		"if ($task) { Write-Output $task.State }",
		"if ($task) { Unregister-ScheduledTask -InputObject $task -Confirm:$false }",
		"if ($task) { Stop-ScheduledTask -InputObject $task }",
	} {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		output, err := userPowerShellOutput(ctx, "function Get-ScheduledTask { param($ErrorAction) }\n"+userTaskScript(config, operation))
		cancel()
		if err != nil || output != "" {
			t.Fatalf("missing task must be a successful no-op: %q, %v", output, err)
		}
	}
}

func TestMissingUserTaskInScheduler(t *testing.T) {
	// Reading the real scheduler needs no elevation and must succeed when this
	// temporary profile has never registered a login task. Do not mutate tasks.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	config := filepath.Join(t.TempDir(), "node.json")
	output, err := userPowerShellOutput(ctx, userTaskScript(config, "if ($task) { throw 'unexpected login task' }"))
	if err != nil || output != "" {
		t.Fatalf("missing login task must be a successful no-op: %q, %v", output, err)
	}
}

func TestSavedWindowsStartupMode(t *testing.T) {
	config := filepath.Join(t.TempDir(), "node.json")
	if got, err := resolveServiceMode(config, ""); err != nil || got != "auto" {
		t.Fatalf("default mode: %q, %v", got, err)
	}
	if err := store.Write(config+".startup.json", startupProfile{Mode: "user"}); err != nil {
		t.Fatal(err)
	}
	if got, err := resolveServiceMode(config, ""); err != nil || got != "user" {
		t.Fatalf("saved mode: %q, %v", got, err)
	}
	if got, err := resolveServiceMode(config, "auto"); err != nil || got != "auto" {
		t.Fatalf("explicit mode: %q, %v", got, err)
	}
	if err := store.Write(config+".startup.json", startupProfile{Mode: "invalid"}); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveServiceMode(config, ""); err == nil {
		t.Fatal("accepted invalid saved mode")
	}
}
