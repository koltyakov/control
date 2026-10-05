#Requires -Version 5.1
[CmdletBinding()]
param([switch]$Help)
$ErrorActionPreference = 'Stop'
if ($Help) { Write-Host 'Install Control from GitHub Releases. Optional: CONTROL_RELEASE_REPO, CONTROL_VERSION, CONTROL_INSTALL_DIR.'; return }
$repo = if ($env:CONTROL_RELEASE_REPO) { $env:CONTROL_RELEASE_REPO } else { 'koltyakov/control' }
if ($repo -notmatch '^[a-zA-Z0-9_.-]+/[a-zA-Z0-9_.-]+$') { throw 'Invalid release repository' }
$nativeArch = if ($env:PROCESSOR_ARCHITEW6432) { $env:PROCESSOR_ARCHITEW6432 } else { $env:PROCESSOR_ARCHITECTURE }
$arch = switch ($nativeArch) { 'AMD64' {'amd64'} 'ARM64' {'arm64'} default { throw 'Unsupported architecture' } }
$base = "https://github.com/$repo/releases/latest/download"
if ($env:CONTROL_VERSION) {
  if ($env:CONTROL_VERSION -notmatch '^[a-zA-Z0-9_.+-]+$') { throw 'Invalid version' }
  $base = "https://github.com/$repo/releases/download/$env:CONTROL_VERSION"
}
$asset = "control_windows_$arch.exe"
$tmp = Join-Path ([IO.Path]::GetTempPath()) ([IO.Path]::GetRandomFileName())
New-Item -ItemType Directory -Path $tmp | Out-Null
try {
  $manifest = Invoke-RestMethod -Uri "$base/control-manifest.json"
  $metadata = @($manifest.assets | Where-Object { $_.file -eq $asset })
  if ($metadata.Count -ne 1) { throw 'Release does not contain this platform' }
  $exe = Join-Path $tmp 'control.exe'
  Invoke-WebRequest -UseBasicParsing -Uri "$base/$asset" -OutFile $exe
  if ((Get-FileHash -Algorithm SHA256 -LiteralPath $exe).Hash.ToLowerInvariant() -ne $metadata[0].sha256) { throw 'Binary checksum mismatch' }
  & $exe install-self
  if ($LASTEXITCODE -ne 0) { throw 'Installation failed' }
  $installDir = if ($env:CONTROL_INSTALL_DIR) { [IO.Path]::GetFullPath($env:CONTROL_INSTALL_DIR) } else { Join-Path $env:LOCALAPPDATA 'Programs\control' }
  if (($env:Path -split ';') -notcontains $installDir) { $env:Path = "$installDir;$env:Path" }
  Write-Host 'Next, in Administrator PowerShell: control setup --gateway https://YOUR_GATEWAY --name main --client opencode'
  Write-Host 'Control is on PATH in this session. Other applications may need a new login session to pick up the saved user PATH.'
} finally { Remove-Item -LiteralPath $tmp -Recurse -Force }
