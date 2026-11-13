package installation

import (
	"context"
	"errors"
	"strings"

	"golang.org/x/sys/windows"
)

// StartUpdateUserHelper starts once, in a separate Scheduler job, under the
// current interactive user's limited token. It has no trigger or restart policy.
// The caller must remove this exact temporary definition after preparation.
func StartUpdateUserHelper(ctx context.Context, name, binary string, args []string) error {
	if !strings.HasPrefix(name, "ControlUpdate-") || strings.ContainsAny(name, `\/`) {
		return errors.New("invalid temporary updater task name")
	}
	script := `$sid = [Security.Principal.WindowsIdentity]::GetCurrent().User.Value
$name = ` + powershellLiteral(name) + `
if (Get-ScheduledTask -TaskPath '\' | Where-Object { $_.TaskName -eq $name }) { throw 'Temporary updater task already exists' }
$action = New-ScheduledTaskAction -Execute ` + powershellLiteral(binary) + ` -Argument ` + powershellLiteral(windows.ComposeCommandLine(args)) + `
$principal = New-ScheduledTaskPrincipal -UserId $sid -LogonType Interactive -RunLevel Limited
$settings = New-ScheduledTaskSettingsSet -Hidden -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries -ExecutionTimeLimit (New-TimeSpan -Minutes 3)
Register-ScheduledTask -TaskPath '\' -TaskName $name -Action $action -Principal $principal -Settings $settings | Out-Null
Start-ScheduledTask -TaskPath '\' -TaskName $name
`
	return userPowerShell(ctx, script)
}

func RemoveUpdateUserHelper(ctx context.Context, name string) error {
	if !strings.HasPrefix(name, "ControlUpdate-") || strings.ContainsAny(name, `\/`) {
		return errors.New("invalid temporary updater task name")
	}
	return userPowerShell(ctx, `$name = `+powershellLiteral(name)+`
$task = Get-ScheduledTask -TaskPath '\' | Where-Object { $_.TaskName -eq $name }
if ($task) { Unregister-ScheduledTask -TaskPath '\' -TaskName $name -Confirm:$false }
`)
}
