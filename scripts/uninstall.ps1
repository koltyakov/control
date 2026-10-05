#Requires -Version 5.1
$ErrorActionPreference = 'Stop'
$dir = if ($env:CONTROL_INSTALL_DIR) { $env:CONTROL_INSTALL_DIR } else { Join-Path $env:LOCALAPPDATA 'Programs\control' }
$exe = Join-Path $dir 'control.exe'
if (Test-Path -LiteralPath $exe) {
  & $exe service uninstall
  if ($LASTEXITCODE -ne 0) { throw 'Could not stop Control; executable retained' }
  $deadline = (Get-Date).AddSeconds(10)
  while (Test-Path -LiteralPath $exe) {
    try { Remove-Item -LiteralPath $exe -Force } catch {
      if ((Get-Date) -ge $deadline) { throw }
      Start-Sleep -Milliseconds 100
    }
  }
}
Write-Host 'Control executable and Windows service removed. Configuration, identities, and work files are retained.'
