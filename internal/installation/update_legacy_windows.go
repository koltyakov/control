package installation

import (
	"context"
	"errors"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc/mgr"
)

// Legacy SCM migration needs existing administrative change-config authority.
// Never grant SERVICE_CHANGE_CONFIG to LocalService: it can change the account
// to LocalSystem. Older restricted services need a one-time privileged refresh.
func OpenLegacyUpdateService(config, launcher string) (*mgr.Service, error) {
	manager, err := windows.OpenSCManager(nil, nil, windows.SC_MANAGER_CONNECT)
	if err != nil {
		return nil, err
	}
	defer func() { _ = windows.CloseServiceHandle(manager) }()
	name, err := windows.UTF16PtrFromString(WindowsServiceName(config))
	if err != nil {
		return nil, err
	}
	handle, err := windows.OpenService(manager, name, windows.SERVICE_CHANGE_CONFIG|windows.SERVICE_START|windows.SERVICE_QUERY_STATUS|windows.SERVICE_QUERY_CONFIG)
	if err != nil {
		return nil, errors.New("legacy SCM service cannot change its own startup; run a current installed CLI service stop/start once from Administrator PowerShell: " + err.Error())
	}
	service := &mgr.Service{Name: WindowsServiceName(config), Handle: handle}
	if err = checkLegacyUpdateService(service, config, launcher); err != nil {
		_ = service.Close()
		return nil, err
	}
	return service, nil
}

func checkLegacyUpdateService(service *mgr.Service, config, launcher string) error {
	settings, err := service.Config()
	if err != nil {
		return err
	}
	args, err := windows.DecomposeCommandLine(settings.BinaryPathName)
	if err != nil || len(args) != 3 || !strings.EqualFold(filepath.Clean(args[0]), filepath.Clean(launcher)) || args[1] != "__service" || !strings.EqualFold(filepath.Clean(args[2]), filepath.Clean(config)) {
		return errors.New("legacy SCM registration belongs to another startup")
	}
	return nil
}

func SwitchLegacyUpdateService(service *mgr.Service, config, launcher, runtime string) error {
	if err := checkLegacyUpdateService(service, config, launcher); err != nil {
		return err
	}
	settings, err := service.Config()
	if err != nil {
		return err
	}
	settings.BinaryPathName = windows.ComposeCommandLine([]string{runtime, "__service", config})
	return service.UpdateConfig(settings)
}

func legacyUpdateUserCheck(config, launcher, directory string) string {
	arguments := `__user-launch "` + config + `" "` + launcher + `"`
	prefix := filepath.Join(directory, "startup") + string(filepath.Separator)
	return `if (-not $task) { throw 'Legacy Control login task is missing' }
if (@($task.Actions).Count -ne 1 -or $task.Principal.LogonType -ne 'Interactive' -or $task.Principal.RunLevel -ne 'Limited') { throw 'Legacy Control task has an unsupported principal or action' }
if (@($task.Triggers).Count -ne 1 -or $task.Triggers[0].CimClass.CimClassName -ne 'MSFT_TaskLogonTrigger') { throw 'Legacy Control task has unsupported triggers' }
if ($task.Actions[0].Arguments -ne ` + powershellLiteral(arguments) + ` -or -not $task.Actions[0].Execute.StartsWith(` + powershellLiteral(prefix) + `, [StringComparison]::OrdinalIgnoreCase) -or [IO.Path]::GetFileName($task.Actions[0].Execute) -notlike 'control-user-*.exe') { throw 'Legacy login action does not belong to this installation' }
`
}

func LegacyUpdateUserBootstrap(ctx context.Context, config, launcher, directory string) (string, error) {
	return userPowerShellOutput(ctx, userTaskScript(config, legacyUpdateUserCheck(config, launcher, directory)+"Write-Output $task.Actions[0].Execute"))
}

const legacyTaskDefinition = `
$scheduler = New-Object -ComObject 'Schedule.Service'
$scheduler.Connect()
$folder = $scheduler.GetFolder('\')
$registered = $folder.GetTask($name)
$definition = $registered.Definition
`

// Updating the unchanged definition checks write authority before shutdown.
// Only a single existing logon trigger is accepted, never a registration trigger;
// this does not execute or change its action, principal, triggers, settings or ACL.
func CheckLegacyUpdateUserTask(ctx context.Context, config, launcher, directory string) error {
	script := legacyUpdateUserCheck(config, launcher, directory) + legacyTaskDefinition + `
$folder.RegisterTaskDefinition($name, $definition, 4, $sid, $null, 3, $null) | Out-Null
`
	return userPowerShell(ctx, userTaskScript(config, script))
}

func SwitchLegacyUpdateUserTask(ctx context.Context, config, launcher, directory, runtime string) error {
	script := legacyUpdateUserCheck(config, launcher, directory) + `
$deadline = [DateTime]::UtcNow.AddSeconds(30)
while ($task.State -eq 'Running') {
  if ([DateTime]::UtcNow -ge $deadline) { throw 'Legacy login wrapper did not exit' }
  Start-Sleep -Milliseconds 100
  $task = Get-ScheduledTask -TaskPath '\' -TaskName $name -ErrorAction Stop
}
` + legacyTaskDefinition + `
$definition.Actions.Item(1).Path = ` + powershellLiteral(runtime) + `
$definition.Actions.Item(1).Arguments = ` + powershellLiteral(userTaskArguments(config)) + `
$folder.RegisterTaskDefinition($name, $definition, 4, $sid, $null, 3, $null) | Out-Null
`
	return userPowerShell(ctx, userTaskScript(config, script))
}
