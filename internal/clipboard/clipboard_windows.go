package clipboard

import (
	"context"
	"os/exec"
	"syscall"
)

func command(ctx context.Context) (*exec.Cmd, error) {
	return powershell(ctx, "$ErrorActionPreference = 'Stop'; [Console]::InputEncoding = [System.Text.UTF8Encoding]::new($false); Set-Clipboard -Value ([Console]::In.ReadToEnd())"), nil
}

func read(ctx context.Context) (Value, error) {
	return readJSON(powershell(ctx, `
$ErrorActionPreference = 'Stop'
[Console]::OutputEncoding = [System.Text.UTF8Encoding]::new($false)
Add-Type -AssemblyName System.Windows.Forms
$paths = @()
$text = ''
if ([System.Windows.Forms.Clipboard]::ContainsFileDropList()) {
  $paths = @([System.Windows.Forms.Clipboard]::GetFileDropList() | ForEach-Object { [string]$_ })
} elseif ([System.Windows.Forms.Clipboard]::ContainsText()) {
  $text = [System.Windows.Forms.Clipboard]::GetText()
} else {
  throw 'Clipboard has no text or regular files'
}
@{ paths = $paths; text = $text } | ConvertTo-Json -Compress`))
}

func powershell(ctx context.Context, script string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "powershell.exe", "-NoProfile", "-NonInteractive", "-STA", "-Command", script)
	// Clipboard access still uses the logged-in desktop, without opening a console window.
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000}
	return cmd
}
