package enrollment

import (
	"bytes"
	"strings"
	"text/template"
)

func InstallCommand(link string, windows bool, mode string) string {
	if windows {
		// Invoke-RestMethod returns a plain-text response as one string, preserving
		// multi-line scripts when piped to Invoke-Expression.
		script := "irm " + PowerShellQuote(link) + "|iex"
		// Run in the caller's PowerShell, including its existing elevation.
		return script
	}
	if mode == "system" {
		return "curl -fsSL " + ShellQuote(link) + " | sudo bash"
	}
	return "curl -fsSL " + ShellQuote(link) + " | bash"
}

func Script(invitation Invitation, link string) (string, error) {
	source, quote := bashScript, ShellQuote
	if invitation.Asset.OS == "windows" {
		source, quote = powershellScript, PowerShellQuote
	}
	t, err := template.New("install").Funcs(template.FuncMap{"q": quote}).Parse(source)
	if err != nil {
		return "", err
	}
	var b bytes.Buffer
	err = t.Execute(&b, struct {
		Invitation
		Link, Binary, SystemLink string
	}{invitation, link, link + "/binary", link[:strings.LastIndex(link, "/")+1] + SystemTicket(link[strings.LastIndex(link, "/")+1:])})
	return b.String(), err
}

const bashScript = `#!/usr/bin/env bash
set -euo pipefail
umask 077
case "$(uname -s)" in Linux) os=linux;; Darwin) os=darwin;; *) echo 'Unsupported OS' >&2; exit 1;; esac
case "$(uname -m)" in x86_64|amd64) arch=amd64;; arm64|aarch64) arch=arm64;; *) echo 'Unsupported architecture' >&2; exit 1;; esac
if [ "$os" = darwin ] && [ "$(sysctl -n hw.optional.arm64 2>/dev/null || true)" = 1 ]; then arch=arm64; fi
if [ "$os" != {{q .Asset.OS}} ]; then echo 'Invitation OS does not match this machine' >&2; exit 1; fi
{{if .ServiceMode}}service_mode={{q .ServiceMode}}
{{else}}service_mode="${CONTROL_SERVICE_MODE:-}"
{{end}}{{if eq .ServiceMode "hybrid"}}
if [ "$(id -u)" = 0 ]; then echo 'Hybrid installation must run as the intended user, not root. Only its System installer uses sudo.' >&2; exit 1; fi
if [ "$os" = linux ]; then
  system_home='/var/lib/control/hybrid/{{.HybridID}}'
  system_bin='/usr/local/lib/control/{{.HybridID}}/control'
else
  system_home='/Library/Application Support/control/hybrid/{{.HybridID}}'
  system_bin='/Library/PrivilegedHelperTools/Control/{{.HybridID}}/control'
fi
system_installed=false
if sudo test -x "$system_bin" && sudo test -f "$system_home/node.json"; then system_installed=true; fi
service_mode=user
{{end}}set --
if [ -n "$service_mode" ]; then set -- --service "$service_mode"; fi
if [ "$service_mode" = system ]; then
  if [ "$(id -u)" != 0 ]; then echo 'System startup requires root; run this installation command with sudo bash.' >&2; exit 1; fi
  if [ "$os" = linux ]; then default_home=/var/lib/control; else default_home='/Library/Application Support/control'; fi
  export CONTROL_HOME="${CONTROL_HOME:-$default_home}"
  export CONTROL_INSTALL_DIR="${CONTROL_INSTALL_DIR:-/usr/local/bin}"
{{if eq .HybridRole "system"}}  export CONTROL_HOME="$default_home/hybrid/{{.HybridID}}"
  export CONTROL_CONFIG="$CONTROL_HOME/node.json"
  if [ "$os" = linux ]; then export CONTROL_INSTALL_DIR="/usr/local/lib/control/{{.HybridID}}"; else export CONTROL_INSTALL_DIR="/Library/PrivilegedHelperTools/Control/{{.HybridID}}"; fi
{{end}}
fi
case "$arch" in
{{range .Candidates}}{{.Arch}}) checksum={{q .SHA256}};;
{{end}}*) echo 'No installer for this architecture' >&2; exit 1;;
esac
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
try_local_binary() {
  [ -f "$1" ] && cp "$1" "$tmp/control" 2>/dev/null || return 1
  if command -v sha256sum >/dev/null; then actual="$(sha256sum "$tmp/control" | awk '{print $1}')"; else actual="$(shasum -a 256 "$tmp/control" | awk '{print $1}')"; fi
  [ "$actual" = "$checksum" ]
}
reused=false
{{if eq .ServiceMode "hybrid"}}# Copy out of the protected installation; never share a writable runtime.
if [ "$system_installed" = true ] && sudo cat "$system_bin" > "$tmp/control"; then
  if command -v sha256sum >/dev/null; then actual="$(sha256sum "$tmp/control" | awk '{print $1}')"; else actual="$(shasum -a 256 "$tmp/control" | awk '{print $1}')"; fi
  if [ "$actual" = "$checksum" ]; then reused=true; fi
fi
{{end}}if [ "$reused" = false ]; then
  for candidate in "./control" "./control_${os}_${arch}" "./bin/control" "./bin/control_${os}_${arch}" "${CONTROL_INSTALL_DIR:-$HOME/.local/bin}/control" "$HOME/.local/bin/control" "/usr/local/bin/control" "$(command -v control || true)"; do
    if try_local_binary "$candidate"; then reused=true; break; fi
  done
fi
if [ "$reused" = false ]; then
  curl -fsSL {{q .Binary}}"?arch=$arch" -o "$tmp/control"
fi
if command -v sha256sum >/dev/null; then actual="$(sha256sum "$tmp/control" | awk '{print $1}')"; else actual="$(shasum -a 256 "$tmp/control" | awk '{print $1}')"; fi
if [ "$actual" != "$checksum" ]; then echo 'Binary checksum mismatch' >&2; exit 1; fi
chmod 700 "$tmp/control"
{{if eq .ServiceMode "hybrid"}}# Find User problems before any privileged change or consumed ticket.
"$tmp/control" enroll --url {{q .Link}}{{if .AutoName}} --auto-name{{end}} --preflight "$@"
if [ "$system_installed" = true ]; then
  if sudo test -f "$system_home/pending-enrollment.json"; then
    sudo env CONTROL_HOME="$system_home" CONTROL_INSTALL_DIR="$(dirname "$system_bin")" "$system_bin" --config "$system_home/node.json" enroll --url {{q .SystemLink}} --service system --hybrid-system
  else
    sudo "$system_bin" --config "$system_home/node.json" service start
  fi
else
  # The System installer verifies ./control against the same pinned checksum.
  (cd "$tmp" && curl -fsSL {{q .SystemLink}} | sudo bash)
fi
{{if .Linger}}if [ "$os" = linux ] && ! sudo loginctl enable-linger "$(id -un)"; then echo 'Warning: could not enable linger; the User context stops at logout.' >&2; fi
{{end}}{{end}}"$tmp/control" enroll --url {{q .Link}}{{if .AutoName}} --auto-name{{end}}{{if eq .HybridRole "system"}} --hybrid-system{{end}} "$@"
`

