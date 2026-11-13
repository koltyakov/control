//go:build windows

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/koltyakov/control/internal/client"
	"github.com/koltyakov/control/internal/gateway"
	"github.com/koltyakov/control/internal/identity"
	"github.com/koltyakov/control/internal/installation"
	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/store"
	"github.com/koltyakov/control/internal/update"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc/mgr"
)

// A prebuilt native CLI lets a disposable Windows Sandbox run the smoke suite
// without installing Go or enabling network access. Normal developer runs build.
func windowsSmokeBinary(t *testing.T, ctx context.Context, destination string) {
	t.Helper()
	if source := os.Getenv("CONTROL_TEST_WINDOWS_BINARY"); source != "" {
		binary, err := update.InspectRuntime(ctx, source)
		if err != nil {
			t.Fatal(err)
		}
		copyWindowsSmokeBinary(t, binary, destination)
		return
	}
	command := exec.CommandContext(ctx, "go", "build", "-o", destination, ".")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("build: %v: %s", err, output)
	}
}

func copyWindowsSmokeBinary(t *testing.T, binary update.RuntimeBinary, destination string) {
	t.Helper()
	source, err := os.Open(binary.Path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = source.Close() }()
	if err = update.SaveBinary(source, destination, binary.Asset); err != nil {
		t.Fatal(err)
	}
}

type windowsSmokeProcess struct {
	PID  uint32 `json:"ProcessId"`
	Path string `json:"ExecutablePath"`
}

func windowsSmokeProcesses(t *testing.T, ctx context.Context, directory string) []windowsSmokeProcess {
	t.Helper()
	prefix := strings.ReplaceAll(directory+string(filepath.Separator), "'", "''")
	script := "$ErrorActionPreference='Stop'; $matches = @(Get-CimInstance Win32_Process | Where-Object { $_.ExecutablePath -and $_.ExecutablePath.StartsWith('" + prefix + "', [StringComparison]::OrdinalIgnoreCase) } | Select-Object ProcessId,ExecutablePath); ConvertTo-Json -InputObject $matches -Compress"
	command := exec.CommandContext(ctx, "powershell.exe", "-NoProfile", "-NonInteractive", "-Command", script)
	if output, err := command.Output(); err != nil {
		t.Fatalf("inspect fixture processes: %v", err)
	} else {
		var processes []windowsSmokeProcess
		if err = json.Unmarshal(output, &processes); err != nil {
			t.Fatalf("fixture process response: %s: %v", output, err)
		}
		return processes
	}
	return nil
}

func windowsSmokeOneProcess(t *testing.T, ctx context.Context, directory, runtimePath string) uint32 {
	t.Helper()
	processes := windowsSmokeProcesses(t, ctx, directory)
	if len(processes) != 1 || !strings.EqualFold(processes[0].Path, runtimePath) {
		t.Fatalf("expected one stable runtime and no supervisor/updater: %+v", processes)
	}
	return processes[0].PID
}

func startLegacyWindowsSmoke(t *testing.T, ctx context.Context, binary, config, mode string) {
	t.Helper()
	arguments := []string{"--config", config, "service", "start", "--mode", mode}
	if mode != "user" {
		command := exec.CommandContext(ctx, binary, arguments...)
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("start legacy SCM fixture: %v: %s", err, output)
		}
		return
	}
	// Install the historical User task as the limited installing user, not the
	// elevated test harness. The short-lived setup task has no trigger/password.
	name := "ControlSmokeInstall-" + identity.NewID()
	literal := func(value string) string { return "'" + strings.ReplaceAll(value, "'", "''") + "'" }
	script := fmt.Sprintf(`$ErrorActionPreference='Stop'
$name=%s
$sid=[Security.Principal.WindowsIdentity]::GetCurrent().User.Value
$action=New-ScheduledTaskAction -Execute %s -Argument %s
$principal=New-ScheduledTaskPrincipal -UserId $sid -LogonType Interactive -RunLevel Limited
$settings=New-ScheduledTaskSettingsSet -ExecutionTimeLimit (New-TimeSpan -Minutes 2)
Register-ScheduledTask -TaskName $name -TaskPath '\' -Action $action -Principal $principal -Settings $settings | Out-Null
try {
  Start-ScheduledTask -TaskName $name -TaskPath '\'
  $deadline=[DateTime]::UtcNow.AddSeconds(60)
  do {
    Start-Sleep -Milliseconds 200
    $task=Get-ScheduledTask -TaskName $name -TaskPath '\'
    $info=Get-ScheduledTaskInfo -TaskName $name -TaskPath '\'
    if ([DateTime]::UtcNow -gt $deadline) { throw 'Legacy installer did not finish' }
  } while ($info.LastRunTime.Year -lt 2000 -or $task.State -in @('Running','Queued'))
  if ($info.LastTaskResult -ne 0) { throw ('Legacy installer failed: '+$info.LastTaskResult) }
} finally { Unregister-ScheduledTask -TaskName $name -TaskPath '\' -Confirm:$false }
`, literal(name), literal(binary), literal(windows.ComposeCommandLine(arguments)))
	command := exec.CommandContext(ctx, "powershell.exe", "-NoProfile", "-NonInteractive", "-Command", script)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("install legacy limited User fixture: %v: %s", err, output)
	}
}

