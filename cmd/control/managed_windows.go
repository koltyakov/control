package main

import (
	"context"
	"fmt"
	"os"

	"github.com/koltyakov/control/internal/installation"
	"github.com/koltyakov/control/internal/update"
)

func managedExecutable(ctx context.Context, dir string, current runtimeBinary) (string, error) {
	if current.Version == "" {
		var err error
		current, err = inspectBinary(ctx, current.Path)
		if err != nil {
			return "", err
		}
	}
	path := installation.WindowsRuntimePath(dir)
	if update.Verify(path, current.Asset) == nil {
		return path, nil
	}
	source, err := os.Open(current.Path)
	if err != nil {
		return "", err
	}
	defer func() { _ = source.Close() }()
	// The previous child has exited before this is called. Preserve versioned
	// sources and runtime pointers; only replace the verified launch copy.
	if err = update.SaveBinary(source, path, current.Asset); err != nil {
		return "", fmt.Errorf("publish Windows runtime executable: %w", err)
	}
	return path, nil
}
