package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/gofrs/flock"
	"github.com/koltyakov/control/internal/buildinfo"
	"github.com/koltyakov/control/internal/store"
)

type testManagedHandoff struct {
	commitFunc func() error
	aborted    bool
}

func (h *testManagedHandoff) commit() error { return h.commitFunc() }
func (h *testManagedHandoff) abort()        { h.aborted = true }

func TestManagedRunsInProcessAndHoldsLock(t *testing.T) {
	dir := t.TempDir()
	pid := os.Getpid()
	err := managed(t.Context(), dir, nil, func(ctx context.Context, _ func(string) error) error {
		if os.Getpid() != pid {
			t.Fatal("service was moved to a child process")
		}
		lock := flock.New(filepath.Join(dir, "runtime.lock"))
		defer func() { _ = lock.Close() }()
		if locked, err := lock.TryLock(); err != nil || locked {
			t.Fatalf("service does not own runtime lock: %v, %v", locked, err)
		}
		if err := ctx.Err(); err != nil {
			t.Fatal(err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	lock := flock.New(filepath.Join(dir, "runtime.lock"))
	defer func() { _ = lock.Close() }()
	if locked, err := lock.TryLock(); err != nil || !locked {
		t.Fatalf("shutdown retained runtime lock: %v, %v", locked, err)
	}
}

func TestManagedHandoffWaitsForCleanupAndHonorsStop(t *testing.T) {
	t.Setenv("CONTROL_TEST_CLI_MAIN", "1")
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, outcome := range []string{"success", "shutdown failure", "explicit stop", "preparation failure"} {
		t.Run(outcome, func(t *testing.T) {
			dir := t.TempDir()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			cleaned, committed, prepared := false, false, 0
			handoff := &testManagedHandoff{commitFunc: func() error {
				if !cleaned {
					t.Fatal("committed before service cleanup")
				}
				committed = true
				return nil
			}}
			failure := errors.New("fixture failure")
			prepare := func(ctx context.Context, _ string, _ []string, _, _ runtimeBinary, _ func() error) (managedHandoff, error) {
				prepared++
				if ctx.Err() != nil {
					t.Fatal("updater got the cancelled service lifetime")
				}
				if outcome == "preparation failure" {
					return nil, failure
				}
				return handoff, nil
			}
			err := managedWithHandoff(ctx, dir, []string{"node", "--config", "profile with spaces"}, func(serviceCtx context.Context, apply func(string) error) error {
				defer func() { cleaned = true }()
				if err := apply(executable); err != nil {
					if outcome != "preparation failure" || !errors.Is(err, failure) {
						t.Fatal(err)
					}
					if serviceCtx.Err() != nil {
						t.Fatal("failed preparation shut down service")
					}
					return nil
				}
				if err := apply(executable); err != nil {
					t.Fatal(err)
				}
				<-serviceCtx.Done()
				if outcome == "explicit stop" {
					cancel()
				}
				if outcome == "shutdown failure" {
					return failure
				}
				return nil
			}, prepare)
			if outcome == "shutdown failure" {
				if !errors.Is(err, failure) {
					t.Fatalf("lost shutdown failure: %v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if committed != (outcome == "success") || prepared != 1 {
				t.Fatalf("unexpected handoff: committed=%v prepared=%d", committed, prepared)
			}
			if outcome != "preparation failure" && !handoff.aborted {
				t.Fatal("handoff cleanup not called")
			}
		})
	}
}

func TestManagedRejectsInvalidCandidateWithoutStopping(t *testing.T) {
	dir := t.TempDir()
	err := managed(t.Context(), dir, nil, func(ctx context.Context, apply func(string) error) error {
		if err := apply(filepath.Join(dir, "missing")); err == nil {
			t.Fatal("missing executable accepted")
		}
		if ctx.Err() != nil {
			t.Fatal("invalid update stopped service")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "runtime.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("invalid update changed runtime selection")
	}
}

func TestLegacyManagedPreservesSupervisorRequestContract(t *testing.T) {
	t.Setenv("CONTROL_TEST_CLI_MAIN", "1")
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	request := filepath.Join(t.TempDir(), "runtime-request.json")
	err = legacyManaged(t.Context(), request, func(ctx context.Context, apply func(string) error) error {
		if err := apply(executable); err != nil {
			return err
		}
		<-ctx.Done()
		return apply("ignored duplicate")
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var saved runtimeBinary
	if err = store.Read(request, &saved); err != nil || saved.Path != executable || saved.Asset.SHA256 == "" {
		t.Fatalf("legacy launcher cannot read request: %+v, %v", saved, err)
	}
}

func TestManagedRejectsCorruptSavedRuntimeBeforeOpeningService(t *testing.T) {
	dir := t.TempDir()
	if err := store.Write(filepath.Join(dir, "runtime.json"), runtimeBinary{Path: filepath.Join(dir, "missing"), Version: "fixture"}); err != nil {
		t.Fatal(err)
	}
	err := managed(t.Context(), dir, nil, func(context.Context, func(string) error) error {
		t.Fatal("service opened before saved executable validation")
		return nil
	})
	if err == nil {
		t.Fatal("corrupt saved selection was accepted")
	}
}

func TestManagedReadinessRequiresMatchingSoftware(t *testing.T) {
	dir := t.TempDir()
	ctx := withManagedReady(t.Context(), dir)
	if err := signalManagedReady(ctx); err != nil {
		t.Fatal(err)
	}
	marker := managedReady{ID: "stale", SHA256: "not-running"}
	if err := store.Write(filepath.Join(dir, "runtime-starting.json"), marker); err != nil {
		t.Fatal(err)
	}
	if err := signalManagedReady(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(managedReadyPath(dir, marker.ID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("stale marker acknowledged")
	}
	marker.ID, marker.SHA256 = "0123456789abcdef0123456789abcdef", buildinfo.Current().SHA256
	if err := store.Write(filepath.Join(dir, "runtime-starting.json"), marker); err != nil {
		t.Fatal(err)
	}
	if err := signalManagedReady(ctx); err != nil {
		t.Fatal(err)
	}
	var ready managedReady
	if err := store.Read(managedReadyPath(dir, marker.ID), &ready); err != nil || ready != marker {
		t.Fatalf("matching startup not acknowledged: %+v, %v", ready, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "runtime-starting.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("readiness did not consume its starting marker")
	}
}
