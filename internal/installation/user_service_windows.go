package installation

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/koltyakov/control/internal/node"
	"github.com/koltyakov/control/internal/processutil"
	"github.com/koltyakov/control/internal/store"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

type startupProfile struct {
	Mode string `json:"mode"`
}

func resolveServiceMode(config, mode string) (string, error) {
	if mode == "system" {
		return "auto", nil
	}
	if mode != "" {
		return mode, nil
	}
	var profile startupProfile
	if err := store.Read(config+".startup.json", &profile); err != nil {
		if os.IsNotExist(err) {
			return "auto", nil
		}
		return "", err
	}
	if profile.Mode != "auto" && profile.Mode != "user" {
		return "", errors.New("invalid saved Windows startup mode")
	}
	return profile.Mode, nil
}

func userPowerShell(ctx context.Context, script string) error {
	_, err := userPowerShellOutput(ctx, script)
	return err
}

func userPowerShellOutput(ctx context.Context, script string) (string, error) {
	// Windows PowerShell can serialize encoded-command errors as CLIXML even
	// with text output selected. Catch failures and bypass its error stream.
	// Suppress module-loading progress so it cannot obscure task state or errors.
	script = "$ErrorActionPreference = 'Stop'\n$ProgressPreference = 'SilentlyContinue'\ntry {\n" + script + "\n} catch {\n[Console]::Error.WriteLine($_.ToString())\nexit 1\n}"
	cmd := exec.CommandContext(ctx, "powershell.exe", "-NoProfile", "-NonInteractive", "-OutputFormat", "Text", "-EncodedCommand", encodedPowerShell(script))
	processutil.HideWindow(cmd)
	cmd.Env = cleanEnvironment()
	output, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("Windows login startup: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return strings.TrimSpace(string(output)), nil
}

func removeUserTask(ctx context.Context, config string) error {
	return userPowerShell(ctx, userTaskScript(config, "if ($task) { Unregister-ScheduledTask -InputObject $task -Confirm:$false }"))
}

func finishServiceStop(ctx context.Context, config, mode string) error {
	if mode != "user" {
		return nil
	}
	// The API has stopped the supervisor and released its runtime lock. Complete
	// the task wrapper too so a subsequent start cannot be ignored as a duplicate.
	return userPowerShell(ctx, userTaskScript(config, "if ($task) { Stop-ScheduledTask -InputObject $task }"))
}

func checkServiceProfile(config, mode string) error {
	if mode != "user" {
		return nil
	}
	s, err := userMigrationService(config)
	if s != nil {
		s.Close()
	}
	return err
}

// A disabled, stopped SCM registration can be retained for explicit rollback.
// Ordinary user starts need only read access to inspect that registration.
func userMigrationService(config string) (*mgr.Service, error) {
	handle, err := windows.OpenSCManager(nil, nil, windows.SC_MANAGER_CONNECT)
	if err != nil {
		return nil, err
	}
	m := &mgr.Mgr{Handle: handle}
	defer m.Disconnect()
	name, err := windows.UTF16PtrFromString(WindowsServiceName(config))
	if err != nil {
		return nil, err
	}
	serviceHandle, err := windows.OpenService(m.Handle, name, windows.SERVICE_QUERY_STATUS|windows.SERVICE_QUERY_CONFIG)
	if errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("inspect existing system service: %w", err)
	}
	readOnly := &mgr.Service{Name: WindowsServiceName(config), Handle: serviceHandle}
	settings, configErr := readOnly.Config()
	status, statusErr := readOnly.Query()
	readOnly.Close()
	if configErr != nil || statusErr != nil {
		return nil, errors.Join(configErr, statusErr)
	}
	if settings.StartType == mgr.StartDisabled && status.State == svc.Stopped {
		return nil, nil
	}
	if !windows.GetCurrentProcessToken().IsElevated() {
		return nil, errors.New("switching a system service to user startup requires Administrator PowerShell under the intended user account")
	}
	return m.OpenService(WindowsServiceName(config))
}

func platformUserService(ctx context.Context, operation, binary, config string, cfg node.Config) (bool, error) {
	if operation == "uninstall" {
		if cfg.Name == "" {
			return true, errors.New("restore the node configuration before uninstalling user startup so the node can stop gracefully")
		}
		if err := removeUserTask(ctx, config); err != nil {
			return true, err
		}
		if err := os.Remove(config + ".startup.json"); err != nil && !os.IsNotExist(err) {
			return true, err
		}
		return false, nil // The local API stops the node gracefully.
	}
	if operation != "start" {
		return false, nil
	}
	if err := CheckServiceMode("user"); err != nil {
		return true, err
	}
	s, err := userMigrationService(config)
	if err != nil {
		return true, err
	}
	if s != nil {
		defer s.Close()
	}
	state, err := userPowerShellOutput(ctx, userTaskScript(config, "if ($task) { Write-Output $task.State }"))
	if err != nil {
		return true, err
	}
	if err = userPowerShell(ctx, userTaskScript(config, userTaskRegistration(binary, config))); err != nil {
		return true, err
	}
	if s != nil {
		if err = stopWindowsService(ctx, s); err != nil {
			return true, err
		}
		settings, err := s.Config()
		if err != nil {
			return true, err
		}
		settings.StartType = mgr.StartDisabled
		if err = s.UpdateConfig(settings); err != nil {
			return true, err
		}
	} else {
		// A currently running login task should not be interrupted on repeated
		// starts. Otherwise stop any detached supervisor before changing account.
		if state != "Running" {
			if err = stopLegacyNode(ctx, cfg); err != nil {
				return true, err
			}
		}
	}
	if err = uninstallService(ctx, config); err != nil {
		return true, err
	}
	if err = store.Write(config+".startup.json", startupProfile{Mode: "user"}); err != nil {
		return true, err
	}
	return true, userPowerShell(ctx, userTaskScript(config, "Start-ScheduledTask -TaskName $name -TaskPath '\\'"))
}
