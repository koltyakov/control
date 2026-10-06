package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"github.com/gofrs/flock"
	"github.com/koltyakov/control/internal/buildinfo"
	"github.com/koltyakov/control/internal/processutil"
	"github.com/koltyakov/control/internal/store"
	"github.com/koltyakov/control/internal/update"
)

var managedChild bool

type runtimeBinary struct {
	Path    string       `json:"path"`
	Version string       `json:"version"`
	Asset   update.Asset `json:"asset"`
}

// The launcher stays alive while the service restarts. Versioned executables
// avoid replacing a running Windows executable and preserve container PID 1.
func managed(ctx context.Context, dir string, args []string, service func(context.Context, func(string) error) error) error {
	dir, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	if err = os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	request := filepath.Join(dir, "runtime-request.json")
	pointer := filepath.Join(dir, "runtime.json")
	if managedChild {
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
			if err = store.Write(request, binary); err != nil {
				return err
			}
			requested = true
			cancel()
			return nil
		})
	}
	lock := flock.New(filepath.Join(dir, "runtime.lock"))
	locked, err := lock.TryLock()
	if err != nil {
		return err
	}
	if !locked {
		return errors.New("runtime data directory is already supervised")
	}
	defer func() { _ = lock.Close() }()
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	current := runtimeBinary{Path: executable}
	if err = store.Read(pointer, &current); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if current.Version != "" {
		if err = update.ValidateExecutable(ctx, current.Path, current.Version, current.Asset); err != nil {
			return fmt.Errorf("saved runtime: %w", err)
		}
	}
	_ = os.Remove(request)
	for {
		path, err := managedExecutable(ctx, dir, current)
		if err != nil {
			return err
		}
		cmd := exec.CommandContext(ctx, path, append([]string{"__managed"}, args...)...)
		processutil.HideWindow(cmd)
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
		// Closing stdin asks the child to shut down on all supported platforms.
		stopReader, stopWriter, err := os.Pipe()
		if err != nil {
			return err
		}
		cmd.Stdin = stopReader
		cmd.Cancel = func() error { return stopWriter.Close() }
		cmd.WaitDelay = 15 * time.Second
		err = cmd.Run()
		_ = stopReader.Close()
		_ = stopWriter.Close()
		if ctx.Err() != nil {
			return nil
		}
		var next runtimeBinary
		readErr := store.Read(request, &next)
		if errors.Is(readErr, os.ErrNotExist) {
			return err
		}
		if readErr != nil {
			return readErr
		}
		if err != nil {
			return fmt.Errorf("service shutdown before update: %w", err)
		}
		if err = update.ValidateExecutable(ctx, next.Path, next.Version, next.Asset); err != nil {
			return err
		}
		if err = store.Write(filepath.Join(dir, "runtime-previous.json"), current); err != nil {
			return err
		}
		if err = store.Write(pointer, next); err != nil {
			return err
		}
		if err = os.Remove(request); err != nil {
			return err
		}
		current = next
	}
}

func inspectBinary(ctx context.Context, path string) (runtimeBinary, error) {
	path, err := filepath.Abs(path)
	if err != nil {
		return runtimeBinary{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	info, err := update.InspectExecutable(ctx, path)
	if err != nil {
		return runtimeBinary{}, err
	}
	if info.OS != buildinfo.Current().OS || info.Arch != buildinfo.Current().Arch {
		return runtimeBinary{}, errors.New("runtime platform mismatch")
	}
	stat, err := os.Stat(path)
	if err != nil {
		return runtimeBinary{}, err
	}
	asset := update.Asset{OS: info.OS, Arch: info.Arch, File: update.AssetName(info.OS, info.Arch), Size: stat.Size(), SHA256: info.SHA256}
	if err = update.Verify(path, asset); err != nil {
		return runtimeBinary{}, err
	}
	return runtimeBinary{Path: path, Version: info.Version, Asset: asset}, nil
}
