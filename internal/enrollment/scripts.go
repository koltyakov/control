package enrollment

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"strings"
	"text/template"
	"unicode/utf16"
)

func InstallCommand(link string, windows bool, mode string) string {
	if windows {
		// Invoke-RestMethod returns a plain-text response as one string, preserving
		// multi-line scripts when piped to Invoke-Expression.
		script := "irm " + PowerShellQuote(link) + "|iex"
		// Keep unusual URLs literal in CMD, Bash, and the caller's PowerShell.
		if strings.ContainsAny(link, "\"$`\\%!\r\n") {
			units := utf16.Encode([]rune(script))
			encoded := make([]byte, len(units)*2)
			for i, unit := range units {
				binary.LittleEndian.PutUint16(encoded[i*2:], unit)
			}
			return "powershell.exe -NoProfile -EncodedCommand " + base64.StdEncoding.EncodeToString(encoded)
		}
		return `powershell.exe -NoProfile -c "` + script + `"`
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
if sudo test -x "$system_bin" && sudo test -f "$system_home/node.json"; then
  if sudo test -f "$system_home/pending-enrollment.json"; then
    sudo env CONTROL_HOME="$system_home" CONTROL_INSTALL_DIR="$(dirname "$system_bin")" "$system_bin" --config "$system_home/node.json" enroll --url {{q .SystemLink}} --service system --hybrid-system
  else
    sudo "$system_bin" --config "$system_home/node.json" service start
  fi
else
  curl -fsSL {{q .SystemLink}} | sudo bash
fi
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
curl -fsSL {{q .Binary}}"?arch=$arch" -o "$tmp/control"
if command -v sha256sum >/dev/null; then actual="$(sha256sum "$tmp/control" | awk '{print $1}')"; else actual="$(shasum -a 256 "$tmp/control" | awk '{print $1}')"; fi
if [ "$actual" != "$checksum" ]; then echo 'Binary checksum mismatch' >&2; exit 1; fi
chmod 700 "$tmp/control"
"$tmp/control" enroll --url {{q .Link}}{{if .AutoName}} --auto-name{{end}}{{if eq .HybridRole "system"}} --hybrid-system{{end}} "$@"
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
{{if eq .ServiceMode "hybrid"}}# Install the protected System companion before starting the limited user task.
$savedHome, $savedConfig, $savedInstallDir = $env:CONTROL_HOME, $env:CONTROL_CONFIG, $env:CONTROL_INSTALL_DIR
try {
  $systemHome = Join-Path $env:ProgramFiles 'Control Hybrid\{{.HybridID}}'
  $systemExe = Join-Path $systemHome 'bin\control.exe'
  $systemConfig = Join-Path $systemHome 'node.json'
  if ((Test-Path -LiteralPath $systemExe) -and (Test-Path -LiteralPath $systemConfig)) {
    if (Test-Path -LiteralPath (Join-Path $systemHome 'pending-enrollment.json')) {
      $env:CONTROL_HOME = $systemHome
      $env:CONTROL_INSTALL_DIR = Join-Path $systemHome 'bin'
      & $systemExe --config $systemConfig enroll --url {{q .SystemLink}} --service system --hybrid-system --firewall
    } else {
      & $systemExe --config $systemConfig service start
    }
    if ($LASTEXITCODE -ne 0) { throw 'Hybrid System recovery failed; User installation was not started' }
  } else {
    $systemScript = Invoke-RestMethod -Uri {{q .SystemLink}}
    & ([ScriptBlock]::Create($systemScript))
  }
} finally {
  $env:CONTROL_HOME, $env:CONTROL_CONFIG, $env:CONTROL_INSTALL_DIR = $savedHome, $savedConfig, $savedInstallDir
}
$serviceMode = 'user'
{{end}}{{if eq .HybridRole "system"}}$protectedHome = Join-Path $env:ProgramFiles 'Control Hybrid\{{.HybridID}}'
foreach ($directory in @((Split-Path $protectedHome), $protectedHome)) {
  New-Item -ItemType Directory -Force -Path $directory | Out-Null
  if ((Get-Item -LiteralPath $directory).Attributes -band [IO.FileAttributes]::ReparsePoint) { throw 'Hybrid System directories must not be reparse points' }
  $acl = New-Object Security.AccessControl.DirectorySecurity
  $acl.SetAccessRuleProtection($true, $false)
  $acl.SetOwner((New-Object Security.Principal.SecurityIdentifier('S-1-5-32-544')))
  foreach ($sid in @('S-1-5-18', 'S-1-5-32-544')) {
    $rule = New-Object Security.AccessControl.FileSystemAccessRule((New-Object Security.Principal.SecurityIdentifier($sid)), 'FullControl', 'ContainerInherit,ObjectInherit', 'None', 'Allow')
    $acl.AddAccessRule($rule)
  }
  Set-Acl -LiteralPath $directory -AclObject $acl
}
$env:CONTROL_HOME = $protectedHome
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
$tmp = Join-Path ([IO.Path]::GetTempPath()) ([IO.Path]::GetRandomFileName())
New-Item -ItemType Directory -Path $tmp | Out-Null
try {
  $exe = Join-Path $tmp 'control.exe'
  Invoke-WebRequest -UseBasicParsing -Uri ({{q .Binary}} + '?arch=' + $arch) -OutFile $exe
  if ((Get-FileHash -Algorithm SHA256 -LiteralPath $exe).Hash.ToLowerInvariant() -ne $checksum) { throw 'Binary checksum mismatch' }
  & $exe enroll --url {{q .Link}}{{if .AutoName}} --auto-name{{end}}{{if eq .HybridRole "system"}} --hybrid-system{{end}} --service $serviceMode --firewall
  if ($LASTEXITCODE -ne 0) { throw 'Control enrollment failed' }
} finally { Remove-Item -LiteralPath $tmp -Recurse -Force }
`
