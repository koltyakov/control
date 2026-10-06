package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/koltyakov/control/internal/installation"
)

func retiredUninstallCLI(ctx context.Context, args []string) error {
	if len(args) != 3 || !filepath.IsAbs(args[0]) || !filepath.IsAbs(args[1]) {
		return errors.New("retired uninstall requires absolute profile and state paths and an identity")
	}
	log, err := os.OpenFile(filepath.Join(filepath.Dir(args[0]), "node.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer func() { _ = log.Close() }()
	defer installation.FinishRetiredHelper(args[2])
	err = installation.UninstallRetired(ctx, args[0], args[1], args[2])
	if err != nil {
		_, _ = fmt.Fprintln(log, "Control retirement uninstall failed:", err)
	} else {
		_, _ = fmt.Fprintln(log, "Control retirement uninstall completed; configuration and work files retained")
	}
	return err
}