// This opt-in test changes real SCM and Task Scheduler registrations only in
// fresh temporary profiles. Run on a disposable Windows machine or Sandbox.
func TestWindowsManagedStartupUpdateSmoke(t *testing.T) {
	if os.Getenv("CONTROL_TEST_WINDOWS_UPDATE_SMOKE") != "1" {
		t.Skip("set CONTROL_TEST_WINDOWS_UPDATE_SMOKE=1 on disposable Windows")
	}
	updatedPath := os.Getenv("CONTROL_TEST_WINDOWS_UPDATED_BINARY")
	if updatedPath == "" {
		t.Fatal("CONTROL_TEST_WINDOWS_UPDATED_BINARY must name a distinct native build")
	}
	type scenario struct {
		Name, Mode, Legacy string
		System             bool
	}
	scenarios := []scenario{{Name: "auto", Mode: "auto"}, {Name: "user", Mode: "user"}}
	if legacy := os.Getenv("CONTROL_TEST_WINDOWS_LEGACY_BINARY"); legacy != "" {
		scenarios = append(scenarios, scenario{Name: "legacy-user", Mode: "user", Legacy: legacy}, scenario{Name: "legacy-system", Mode: "auto", Legacy: legacy, System: true}, scenario{Name: "legacy-restricted-service", Mode: "auto", Legacy: legacy})
	}
	for _, scenario := range scenarios {
		mode := scenario.Mode
		t.Run(scenario.Name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
			defer cancel()
			dir := filepath.Join(t.TempDir(), "managed update with spaces")
			if err := os.MkdirAll(dir, 0700); err != nil {
				t.Fatal(err)
			}
			binary := filepath.Join(dir, "installed.exe")
			if scenario.Legacy == "" {
				windowsSmokeBinary(t, ctx, binary)
			} else {
				legacy, err := update.InspectRuntime(ctx, scenario.Legacy)
				if err != nil {
					t.Fatal(err)
				}
				copyWindowsSmokeBinary(t, legacy, binary)
			}
			beforeBinary, err := update.InspectRuntime(ctx, binary)
			if err != nil {
				t.Fatal(err)
			}
			next, err := update.InspectRuntime(ctx, updatedPath)
			if err != nil {
				t.Fatal(err)
			}
			if beforeBinary.Version == next.Version {
				t.Fatal("smoke builds must have distinct version labels")
			}
			nextSoftware, err := update.ValidatedExecutable(ctx, next.Path, next.Version, next.Asset)
			if err != nil {
				t.Fatal(err)
			}
			const token = "windows-native-smoke-node-credential"
			const operator = "windows-native-smoke-operator-credential-0001"
			// Advertise a bridge gateway already at the target version, keeping
			// this fixture focused on real node/SCM/login-task replacement.
			g, err := gateway.New(t.TempDir(), token, gateway.Options{Software: nextSoftware, SuperuserKey: operator, Apply: func(string) error { return errors.New("unexpected fixture gateway restart") }})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = g.Close() }()
			server := httptest.NewServer(g.Handler())
			defer server.Close()
			g.StartUpdates(ctx)
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			address := listener.Addr().String()
			_ = listener.Close()
			config := filepath.Join(dir, "node.json")
			if err = createNodeConfig(config, "native-update-"+mode, server.URL, token, address, false); err != nil {
				t.Fatal(err)
			}
			defer func() {
				cleanup, done := context.WithTimeout(context.Background(), 30*time.Second)
				defer done()
				if err := installation.Service(cleanup, "uninstall", binary, config, mode); err != nil {
					t.Errorf("remove smoke startup: %v", err)
				}
				if remaining := windowsSmokeProcesses(t, cleanup, dir); len(remaining) != 0 {
					t.Errorf("fixture processes remain after uninstall: %+v", remaining)
				}
				if t.Failed() {
					if data, err := os.ReadFile(filepath.Join(dir, "node.log")); err == nil {
						t.Logf("node log:\n%s", data)
					}
				}
			}()
			if scenario.Legacy != "" {
				startLegacyWindowsSmoke(t, ctx, binary, config, mode)
				if scenario.System {
					if err = installation.Service(ctx, "stop", binary, config, mode); err != nil {
						t.Fatal(err)
					}
					manager, err := mgr.Connect()
					if err != nil {
						t.Fatal(err)
					}
					service, err := manager.OpenService(installation.WindowsServiceName(config))
					if err != nil {
						_ = manager.Disconnect()
						t.Fatal(err)
					}
					settings, err := service.Config()
					if err == nil {
						settings.ServiceStartName = "LocalSystem"
						err = service.UpdateConfig(settings)
					}
					_ = service.Close()
					_ = manager.Disconnect()
					if err != nil {
						t.Fatal(err)
					}
					startLegacyWindowsSmoke(t, ctx, binary, config, mode)
				}
			} else if err = installation.Service(ctx, "start", binary, config, mode); err != nil {
				t.Fatal(err)
			}
			c := client.Client{URL: "http://" + address, Token: token}
			defer func() { _ = c.Close() }()
			var before struct{ ID string }
			if err = c.Call(ctx, "", "node.describe", map[string]any{}, &before); err != nil {
				t.Fatal(err)
			}
			runtimePath := installation.WindowsRuntimePath(filepath.Join(dir, "state"))
			var beforePID uint32
			if scenario.Legacy == "" {
				beforePID = windowsSmokeOneProcess(t, ctx, dir, runtimePath)
			} else {
				processes := windowsSmokeProcesses(t, ctx, dir)
				expected := 2
				if mode == "user" {
					expected = 3
				}
				if len(processes) != expected {
					t.Fatalf("legacy baseline process count: %+v", processes)
				}
				for _, process := range processes {
					if strings.EqualFold(process.Path, runtimePath) {
						beforePID = process.PID
					}
				}
				if beforePID == 0 {
					t.Fatal("legacy child not found")
				}
				t.Logf("legacy baseline: %d Control processes", len(processes))
			}
			var execution struct{ Stdout string }
			probe := `$identity=[Security.Principal.WindowsIdentity]::GetCurrent(); $principal=New-Object Security.Principal.WindowsPrincipal($identity); [pscustomobject]@{SID=$identity.User.Value; Elevated=$principal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator); Session=[Diagnostics.Process]::GetCurrentProcess().SessionId} | ConvertTo-Json -Compress`
			if err = c.Call(ctx, "", "exec.run", map[string]any{"command": "powershell.exe", "args": []string{"-NoProfile", "-NonInteractive", "-Command", probe}}, &execution); err != nil {
				t.Fatal(err)
			}
			var identity struct {
				SID      string
				Elevated bool
				Session  uint32
			}
			if err = json.Unmarshal([]byte(execution.Stdout), &identity); err != nil {
				t.Fatal(err)
			}
			if identity.Elevated != scenario.System {
				t.Fatal("fixture node unexpectedly runs elevated")
			}
			if mode == "auto" {
				expectedSID := "S-1-5-19"
				if scenario.System {
					expectedSID = "S-1-5-18"
				}
				if identity.SID != expectedSID || identity.Session != 0 {
					t.Fatalf("SCM did not use LocalService in session zero: %+v", identity)
				}
			} else {
				user, err := windows.GetCurrentProcessToken().GetTokenUser()
				if err != nil || identity.SID != user.User.Sid.String() || identity.Session == 0 {
					t.Fatalf("login task did not use the limited interactive user: %+v, %v", identity, err)
				}
			}
			var accepted model.Task
			if err = c.Call(ctx, "", "tasks.start", model.TaskSpec{ID: "retained-smoke-task", Capability: "exec.run", Args: model.JSON(map[string]any{"command": "cmd.exe", "args": []string{"/c", "echo", "native-smoke"}})}, &accepted); err != nil {
				t.Fatal(err)
			}
			finished, err := c.Wait(ctx, "", accepted.ID)
			if err != nil || finished.State != "succeeded" {
				t.Fatalf("pre-update task: %+v, %v", finished, err)
			}
			bundle := filepath.Join(dir, "bundle")
			copyWindowsSmokeBinary(t, next, filepath.Join(bundle, next.Asset.File))
			manifest := update.Manifest{Version: next.Version, CreatedAt: time.Now().UTC(), Assets: []update.Asset{next.Asset}}
			if err = store.Write(filepath.Join(bundle, "control-manifest.json"), manifest); err != nil {
				t.Fatal(err)
			}
			admin := client.Admin{URL: server.URL, Key: operator}
			deployment, err := admin.Push(ctx, bundle)
			if err != nil {
				t.Fatal(err)
			}
			var status struct {
				Deployment   *update.Deployment
				Nodes        map[string]update.Status
				NodeProgress map[string]string
			}
			refreshed := false
			if scenario.Legacy != "" && mode == "auto" && !scenario.System {
				waitWindowsNode(t, ctx, func() bool {
					if admin.JSON(ctx, "GET", "/v1/admin/updates", nil, &status) != nil {
						return false
					}
					state := status.Nodes[before.ID]
					return state.Software.Version == next.Version && state.Software.Runtime == "supervised" && status.NodeProgress[before.ID] == "startup-refresh-required"
				})
				if status.Deployment == nil || status.Deployment.Phase == "complete" {
					t.Fatal("restricted legacy service was falsely acknowledged as complete")
				}
				t.Logf("restricted legacy service correctly blocked without retrying the same executable: %s", status.NodeProgress[before.ID])
				if err = installation.Service(ctx, "stop", binary, config, mode); err != nil {
					t.Fatal(err)
				}
				copyWindowsSmokeBinary(t, next, binary)
				if err = installation.Service(ctx, "start", binary, config, mode); err != nil {
					t.Fatal(err)
				}
				refreshed = true
			}
			waitWindowsNode(t, ctx, func() bool {
				if admin.JSON(ctx, "GET", "/v1/admin/updates", nil, &status) != nil || status.Deployment == nil {
					return false
				}
				nodeStatus := status.Nodes[before.ID]
				if nodeStatus.Error != "" && (!refreshed || nodeStatus.Software.Runtime != "supervised") {
					t.Fatalf("managed update failed: %+v", nodeStatus)
				}
				return status.Deployment.ID == deployment.ID && status.Deployment.Phase == "complete" && nodeStatus.Software.Version == next.Version && nodeStatus.Software.SHA256 == next.Asset.SHA256 && nodeStatus.Software.Runtime == "single" && !nodeStatus.Paused
			})
			waitWindowsNode(t, ctx, func() bool {
				_, markerErr := os.Stat(filepath.Join(dir, "state", "runtime-starting.json"))
				acks, err := filepath.Glob(filepath.Join(dir, "state", "runtime-ready-*.json"))
				return os.IsNotExist(markerErr) && err == nil && len(acks) == 0
			})
			afterPID := windowsSmokeOneProcess(t, ctx, dir, runtimePath)
			if afterPID == beforePID {
				t.Fatal("update did not replace the process")
			}
			var after struct{ ID string }
			if err = c.Call(ctx, "", "node.describe", map[string]any{}, &after); err != nil || after.ID != before.ID {
				t.Fatalf("identity changed after update: %+v, %v", after, err)
			}
			if err = c.Call(ctx, "", "tasks.get", map[string]any{"id": accepted.ID}, &finished); err != nil || finished.State != "succeeded" {
				t.Fatalf("task record lost after update: %+v, %v", finished, err)
			}
			if err = installation.Service(ctx, "stop", binary, config, mode); err != nil {
				t.Fatal(err)
			}
			if remaining := windowsSmokeProcesses(t, ctx, dir); len(remaining) != 0 {
				t.Fatalf("explicit stop left processes: %+v", remaining)
			}
			if err = installation.Service(ctx, "start", binary, config, mode); err != nil {
				t.Fatal(err)
			}
			var snapshot model.NodeActivitySnapshot
			if err = c.Call(ctx, "", "activities.list", map[string]any{}, &snapshot); err != nil || snapshot.Software.Version != next.Version || snapshot.Software.SHA256 != next.Asset.SHA256 {
				t.Fatalf("restart reverted the selected software: %+v, %v", snapshot.Software, err)
			}
			restartedPID := windowsSmokeOneProcess(t, ctx, dir, runtimePath)
			if restartedPID == afterPID {
				t.Fatal("explicit restart retained the old process")
			}
			t.Logf("mode=%s identity=%s session=%d elevated=%v version=%s->%s pids=%d->%d->%d selectedSHA256=%s; identity/task retained; no supervisor/updater after readiness", mode, identity.SID, identity.Session, identity.Elevated, beforeBinary.Version, next.Version, beforePID, afterPID, restartedPID, next.Asset.SHA256)
		})
	}
}
