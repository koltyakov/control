package installation

import (
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"path/filepath"
	"strings"
	"unicode/utf16"
)

func powershellLiteral(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
}

// PowerShell's encoded commands use UTF-16LE, including surrogate pairs.
func encodedPowerShell(script string) string {
	units := utf16.Encode([]rune(script))
	data := make([]byte, len(units)*2)
	for i, unit := range units {
		data[2*i], data[2*i+1] = byte(unit), byte(unit>>8)
	}
	return base64.StdEncoding.EncodeToString(data)
}

func userTaskLauncher(binary, config string) string {
	// Start-Process joins ArgumentList into a Windows command line. Configuration
	// paths are files, so they cannot contain double quotes or end in a backslash.
	args := `__user "` + config + `"`
	return fmt.Sprintf("$ErrorActionPreference = 'Stop'; $p = Start-Process -FilePath %s -ArgumentList %s -WindowStyle Hidden -PassThru; $p.WaitForExit(); exit $p.ExitCode",
		powershellLiteral(binary), powershellLiteral(args))
}

func userTaskRegistration(binary, config string) string {
	arguments := "-NoProfile -NonInteractive -WindowStyle Hidden -EncodedCommand " + encodedPowerShell(userTaskLauncher(binary, config))
	return fmt.Sprintf(`$action = New-ScheduledTaskAction -Execute (Join-Path $PSHOME 'powershell.exe') -Argument %s
$trigger = New-ScheduledTaskTrigger -AtLogOn -User $sid
$principal = New-ScheduledTaskPrincipal -UserId $sid -LogonType Interactive -RunLevel Limited
$settings = New-ScheduledTaskSettingsSet -Hidden -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries -ExecutionTimeLimit ([TimeSpan]::Zero) -MultipleInstances IgnoreNew -RestartCount 3 -RestartInterval (New-TimeSpan -Minutes 1)
Register-ScheduledTask -TaskName $name -TaskPath '\' -Description $description -Action $action -Trigger $trigger -Principal $principal -Settings $settings -Force | Out-Null`, powershellLiteral(arguments))
}

func userTaskScript(config, operation string) string {
	return fmt.Sprintf(`$ErrorActionPreference = 'Stop'
$name = %s
$description = %s
$sid = [Security.Principal.WindowsIdentity]::GetCurrent().User.Value
# A missing named task is a CIM error even with SilentlyContinue, and can leave
# powershell.exe exiting with status 1. Filter a successful enumeration instead.
$task = Get-ScheduledTask -ErrorAction Stop | Where-Object { $_.TaskPath -eq '\' -and $_.TaskName -eq $name }
if ($task) {
  $taskSid = $task.Principal.UserId
  if ($taskSid -notlike 'S-1-*') { $taskSid = (New-Object Security.Principal.NTAccount($taskSid)).Translate([Security.Principal.SecurityIdentifier]).Value }
  if ($task.Description -ne $description -or $taskSid -ne $sid) { throw 'Control login task belongs to another profile or user' }
}
%s
`, powershellLiteral(userTaskName(config)), powershellLiteral("Control user node: "+config), operation)
}

func userTaskName(config string) string {
	// The caller passes an absolute, cleaned path, matching the SCM profile name.
	return "ControlUser-" + profileName(config)
}

func profileName(config string) string {
	sum := sha256.Sum256([]byte(strings.ToLower(filepath.Clean(config))))
	return fmt.Sprintf("%x", sum[:8])
}
