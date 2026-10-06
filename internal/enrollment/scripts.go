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
		Link, Binary string
	}{invitation, link, link + "/binary"})
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
{{end}}set --
if [ -n "$service_mode" ]; then set -- --service "$service_mode"; fi
if [ "$service_mode" = system ]; then
  if [ "$(id -u)" != 0 ]; then echo 'System startup requires root; run this installation command with sudo bash.' >&2; exit 1; fi
  if [ "$os" = linux ]; then default_home=/var/lib/control; else default_home='/Library/Application Support/control'; fi
  export CONTROL_HOME="${CONTROL_HOME:-$default_home}"
  export CONTROL_INSTALL_DIR="${CONTROL_INSTALL_DIR:-/usr/local/bin}"
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
"$tmp/control" enroll --url {{q .Link}}{{if .AutoName}} --auto-name{{end}} "$@"
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
  & $exe enroll --url {{q .Link}}{{if .AutoName}} --auto-name{{end}} --service $serviceMode --firewall
  if ($LASTEXITCODE -ne 0) { throw 'Control enrollment failed' }
} finally { Remove-Item -LiteralPath $tmp -Recurse -Force }
`
