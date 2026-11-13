package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/gofrs/flock"
	"github.com/koltyakov/control/internal/buildinfo"
	"github.com/koltyakov/control/internal/store"
	"github.com/koltyakov/control/internal/update"
)

// Older installed launchers still invoke __managed. Retain their request/EOF
// contract until the installed launcher and startup registration are refreshed.
var managedChild bool

type runtimeBinary = update.RuntimeBinary

type managedHandoff interface {
	commit() error
	abort()
}

type managedHandoffFactory func(context.Context, string, []string, runtimeBinary, runtimeBinary, func() error) (managedHandoff, error)

// managed runs the service in this process. Only a verified update can request
// replacement, and replacement happens after all service-owned cleanup returns.
func managed(ctx context.Context, dir string, args []string, service func(context.Context, func(string) error) error) error {
	return managedWithHandoff(ctx, dir, args, service, prepareManagedHandoff)
}

func managedWithHandoff(ctx context.Context, dir string, args []string, service func(context.Context, func(string) error) error, prepare managedHandoffFactory) error {
	dir, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	if err = os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	request := filepath.Join(dir, "runtime-request.json")
	if legacyManagedRuntime(ctx) {
		handled, migrationErr := migrateLegacyManaged(ctx, dir, args)
		if handled {
			return migrationErr
		}
		return legacyManaged(ctx, request, service, migrationErr)
	}
	lock := flock.New(filepath.Join(dir, "runtime.lock"))
	locked, err := lock.TryLock()
	if err != nil {
		return err
	}
	if !locked {
		return errors.New("runtime data directory is already in use")
	}
	defer func() { _ = lock.Close() }()
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	current := runtimeBinary{Path: executable}
	if err = store.Read(filepath.Join(dir, "runtime.json"), &current); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err == nil && current.Version == "" {
		// Older supervisors retained their initial launcher without metadata in
		// runtime-previous.json. A manually restored selection still selects that
		// executable, rather than silently running whichever CLI was invoked.
		current, err = inspectBinary(ctx, current.Path)
		if err != nil {
			return err
		}
		if err = store.Write(filepath.Join(dir, "runtime.json"), current); err != nil {
			return err
		}
	}
	if current.Version != "" {
		if err = update.ValidateExecutable(ctx, current.Path, current.Version, current.Asset); err != nil {
			return fmt.Errorf("saved runtime: %w", err)
		}
		software := buildinfo.Current()
		if software.Version != current.Version || !update.Matches(software, current.Asset) {
			// A service-manager restart may invoke the original installed CLI.
			// Select the persisted runtime without leaving a resident launcher.
			if err = store.Write(request, current); err != nil {
				return err
			}
			handoff, err := prepare(ctx, dir, args, current, current, lock.Close)
			if err != nil {
				return err
			}
			defer handoff.abort()
			return handoff.commit()
		}
	}
	if err = os.Remove(request); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	serviceCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var mu sync.Mutex
	var handoff managedHandoff
	err = service(withManagedReady(serviceCtx, dir), func(path string) error {
		mu.Lock()
		defer mu.Unlock()
		if handoff != nil {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		next, err := inspectBinary(ctx, path)
		if err != nil {
			return err
		}
		if err = store.Write(request, next); err != nil {
			return err
		}
		handoff, err = prepare(ctx, dir, args, current, next, lock.Close)
		if err != nil {
			_ = os.Remove(request)
			return err
		}
		cancel()
		return nil
	})
	mu.Lock()
	defer mu.Unlock()
	if handoff == nil {
		return err
	}
	defer handoff.abort()
	if err != nil {
		return fmt.Errorf("service shutdown before update: %w", err)
	}
	if ctx.Err() != nil {
		return nil // An explicit stop/shutdown must not restart the service.
	}
	return handoff.commit()
}

func legacyManaged(ctx context.Context, request string, service func(context.Context, func(string) error) error, migrationErr error) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var mu sync.Mutex
	requested := false
	return service(ctx, func(path string) error {
		mu.Lock()
		defer mu.Unlock()
		if requested {
			return nil
		}
		binary, err := inspectBinary(ctx, path)
		if err != nil {
			return err
		}
		if migrationErr != nil && binary.Version == buildinfo.Current().Version {
			return migrationErr
		}
		if err = store.Write(request, binary); err != nil {
			return err
		}
		requested = true
		cancel()
		return nil
	})
}

func inspectBinary(ctx context.Context, path string) (runtimeBinary, error) {
	return update.InspectRuntime(ctx, path)
}
