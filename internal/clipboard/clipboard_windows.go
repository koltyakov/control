package clipboard

import (
	"context"
	"os/exec"
)

func command(ctx context.Context) (*exec.Cmd, error) {
	return exec.CommandContext(ctx, "powershell.exe", "-NoProfile", "-NonInteractive", "-Command", "Set-Clipboard -Value ([Console]::In.ReadToEnd())"), nil
}
