package installation

import (
	"context"
	"fmt"
)

func tunnelTaskScript(binary, dir string) string {
	launcher := fmt.Sprintf("$p = Start-Process -FilePath %s -ArgumentList %s -WindowStyle Hidden -PassThru; $p.WaitForExit(); exit $p.ExitCode", powershellLiteral(binary), powershellLiteral(`__tunnels "`+dir+`"`))
	arguments := "-NoProfile -NonInteractive -WindowStyle Hidden -EncodedCommand " + encodedPowerShell(launcher)
	return fmt.Sprintf(`$name = %s
$description = %s
$sid = [Security.Principal.WindowsIdentity]::GetCurrent().User.Value
$task = Get-ScheduledTask -ErrorAction Stop | Where-Object { $_.TaskPath -eq '\' -and $_.TaskName -eq $name }
if ($task) {
  $taskSid = $task.Principal.UserId
  if ($taskSid -notlike 'S-1-*') { $taskSid = (New-Object Security.Principal.NTAccount($taskSid)).Translate([Security.Principal.SecurityIdentifier]).Value }
  if ($task.Description -ne $description -or $taskSid -ne $sid) { throw 'Control tunnel task belongs to another profile or user' }
}
$action = New-ScheduledTaskAction -Execute (Join-Path $PSHOME 'powershell.exe') -Argument %s
$trigger = New-ScheduledTaskTrigger -AtLogOn -User $sid
$principal = New-ScheduledTaskPrincipal -UserId $sid -LogonType Interactive -RunLevel Limited
$settings = New-ScheduledTaskSettingsSet -Hidden -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries -ExecutionTimeLimit ([TimeSpan]::Zero) -MultipleInstances IgnoreNew -RestartCount 999 -RestartInterval (New-TimeSpan -Minutes 1)
Register-ScheduledTask -TaskName $name -TaskPath '\' -Description $description -Action $action -Trigger $trigger -Principal $principal -Settings $settings -Force | Out-Null
Start-ScheduledTask -TaskName $name -TaskPath '\'
`, powershellLiteral("ControlTunnels-"+profileName(dir)), powershellLiteral("Control persistent tunnels: "+dir), powershellLiteral(arguments))
}

func installTunnelService(ctx context.Context, binary, dir string) error {
	return userPowerShell(ctx, tunnelTaskScript(binary, dir))
}
