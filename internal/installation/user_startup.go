package installation

import (
	"context"
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

// Installation helpers use Windows PowerShell's built-in modules, not a
// PSModulePath inherited from PowerShell 7 or an unrelated application.
func powerShellEnvironment(environment []string, modules string) []string {
	values := make([]string, 0, len(environment)+1)
	for _, value := range environment {
		name, _, _ := strings.Cut(value, "=")
		if !strings.EqualFold(name, "PSModulePath") {
			values = append(values, value)
		}
	}
	return append(values, "PSModulePath="+modules)
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

func userTaskArguments(config string) string {
	// Absolute file paths cannot contain double quotes or end in a separator.
	return `__user-direct "` + config + `"`
}

// The protected Hybrid parent may remove a matching legacy login task that its
// limited child cannot delete. Preflight must never perform this preparation.
func runHybridUserPhase(ctx context.Context, preflight bool, prepare, enroll func() error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !preflight {
		if err := prepare(); err != nil {
			return fmt.Errorf("prepare Hybrid User login startup from protected installer: %w", err)
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := enroll(); err != nil {
		return fmt.Errorf("limited-token Hybrid User enrollment: %w", err)
	}
	return nil
}

func userTaskRemovalScript(config string) string {
	return userTaskScript(config, "if ($task) { Unregister-ScheduledTask -InputObject $task -Confirm:$false }")
}

func userTaskRegistration(launcher, config string) string {
	return fmt.Sprintf(`$action = New-ScheduledTaskAction -Execute %s -Argument %s
$trigger = New-ScheduledTaskTrigger -AtLogOn -User $sid
$principal = New-ScheduledTaskPrincipal -UserId $sid -LogonType Interactive -RunLevel Limited
$settings = New-ScheduledTaskSettingsSet -Hidden -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries -ExecutionTimeLimit ([TimeSpan]::Zero) -MultipleInstances IgnoreNew -RestartCount 3 -RestartInterval (New-TimeSpan -Minutes 1)
Register-ScheduledTask -TaskName $name -TaskPath '\' -Description $description -Action $action -Trigger $trigger -Principal $principal -Settings $settings -Force | Out-Null
# An elevated installer otherwise leaves only a read grant for the Limited user.
# Preserve every existing ACE and grant maintenance only on this user's task.
$scheduler = New-Object -ComObject 'Schedule.Service'
$scheduler.Connect()
$registered = $scheduler.GetFolder('\').GetTask($name)
$security = [Security.AccessControl.RawSecurityDescriptor]::new($registered.GetSecurityDescriptor(7))
$maintenance = 0x1201bf # FILE_GENERIC_READ | FILE_GENERIC_WRITE | FILE_GENERIC_EXECUTE.
$granted = 0
foreach ($ace in $security.DiscretionaryAcl) {
  if ($ace.SecurityIdentifier.Value -eq $sid -and $ace.AceQualifier -eq [Security.AccessControl.AceQualifier]::AccessAllowed) { $granted = $granted -bor $ace.AccessMask }
}
if (($granted -band $maintenance) -ne $maintenance) {
  if ($null -eq $security.DiscretionaryAcl) { throw 'Control login task has no supported DACL' }
  $grant = [Security.AccessControl.CommonAce]::new([Security.AccessControl.AceFlags]::None, [Security.AccessControl.AceQualifier]::AccessAllowed, $maintenance, [Security.Principal.SecurityIdentifier]::new($sid), $false, $null)
  $security.DiscretionaryAcl.InsertAce(0, $grant)
  $registered.SetSecurityDescriptor($security.GetSddlForm([Security.AccessControl.AccessControlSections]::All), 0)
}`, powershellLiteral(launcher), powershellLiteral(userTaskArguments(config)))
}

func userTaskScript(config, operation string) string {
	return fmt.Sprintf(`$ErrorActionPreference = 'Stop'
$name = %s
$description = %s
$sid = [Security.Principal.WindowsIdentity]::GetCurrent().User.Value
# A missing named task is a CIM error even with SilentlyContinue, and can leave
# powershell.exe exiting with status 1. Filter a successful enumeration instead.
# Control tasks live in the root folder. Do not enumerate protected Windows
# subfolders that the limited installing user may not be allowed to inspect.
$task = Get-ScheduledTask -TaskPath '\' -ErrorAction Stop | Where-Object { $_.TaskPath -eq '\' -and $_.TaskName -eq $name }
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
