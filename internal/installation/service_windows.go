package installation

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/koltyakov/control/internal/node"
	"github.com/koltyakov/control/internal/store"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

func detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW | windows.CREATE_NEW_PROCESS_GROUP, HideWindow: true}
}
func finishUninstall(ctx context.Context) error { return nil }
func addUserPath(dir string) error {
	key, _, err := registry.CreateKey(registry.CURRENT_USER, `Environment`, registry.QUERY_VALUE|registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer key.Close()
	value, kind, err := key.GetStringValue("Path")
	if err != nil && err != registry.ErrNotExist {
		return err
	}
	for _, part := range strings.Split(value, ";") {
		if strings.EqualFold(part, dir) {
			return nil
		}
	}
	if value != "" {
		value += ";"
	}
	value += dir
	if kind == registry.EXPAND_SZ {
		return key.SetExpandStringValue("Path", value)
	}
	return key.SetStringValue("Path", value)
}
func installService(ctx context.Context, binary, config string) error {
	return errors.New("Windows startup must use the Service Control Manager")
}
func uninstallService(ctx context.Context, config string) error {
	key, err := registry.OpenKey(registry.CURRENT_USER, `Software\Microsoft\Windows\CurrentVersion\Run`, registry.QUERY_VALUE|registry.SET_VALUE)
	if err == registry.ErrNotExist {
		return nil
	}
	if err != nil {
		return err
	}
	defer key.Close()
	value, _, err := key.GetStringValue("ControlNode")
	if err == registry.ErrNotExist {
		return nil
	}
	if err != nil {
		return err
	}
	args, err := windows.DecomposeCommandLine(value)
	if err != nil {
		return err
	}
	matched := false
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "--config" && strings.EqualFold(filepath.Clean(args[i+1]), filepath.Clean(config)) {
			matched = true
		}
	}
	if !matched {
		return nil
	}
	err = key.DeleteValue("ControlNode")
	if err == registry.ErrNotExist {
		return nil
	}
	return err
}

// WindowsServiceName binds each SCM registration to an absolute node profile.
func WindowsServiceName(config string) string {
	return "ControlNode-" + profileName(config)
}

// CheckServiceMode checks elevation before setup issues credentials or enrollment
// consumes an invitation. Process-only development installations need no elevation.
func CheckServiceMode(mode string) error {
	if err := ValidateServiceMode(mode); err != nil {
		return err
	}
	if mode == "user" {
		user, err := windows.GetCurrentProcessToken().GetTokenUser()
		if err != nil {
			return err
		}
		sid := user.User.Sid.String()
		if sid == "S-1-5-18" || sid == "S-1-5-19" || sid == "S-1-5-20" {
			return errors.New("user startup must be installed under the intended interactive user account, not a system service account")
		}
	}
	if mode != "process" && mode != "user" && !windows.GetCurrentProcessToken().IsElevated() {
		return errors.New("Windows service installation requires Administrator PowerShell; rerun the command there")
	}
	return nil
}

