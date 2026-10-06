//go:build !windows

package main

import "context"

func managedExecutable(_ context.Context, _ string, current runtimeBinary) (string, error) {
	return current.Path, nil
}