const powershellScript = `#Requires -Version 5.1
$ErrorActionPreference = 'Stop'
{{if .ServiceMode}}$serviceMode = {{q .ServiceMode}}
{{else}}$serviceMode = if ($env:CONTROL_SERVICE_MODE) { $env:CONTROL_SERVICE_MODE } else { 'user' }
{{end}}if ($serviceMode -eq 'system') { $serviceMode = 'auto' }
if ($serviceMode -ne 'process' -and $serviceMode -ne 'user') {
  $principal = New-Object Security.Principal.WindowsPrincipal([Security.Principal.WindowsIdentity]::GetCurrent())
  if (-not $principal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) { throw 'Run this installation command in Administrator PowerShell to install the automatic Windows service.' }
}
{{if or (eq .ServiceMode "hybrid") (eq .HybridRole "system")}}# Elevated code must not run from a folder the limited user token can write.
$protectedHome = Join-Path $env:ProgramFiles 'Control Hybrid\{{.HybridID}}'
{{if eq .ServiceMode "hybrid"}}$systemExe = Join-Path $protectedHome 'bin\control.exe'
$systemConfig = Join-Path $protectedHome 'node.json'
$systemInstalled = (Test-Path -LiteralPath $systemExe) -and (Test-Path -LiteralPath $systemConfig)
$serviceMode = 'user'
{{end}}foreach ($directory in @((Split-Path $protectedHome), $protectedHome)) {
  New-Item -ItemType Directory -Force -Path $directory | Out-Null
  if ((Get-Item -LiteralPath $directory).Attributes -band [IO.FileAttributes]::ReparsePoint) { throw 'Hybrid System directories must not be reparse points' }
  $acl = New-Object Security.AccessControl.DirectorySecurity
  $acl.SetAccessRuleProtection($true, $false)
  $acl.SetOwner((New-Object Security.Principal.SecurityIdentifier('S-1-5-32-544')))
  foreach ($sid in @('S-1-5-18', 'S-1-5-32-544')) {
    $rule = New-Object Security.AccessControl.FileSystemAccessRule((New-Object Security.Principal.SecurityIdentifier($sid)), 'FullControl', 'ContainerInherit,ObjectInherit', 'None', 'Allow')
    $acl.AddAccessRule($rule)
  }
  # Permit traversal, not inheritance: User must never read System credentials.
  $userSid = [Security.Principal.WindowsIdentity]::GetCurrent().User
  $acl.AddAccessRule((New-Object Security.AccessControl.FileSystemAccessRule($userSid, 'ReadAndExecute', 'Allow')))
  Set-Acl -LiteralPath $directory -AclObject $acl
}
{{end}}{{if eq .HybridRole "system"}}$env:CONTROL_HOME = $protectedHome
$env:CONTROL_CONFIG = Join-Path $protectedHome 'node.json'
$env:CONTROL_INSTALL_DIR = Join-Path $protectedHome 'bin'
{{end}}
$arch = if ($env:PROCESSOR_ARCHITEW6432) { $env:PROCESSOR_ARCHITEW6432 } else { $env:PROCESSOR_ARCHITECTURE }
try { $arch = [Runtime.InteropServices.RuntimeInformation]::OSArchitecture.ToString() } catch { }
$arch = switch ($arch) { 'AMD64' {'amd64'} 'X64' {'amd64'} 'ARM64' {'arm64'} default {throw 'Unsupported architecture'} }
$checksum = switch ($arch) {
{{range .Candidates}}  {{q .Arch}} { {{q .SHA256}} }
{{end}}  default { throw 'No installer for this architecture' }
}
{{if or (eq .ServiceMode "hybrid") (eq .HybridRole "system")}}$tmp = Join-Path $protectedHome ([IO.Path]::GetRandomFileName())
{{else}}$tmp = Join-Path ([IO.Path]::GetTempPath()) ([IO.Path]::GetRandomFileName())
{{end}}New-Item -ItemType Directory -Path $tmp | Out-Null
{{if eq .ServiceMode "hybrid"}}# The limited child may execute verified staging bytes, but cannot change them.
$stageAcl = Get-Acl -LiteralPath $tmp
$stageAcl.SetOwner((New-Object Security.Principal.SecurityIdentifier('S-1-5-32-544')))
$stageAcl.AddAccessRule((New-Object Security.AccessControl.FileSystemAccessRule($userSid, 'ReadAndExecute', 'ContainerInherit,ObjectInherit', 'None', 'Allow')))
Set-Acl -LiteralPath $tmp -AclObject $stageAcl
{{end}}
try {
  $exe = Join-Path $tmp 'control.exe'
  $reused = $false
  function Copy-ControlBinary([string]$Source, [string]$Destination) {
    # Copy bytes only. Copy-Item may also copy a user-writable source DACL.
    $reader = [IO.File]::OpenRead($Source)
    try {
      $writer = [IO.File]::Create($Destination)
      try { $reader.CopyTo($writer) } finally { $writer.Dispose() }
    } finally { $reader.Dispose() }
  }
{{if eq .ServiceMode "hybrid"}}  # Copy out of the protected installation; never share a writable runtime.
  if ($systemInstalled) {
    try {
      Copy-ControlBinary $systemExe $exe
      $reused = (Get-FileHash -Algorithm SHA256 -LiteralPath $exe).Hash.ToLowerInvariant() -eq $checksum
    } catch { $reused = $false }
  }
{{end}}  if (-not $reused) {
    $installDir = if ($env:CONTROL_INSTALL_DIR) { $env:CONTROL_INSTALL_DIR } else { Join-Path $env:LOCALAPPDATA 'Programs\control' }
    $asset = 'control_windows_' + $arch + '.exe'
    $candidates = @((Join-Path $PWD.Path 'control.exe'), (Join-Path $PWD.Path $asset), (Join-Path $PWD.Path 'bin\control.exe'), (Join-Path (Join-Path $PWD.Path 'bin') $asset), (Join-Path $installDir 'control.exe'), (Join-Path $env:LOCALAPPDATA 'Programs\control\control.exe'))
    $candidates += @(Get-Command control.exe -CommandType Application -All -ErrorAction SilentlyContinue | Select-Object -ExpandProperty Source)
    foreach ($candidate in $candidates) {
      try {
        if (-not (Test-Path -LiteralPath $candidate -PathType Leaf)) { continue }
        Copy-ControlBinary $candidate $exe
        if ((Get-FileHash -Algorithm SHA256 -LiteralPath $exe).Hash.ToLowerInvariant() -eq $checksum) { $reused = $true; break }
      } catch { continue }
    }
  }
  if (-not $reused) {
    # A child scope suppresses slow PowerShell 5.1 progress without changing the caller.
    & {
      $ProgressPreference = 'SilentlyContinue'
      Invoke-WebRequest -UseBasicParsing -Uri ({{q .Binary}} + '?arch=' + $arch) -OutFile $exe
    }
  }
  if ((Get-FileHash -Algorithm SHA256 -LiteralPath $exe).Hash.ToLowerInvariant() -ne $checksum) { throw 'Binary checksum mismatch' }
{{if eq .ServiceMode "hybrid"}}  # Run User preflight with the same user's limited token, never Administrator.
  & $exe __hybrid-user --url {{q .Link}}{{if .AutoName}} --auto-name{{end}} --preflight
  if ($LASTEXITCODE -ne 0) { throw 'Hybrid preflight failed; nothing was installed' }
  $savedHome, $savedConfig, $savedInstallDir = $env:CONTROL_HOME, $env:CONTROL_CONFIG, $env:CONTROL_INSTALL_DIR
  try {
    if ($systemInstalled) {
      # Check every existing privileged path before executing the installed copy.
      & $exe __hybrid-system-check $systemExe $systemConfig
      if ($LASTEXITCODE -ne 0) { throw 'Hybrid System recovery rejected unsafe installed paths' }
      if (Test-Path -LiteralPath (Join-Path $protectedHome 'pending-enrollment.json')) {
        $env:CONTROL_HOME = $protectedHome
        $env:CONTROL_INSTALL_DIR = Join-Path $protectedHome 'bin'
        & $systemExe --config $systemConfig enroll --url {{q .SystemLink}} --service system --hybrid-system --firewall
      } else {
        & $systemExe --config $systemConfig service start
      }
      if ($LASTEXITCODE -ne 0) { throw 'Hybrid System recovery failed; User installation was not started' }
    } else {
      # The System installer verifies .\control.exe against the same pinned checksum.
      Push-Location -LiteralPath $tmp
      try {
        $systemScript = & {
          $ProgressPreference = 'SilentlyContinue'
          Invoke-RestMethod -Uri {{q .SystemLink}}
        }
        & ([ScriptBlock]::Create($systemScript))
      } finally { Pop-Location }
    }
  } finally {
    $env:CONTROL_HOME, $env:CONTROL_CONFIG, $env:CONTROL_INSTALL_DIR = $savedHome, $savedConfig, $savedInstallDir
  }
  # The protected parent configures User firewall rules without elevating its CLI.
  & $exe __hybrid-user --url {{q .Link}}{{if .AutoName}} --auto-name{{end}}
{{else}}  & $exe enroll --url {{q .Link}}{{if .AutoName}} --auto-name{{end}}{{if eq .HybridRole "system"}} --hybrid-system{{end}} --service $serviceMode --firewall
{{end}}
  if ($LASTEXITCODE -ne 0) { throw 'Control enrollment failed' }
} finally {
  Remove-Item -LiteralPath $tmp -Recurse -Force
{{if eq .ServiceMode "hybrid"}}  if (-not (Get-ChildItem -LiteralPath $protectedHome -Force)) { Remove-Item -LiteralPath $protectedHome -ErrorAction SilentlyContinue }
{{end}}}
`
