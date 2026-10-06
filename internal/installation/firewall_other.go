//go:build !windows

package installation

import (
	"context"
	"errors"

	"github.com/koltyakov/control/internal/node"
)

func configureFirewall(context.Context, string, string, node.Config) error {
	return errors.New("automatic firewall configuration is supported only on Windows")
}
