// Package clipboard copies text through the host's clipboard tool.
package clipboard

import (
	"context"
	"fmt"
	"strings"
	"time"
)

func Copy(ctx context.Context, text string) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cmd, err := command(ctx)
	if err != nil {
		return err
	}
	cmd.Stdin = strings.NewReader(text)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("copy to clipboard: %w", err)
	}
	return nil
}
