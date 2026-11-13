//go:build !windows

package main

import (
	"context"
	"os"
	"path/filepath"
	"syscall"

	"github.com/koltyakov/control/internal/update"
)

func withManagedNodeProfile(ctx context.Context, _ string) context.Context { return ctx }
func legacyManagedRuntime(context.Context) bool                            { return managedChild }

type unixManagedHandoff struct {
	ctx               context.Context
	dir               string
	args, environment []string
	current, next     runtimeBinary
	unlock            func() error
}

func prepareManagedHandoff(ctx context.Context, dir string, args []string, current, next runtimeBinary, unlock func() error) (managedHandoff, error) {
	return &unixManagedHandoff{ctx: ctx, dir: dir, args: append([]string(nil), args...), environment: os.Environ(), current: current, next: next, unlock: unlock}, nil
}

func (h *unixManagedHandoff) abort() {}

func (h *unixManagedHandoff) commit() error {
	if err := h.ctx.Err(); err != nil {
		return err
	}
	if err := update.ValidateExecutable(h.ctx, h.next.Path, h.next.Version, h.next.Asset); err != nil {
		return err
	}
	if h.current != h.next {
		if err := update.SaveRuntimeSwitch(h.dir, h.current, h.next); err != nil {
			return err
		}
	}
	if err := os.Remove(filepath.Join(h.dir, "runtime-request.json")); err != nil {
		return err
	}
	// No service work remains. The new image reacquires runtime.lock before
	// opening state or listeners. exec preserves PID 1, service-manager tracking,
	// the execution account, working directory and standard streams.
	if err := h.unlock(); err != nil {
		return err
	}
	return syscall.Exec(h.next.Path, append([]string{h.next.Path}, h.args...), h.environment)
}
