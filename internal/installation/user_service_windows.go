package installation

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"unsafe"

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

func checkUserStartup(context.Context) error { return CheckServiceMode("user") }

// UserStartupWarning detects over-the-shoulder elevation. User startup installs
// for the account running this process, which may not be the logged-on user.
func UserStartupWarning() string {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return ""
	}
	account, domain, _, err := user.User.Sid.LookupAccount("")
	if err != nil {
		return ""
	}
	sessionAccount, sessionDomain := sessionUser()
	if sessionAccount == "" || (strings.EqualFold(account, sessionAccount) && strings.EqualFold(domain, sessionDomain)) {
		return ""
	}
	return fmt.Sprintf("this installation runs as %s\\%s, but %s\\%s is logged on here. The User context and its files will belong to %s; run the command in that user's own elevated session to install it for them.", domain, account, sessionDomain, sessionAccount, account)
}

// sessionUser returns this session's logged-on user, or empty names for a
// session without one, such as a service or remote command session.
func sessionUser() (account, domain string) {
	var session uint32
	if windows.ProcessIdToSessionId(windows.GetCurrentProcessId(), &session) != nil {
		return "", ""
	}
	wts := windows.NewLazySystemDLL("wtsapi32.dll").NewProc("WTSQuerySessionInformationW")
	if wts.Find() != nil {
		return "", ""
	}
	query := func(class uintptr) string {
		var text *uint16
		var size uint32
		if result, _, _ := wts.Call(0, uintptr(session), class, uintptr(unsafe.Pointer(&text)), uintptr(unsafe.Pointer(&size))); result == 0 || text == nil {
			return ""
		}
		defer windows.WTSFreeMemory(uintptr(unsafe.Pointer(text)))
		return windows.UTF16PtrToString(text)
	}
	const wtsUserName, wtsDomainName = 5, 7
	return query(wtsUserName), query(wtsDomainName)
}

func userPowerShell(ctx context.Context, script string) error {
	_, err := userPowerShellOutput(ctx, script)
	return err
}

func userPowerShellOutput(ctx context.Context, script string) (string, error) {
	// Windows PowerShell can serialize encoded-command errors as CLIXML even
	// with text output selected. Catch failures and bypass its error stream.
	// Suppress module-loading progress so it cannot obscure task state or errors.
	script = "$ErrorActionPreference = 'Stop'\n$ProgressPreference = 'SilentlyContinue'\ntry {\n" + script + "\n} catch {\n$command = $_.InvocationInfo.MyCommand.Name\nif ($command) { [Console]::Error.WriteLine($command + ': ' + $_.ToString()) } else { [Console]::Error.WriteLine($_.ToString()) }\nexit 1\n}"
	system, err := windows.GetSystemDirectory()
	if err != nil {
		return "", fmt.Errorf("locate Windows PowerShell: %w", err)
	}
	home := filepath.Join(system, "WindowsPowerShell", "v1.0")
	cmd := exec.CommandContext(ctx, filepath.Join(home, "powershell.exe"), "-NoProfile", "-NonInteractive", "-OutputFormat", "Text", "-EncodedCommand", encodedPowerShell(script))
	processutil.HideWindow(cmd)
	cmd.Env = powerShellEnvironment(cleanEnvironment(), filepath.Join(home, "Modules"))
	output, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("Windows login startup: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return strings.TrimSpace(string(output)), nil
}

func removeUserTask(ctx context.Context, config string) error {
	if err := userPowerShell(ctx, userTaskScript(config, "if ($task) { Unregister-ScheduledTask -InputObject $task -Confirm:$false }")); err != nil {
		return fmt.Errorf("remove User login task: %w", err)
	}
	return nil
}

func finishServiceStop(ctx context.Context, config, mode string) error {
	if mode != "user" {
		return nil
	}
	// The API has stopped the supervisor and released its runtime lock. Complete
	// the task wrapper too so a subsequent start cannot be ignored as a duplicate.
	if err := userPowerShell(ctx, userTaskScript(config, "if ($task) { Stop-ScheduledTask -InputObject $task }")); err != nil {
		return fmt.Errorf("stop User login task: %w", err)
	}
	return nil
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
		return true, fmt.Errorf("read User login task state: %w", err)
	}
	launcher, err := prepareUserLauncher(binary, cfg.DataDir)
	if err != nil {
		return true, err
	}
	if err = userPowerShell(ctx, userTaskScript(config, userTaskRegistration(launcher, binary, config))); err != nil {
		return true, fmt.Errorf("register User login task: %w", err)
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
	if err = userPowerShell(ctx, userTaskScript(config, "Start-ScheduledTask -TaskName $name -TaskPath '\\'")); err != nil {
		return true, fmt.Errorf("start User login task: %w", err)
	}
	return true, nil
}
