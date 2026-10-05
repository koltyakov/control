package clipboard

import (
	"context"
	"os/exec"
)

func command(ctx context.Context) (*exec.Cmd, error) {
	return exec.CommandContext(ctx, "pbcopy"), nil
}