func platformService(ctx context.Context, operation, binary, config, mode string, cfg node.Config) (bool, error) {
	if operation == "status" {
		return false, nil
	}
	if operation != "start" && operation != "stop" && operation != "uninstall" {
		return false, nil
	}
	if mode == "process" {
		return false, nil
	}
	if mode == "user" {
		return platformUserService(ctx, operation, binary, config, cfg)
	}
	if err := CheckServiceMode(mode); err != nil {
		return true, err
	}
	m, err := mgr.Connect()
	if err != nil {
		return true, err
	}
	defer m.Disconnect()
	s, err := m.OpenService(WindowsServiceName(config))
	if err != nil && !errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
		return true, err
	}
	if s == nil && operation != "start" {
		return false, nil // A pre-service installation still uses the local API.
	}
	if operation == "start" {
		// Remove this user's login startup before SCM takes over the identity.
		if err = removeUserTask(ctx, config); err != nil {
			if s != nil {
				s.Close()
			}
			return true, err
		}
		if err = prepareServiceFiles(ctx, binary, config, cfg); err != nil {
			if s != nil {
				s.Close()
			}
			return true, err
		}
		if s == nil {
			s, err = m.CreateService(WindowsServiceName(config), binary, mgr.Config{
				DisplayName:      "Control node: " + cfg.Name,
				Description:      "Control peer execution node",
				StartType:        mgr.StartAutomatic,
				ServiceStartName: `NT AUTHORITY\LocalService`,
			}, "__service", config)
			if err != nil {
				return true, err
			}
		}
		defer s.Close()
		serviceConfig, err := s.Config()
		if err != nil {
			return true, err
		}
		if serviceConfig.StartType != mgr.StartAutomatic {
			serviceConfig.StartType = mgr.StartAutomatic
			if err = s.UpdateConfig(serviceConfig); err != nil {
				return true, err
			}
		}
		if err = s.SetRecoveryActions([]mgr.RecoveryAction{
			{Type: mgr.ServiceRestart, Delay: 5 * time.Second},
			{Type: mgr.ServiceRestart, Delay: 15 * time.Second},
			{Type: mgr.ServiceRestart, Delay: time.Minute},
		}, 86400); err != nil {
			return true, err
		}
		if err = s.SetRecoveryActionsOnNonCrashFailures(true); err != nil {
			return true, err
		}
		status, err := s.Query()
		if err != nil {
			return true, err
		}
		if status.State == svc.StopPending {
			if err = stopWindowsService(ctx, s); err != nil {
				return true, err
			}
			status.State = svc.Stopped
		}
		if status.State == svc.Stopped {
			// Stop an older detached supervisor before SCM takes over its state.
			if err = stopLegacyNode(ctx, cfg); err != nil {
				return true, err
			}
			if err = s.Start(); err != nil && !errors.Is(err, windows.ERROR_SERVICE_ALREADY_RUNNING) {
				return true, err
			}
		}
		if err = uninstallService(ctx, config); err != nil {
			return true, err
		}
		return true, store.Write(config+".startup.json", startupProfile{Mode: "auto"})
	}
	defer s.Close()
	if err = stopWindowsService(ctx, s); err != nil {
		return true, err
	}
	if operation == "uninstall" {
		if err = s.Delete(); err != nil {
			return true, err
		}
		return true, uninstallService(ctx, config)
	}
	return true, nil
}

func stopWindowsService(ctx context.Context, s *mgr.Service) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		status, err := s.Query()
		if err != nil {
			return err
		}
		if status.State == svc.Stopped {
			return nil
		}
		if status.State != svc.StopPending {
			if _, err = s.Control(svc.Stop); err != nil && !errors.Is(err, windows.ERROR_SERVICE_CANNOT_ACCEPT_CTRL) && !errors.Is(err, windows.ERROR_SERVICE_NOT_ACTIVE) {
				return err
			}
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("Windows service did not stop: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

func stopLegacyNode(ctx context.Context, cfg node.Config) error {
	probe, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	err := waitStopped(probe, cfg)
	cancel()
	if err == nil {
		return nil
	}
	ctx, cancel = context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, API(cfg)+"/v1/service/stop", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+cfg.Token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("stop existing node before service migration: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("stop existing node: %s", resp.Status)
	}
	return waitStopped(ctx, cfg)
}

func prepareServiceFiles(ctx context.Context, binary, config string, cfg node.Config) error {
	admin, err := filepath.Abs(filepath.Join(Home(), "admin.json"))
	if err != nil {
		return err
	}
	for _, path := range []string{cfg.DataDir, cfg.WorkDir} {
		for _, protected := range []string{config, admin} {
			rel, err := filepath.Rel(path, protected)
			if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				return errors.New("Windows service state and work directories must not contain node or administration profiles")
			}
		}
		if err := os.MkdirAll(path, 0700); err != nil {
			return err
		}
	}
	log := filepath.Join(filepath.Dir(config), "node.log")
	f, err := os.OpenFile(log, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	// Grant only the runtime files, never admin.json or the whole user profile.
	for _, path := range []string{filepath.Dir(config), binary, config} {
		if err = run(ctx, "icacls.exe", path, "/grant", "*S-1-5-19:(RX)", "/Q"); err != nil {
			return err
		}
	}
	if err = run(ctx, "icacls.exe", log, "/grant", "*S-1-5-19:(M)", "/Q"); err != nil {
		return err
	}
	for _, path := range []string{cfg.DataDir, cfg.WorkDir} {
		if err = run(ctx, "icacls.exe", path, "/grant", "*S-1-5-19:(OI)(CI)(M)", "/T", "/Q"); err != nil {
			return err
		}
	}
	return nil
}
