package installation

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gofrs/flock"
	"github.com/koltyakov/control/internal/node"
	"github.com/koltyakov/control/internal/store"
	"github.com/koltyakov/control/internal/update"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

func prepareDirectWindowsRuntime(ctx context.Context, binary string, cfg node.Config, gui bool) (string, error) {
	if err := os.MkdirAll(cfg.DataDir, 0700); err != nil {
		return "", err
	}
	lock := flock.New(filepath.Join(cfg.DataDir, "runtime.lock"))
	defer func() { _ = lock.Close() }()
	locked, err := lock.TryLock()
	if err != nil {
		return "", err
	}
	var current update.RuntimeBinary
	err = store.Read(filepath.Join(cfg.DataDir, "runtime.json"), &current)
	if errors.Is(err, os.ErrNotExist) && locked {
		current, err = update.InspectRuntime(ctx, binary)
	}
	if err == nil && current.Version == "" && locked {
		current, err = update.InspectRuntime(ctx, current.Path)
	}
	if err != nil {
		return "", errors.New("stop the existing node before refreshing single-process startup: " + err.Error())
	}
	if err = update.ValidateExecutable(ctx, current.Path, current.Version, current.Asset); err != nil {
		return "", err
	}
	if !locked {
		_, asset, err := update.WindowsRuntimeImage(current, gui)
		if err != nil {
			return "", err
		}
		path := WindowsRuntimePath(cfg.DataDir)
		if err = update.Verify(path, asset); err != nil {
			return "", errors.New("stop the existing node before replacing its runtime executable")
		}
		return path, nil
	}
	// Never bind a new managed selection to the installed CLI, which a local
	// control update can replace independently of the service's selected build.
	staged := filepath.Join(cfg.DataDir, "updates", current.Asset.SHA256, current.Asset.File)
	if !strings.EqualFold(filepath.Clean(staged), filepath.Clean(current.Path)) {
		source, err := os.Open(current.Path)
		if err != nil {
			return "", err
		}
		err = update.SaveBinary(source, staged, current.Asset)
		_ = source.Close()
		if err != nil {
			return "", err
		}
		current.Path = staged
	}
	if err = store.Write(filepath.Join(cfg.DataDir, "runtime.json"), current); err != nil {
		return "", err
	}
	return update.PrepareWindowsRuntime(cfg.DataDir, current, gui)
}

// OpenUpdateService uses only this service SID's restart/query rights. It also
// checks the complete command before allowing an updater to restart anything.
func OpenUpdateService(config, runtime string) (*mgr.Service, error) {
	handle, err := windows.OpenSCManager(nil, nil, windows.SC_MANAGER_CONNECT)
	if err != nil {
		return nil, err
	}
	defer func() { _ = windows.CloseServiceHandle(handle) }()
	name, err := windows.UTF16PtrFromString(WindowsServiceName(config))
	if err != nil {
		return nil, err
	}
	h, err := windows.OpenService(handle, name, windows.SERVICE_START|windows.SERVICE_QUERY_STATUS|windows.SERVICE_QUERY_CONFIG)
	if err != nil {
		return nil, err
	}
	s := &mgr.Service{Name: WindowsServiceName(config), Handle: h}
	settings, err := s.Config()
	if err == nil {
		args, parseErr := windows.DecomposeCommandLine(settings.BinaryPathName)
		if parseErr != nil || len(args) != 3 || !strings.EqualFold(filepath.Clean(args[0]), filepath.Clean(runtime)) || args[1] != "__service" || !strings.EqualFold(filepath.Clean(args[2]), filepath.Clean(config)) {
			err = errors.New("refresh this profile with a current CLI service stop/start before updating single-process SCM startup")
		}
	}
	if err != nil {
		_ = s.Close()
		return nil, err
	}
	return s, nil
}

// RestartUpdateService starts exactly once after SCM acknowledges a clean stop.
func RestartUpdateService(ctx context.Context, s *mgr.Service) error {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		status, err := s.Query()
		if err != nil {
			return err
		}
		if status.State == svc.Stopped {
			return s.Start()
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// CheckUpdateUserTask verifies ownership, arguments and the stable runtime path.
func CheckUpdateUserTask(ctx context.Context, config, runtime string) error {
	return userPowerShell(ctx, userTaskScript(config, updateUserTaskCheck(runtime, config)))
}

func updateUserTaskCheck(runtime, config string) string {
	return "if (-not $task) { throw 'Control login task is missing' }\n" +
		"if (@($task.Actions).Count -ne 1 -or $task.Actions[0].Execute -ne " + powershellLiteral(runtime) +
		" -or $task.Actions[0].Arguments -ne " + powershellLiteral(userTaskArguments(config)) +
		") { throw 'Refresh this profile with a current CLI service stop/start before updating single-process User startup' }\n"
}

// RestartUpdateUserTask waits for the old instance to leave Task Scheduler, then
// starts once. The temporary updater must be outside the task's process job.
func RestartUpdateUserTask(ctx context.Context, config, runtime string) error {
	script := updateUserTaskCheck(runtime, config) + `
$deadline = [DateTime]::UtcNow.AddSeconds(30)
while ($task.State -eq 'Running') {
  if ([DateTime]::UtcNow -ge $deadline) { throw 'Control login task did not stop' }
  Start-Sleep -Milliseconds 100
  $task = Get-ScheduledTask -TaskPath '\' -TaskName $name -ErrorAction Stop
}
Start-ScheduledTask -TaskPath '\' -TaskName $name
`
	return userPowerShell(ctx, userTaskScript(config, script))
}
