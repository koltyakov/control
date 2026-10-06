//go:build !windows

package installation

import (
	"context"

	"github.com/koltyakov/control/internal/node"
)

func CheckServiceMode(mode string) error { return ValidateServiceMode(mode) }

func resolveServiceMode(_ string, mode string) (string, error) { return mode, nil }

func finishServiceStop(_ context.Context, _, _ string) error { return nil }

func checkServiceProfile(_, _ string) error { return nil }

func platformService(context.Context, string, string, string, string, node.Config) (bool, error) {
	return false, nil
}
