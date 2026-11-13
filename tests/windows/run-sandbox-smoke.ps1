param(
    [string]$InputDirectory = 'C:\ControlSmokeInput',
    [string]$ResultsDirectory = 'C:\ControlSmokeResults'
)

$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'
# Keep the shutdown below unreachable on the physical host or an ordinary VM.
if ($env:USERNAME -ne 'WDAGUtilityAccount' -or (Get-CimInstance Win32_ComputerSystem).Model -ne 'Virtual Machine') {
    throw 'This runner is restricted to the disposable Windows Sandbox account'
}
$identity = [Security.Principal.WindowsIdentity]::GetCurrent()
$principal = New-Object Security.Principal.WindowsPrincipal($identity)
$uacPath = 'HKLM:\SOFTWARE\Microsoft\Windows\CurrentVersion\Policies\System'
if ((Get-ItemProperty -LiteralPath $uacPath -Name EnableLUA).EnableLUA -ne 1) {
    if (-not $principal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) { throw 'Sandbox UAC preparation requires its existing administrator token' }
    # Sandbox disables UAC by default, so a Limited login task otherwise still
    # gets an administrator token. Reboot only this disposable guest with UAC
    # enabled, then run the harness as Highest while testing a Limited node.
    $action = New-ScheduledTaskAction -Execute 'C:\Windows\System32\WindowsPowerShell\v1.0\powershell.exe' -Argument ('-NoProfile -ExecutionPolicy Bypass -WindowStyle Hidden -File "' + $PSCommandPath + '"')
    $trigger = New-ScheduledTaskTrigger -AtLogOn -User $identity.User.Value
    $runnerPrincipal = New-ScheduledTaskPrincipal -UserId $identity.User.Value -LogonType Interactive -RunLevel Highest
    Register-ScheduledTask -TaskName 'ControlSmokeHarness' -Action $action -Trigger $trigger -Principal $runnerPrincipal | Out-Null
    Set-ItemProperty -LiteralPath $uacPath -Name EnableLUA -Value 1
    'Sandbox-only UAC reboot requested' | Set-Content -LiteralPath (Join-Path $ResultsDirectory 'preparation.log')
    shutdown.exe /r /t 0
    exit
}
if (-not $principal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) { exit }
# A logon command and the Highest runner must never start overlapping suites.
$startMarker = Join-Path $ResultsDirectory 'suite-started'
$marker = [IO.File]::Open($startMarker, [IO.FileMode]::CreateNew, [IO.FileAccess]::Write, [IO.FileShare]::None)
$marker.Close()
$exitCode = 1
$summary = [ordered]@{ completed = $false; exitCode = 1 }

function Get-SmokeRegistrations {
    [pscustomobject]@{
        services = @(Get-Service -Name 'ControlNode-*' -ErrorAction SilentlyContinue | Select-Object Name, Status)
        tasks = @(Get-ScheduledTask -TaskPath '\' | Where-Object { $_.TaskName -like 'ControlUser-*' -or $_.TaskName -like 'ControlUpdate-*' -or $_.TaskName -like 'ControlSmokeInstall-*' } | Select-Object TaskName, State)
    }
}

try {
    $identity = [Security.Principal.WindowsIdentity]::GetCurrent()
    $principal = New-Object Security.Principal.WindowsPrincipal($identity)
    $summary.identity = $identity.Name
    $summary.elevated = $principal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)
    $summary.os = Get-CimInstance Win32_OperatingSystem | Select-Object Caption, Version, BuildNumber
    $summary.architecture = [Runtime.InteropServices.RuntimeInformation]::OSArchitecture.ToString()
    $summary.before = Get-SmokeRegistrations
    $manifest = Get-Content -LiteralPath (Join-Path $InputDirectory 'smoke-inputs.json') -Raw | ConvertFrom-Json
    foreach ($entry in $manifest.files) {
        $path = Join-Path $InputDirectory $entry.name
        $actual = (Get-FileHash -LiteralPath $path -Algorithm SHA256).Hash.ToLowerInvariant()
        if ($actual -ne $entry.sha256) { throw ('Smoke input checksum mismatch: ' + $entry.name) }
    }
    $env:CONTROL_HOME = 'C:\ControlSmokeHome'
    $env:CONTROL_TEST_WINDOWS_BINARY = Join-Path $InputDirectory 'control-before.exe'
    $env:CONTROL_TEST_WINDOWS_UPDATED_BINARY = Join-Path $InputDirectory 'control-after.exe'
    if (Test-Path -LiteralPath (Join-Path $InputDirectory 'control-legacy.exe')) {
        $env:CONTROL_TEST_WINDOWS_LEGACY_BINARY = Join-Path $InputDirectory 'control-legacy.exe'
    }
    $env:CONTROL_TEST_WINDOWS_SERVICE = '1'
    $env:CONTROL_TEST_WINDOWS_USER_STARTUP = '1'
    $env:CONTROL_TEST_WINDOWS_UPDATE_SMOKE = '1'
    $selection = '^(TestWindowsServiceLifecycle|TestWindowsUserStartupLifecycle|TestWindowsManagedStartupUpdateSmoke|TestWindowsTemporaryHandoffReplacesRuntimeAndFinishes|TestWindowsHandoffEOFDoesNotCommit|TestWindowsGUIRuntimeReportsOnlyVerifiedSource|TestWindowsManagedExecutableStablePath|TestWindowsManagedExecutableLaunchesVerifiedBinary)$'
    $testProcess = Start-Process -FilePath (Join-Path $InputDirectory 'control-smoke-tests.exe') `
        -ArgumentList @('-test.v', ('-test.run=' + $selection), '-test.timeout=15m') `
        -NoNewWindow -PassThru `
        -RedirectStandardOutput (Join-Path $ResultsDirectory 'tests.stdout.log') `
        -RedirectStandardError (Join-Path $ResultsDirectory 'tests.stderr.log')
    # The process fixture deliberately inherits the runner's scheduled-task
    # job. Updating must not require breakaway from that enclosing job.
    # Retain a native handle before exit. Windows PowerShell can otherwise
    # return a Process object whose ExitCode stays null after WaitForExit.
    $testHandle = $testProcess.Handle
    $testProcess.WaitForExit()
    $testProcess.Refresh()
    $nativeExitCode = $testProcess.ExitCode
    if ($null -eq $nativeExitCode) { throw 'Native test process did not provide an exit code' }
    $exitCode = [int]$nativeExitCode
    $summary.testExitCode = $exitCode
    $summary.after = Get-SmokeRegistrations
    if ($summary.after.services.Count -ne $summary.before.services.Count -or $summary.after.tasks.Count -ne $summary.before.tasks.Count) {
        $summary.cleanupError = 'SCM or login-task registrations remain after the fixture suite'
        $exitCode = 1
    }
    $summary.completed = $true
} catch {
    $summary.error = $_.ToString()
} finally {
    $summary.exitCode = $exitCode
    $summary.finishedAt = [DateTime]::UtcNow.ToString('o')
    $temporary = Join-Path $ResultsDirectory 'summary.pending.json'
    $summary | ConvertTo-Json -Depth 8 | Set-Content -LiteralPath $temporary -Encoding UTF8
    Move-Item -LiteralPath $temporary -Destination (Join-Path $ResultsDirectory 'summary.json')
    Get-ScheduledTask -TaskName 'ControlSmokeHarness' -TaskPath '\' -ErrorAction SilentlyContinue | Unregister-ScheduledTask -Confirm:$false
    # The mapped result folder survives; the VM, its services and all local
    # state are discarded. Never run this runner on the physical host.
    shutdown.exe /s /t 0
}
