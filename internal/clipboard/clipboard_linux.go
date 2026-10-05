package clipboard

import (
	"context"
	"errors"
	"os"
	"os/exec"
)

func command(ctx context.Context) (*exec.Cmd, error) {
	if os.Getenv("WAYLAND_DISPLAY") != "" {
		if path, err := exec.LookPath("wl-copy"); err == nil {
			return exec.CommandContext(ctx, path), nil
		}
	}
	if os.Getenv("DISPLAY") != "" {
		if path, err := exec.LookPath("xclip"); err == nil {
			return exec.CommandContext(ctx, path, "-selection", "clipboard"), nil
		}
		if path, err := exec.LookPath("xsel"); err == nil {
			return exec.CommandContext(ctx, path, "--clipboard", "--input"), nil
		}
	}
	return nil, errors.New("clipboard unavailable; install wl-copy, xclip, or xsel in a desktop session")
}
