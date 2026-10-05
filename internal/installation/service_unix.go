//go:build !windows

package installation

import (
	"context"

	"github.com/koltyakov/control/internal/node"
)

func CheckServiceMode(mode string) error { return ValidateServiceMode(mode) }

func platformService(context.Context, string, string, string, string, node.Config) (bool, error) {
	return false, nil
}
