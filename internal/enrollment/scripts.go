package enrollment

import (
	"bytes"
	"text/template"
)

func InstallCommand(link string, windows bool) string {
	if windows {
		return "& ([scriptblock]::Create((Invoke-RestMethod -Uri " + PowerShellQuote(link) + ")))"
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
if [ "$os/$arch" != {{q (printf "%s/%s" .Asset.OS .Asset.Arch)}} ]; then echo 'Invitation platform does not match this machine' >&2; exit 1; fi
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
curl -fsSL {{q .Binary}} -o "$tmp/control"
if command -v sha256sum >/dev/null; then actual="$(sha256sum "$tmp/control" | awk '{print $1}')"; else actual="$(shasum -a 256 "$tmp/control" | awk '{print $1}')"; fi
if [ "$actual" != {{q .Asset.SHA256}} ]; then echo 'Binary checksum mismatch' >&2; exit 1; fi
chmod 700 "$tmp/control"
"$tmp/control" enroll --url {{q .Link}}
`

const powershellScript = `#Requires -Version 5.1
$ErrorActionPreference = 'Stop'
$arch = if ($env:PROCESSOR_ARCHITEW6432) { $env:PROCESSOR_ARCHITEW6432 } else { $env:PROCESSOR_ARCHITECTURE }
$arch = switch ($arch) { 'AMD64' {'amd64'} 'ARM64' {'arm64'} default {throw 'Unsupported architecture'} }
if ($arch -ne {{q .Asset.Arch}}) { throw 'Invitation platform does not match this machine' }
$tmp = Join-Path ([IO.Path]::GetTempPath()) ([IO.Path]::GetRandomFileName())
New-Item -ItemType Directory -Path $tmp | Out-Null
try {
  $exe = Join-Path $tmp 'control.exe'
  Invoke-WebRequest -UseBasicParsing -Uri {{q .Binary}} -OutFile $exe
  if ((Get-FileHash -Algorithm SHA256 -LiteralPath $exe).Hash.ToLowerInvariant() -ne {{q .Asset.SHA256}}) { throw 'Binary checksum mismatch' }
  & $exe enroll --url {{q .Link}}
  if ($LASTEXITCODE -ne 0) { throw 'Control enrollment failed' }
} finally { Remove-Item -LiteralPath $tmp -Recurse -Force }
`
