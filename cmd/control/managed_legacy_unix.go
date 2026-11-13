//go:build !windows

package main

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/koltyakov/control/internal/buildinfo"
	"github.com/koltyakov/control/internal/installation"
	"github.com/koltyakov/control/internal/store"
	"github.com/koltyakov/control/internal/update"
)

func migrateLegacyManaged(ctx context.Context, dir string, args []string) (bool, error) {
	if len(args) != 3 || args[0] != "node" || args[1] != "--config" {
		return false, errors.New("legacy Unix migration requires the installed node arguments")
	}
	profile, err := installation.ReadUpdateStartup(args[2], dir)
	if err != nil {
		return false, fmt.Errorf("single-process migration needs installation recovery: %w", err)
	}
	var selected runtimeBinary
	if err = store.Read(filepath.Join(dir, "runtime.json"), &selected); err != nil {
		return false, err
	}
	software := buildinfo.Current()
	if software.Version != selected.Version || !update.Matches(software, selected.Asset) {
		return false, errors.New("legacy startup selection differs from the loaded executable")
	}
	if err = installation.RefreshLegacyUnixStartup(ctx, profile, dir, selected); err != nil {
		return false, fmt.Errorf("single-process Unix migration blocked: %w", err)
	}
	if ctx.Err() != nil {
		return true, nil // An explicit shutdown must not ask the manager to restart.
	}
	// The old supervisor propagates this exit failure. Its existing, verified
	// restart-on-failure job then reopens the refreshed entry point. No detached
	// process, signal, registration mutation, or child-restart request is needed.
	return true, errors.New("legacy Unix entry refreshed; restarting once into the single-process runtime")
}
