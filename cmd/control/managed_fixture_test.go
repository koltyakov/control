package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/koltyakov/control/internal/store"
)

// Appended bytes distinguish valid executable images without rebuilding. This
// dispatch is test-only; version inspection still uses the real CLI entry point.
func managedExecFixture() error {
	dir, next := os.Args[2], os.Args[3]
	return managed(context.Background(), dir, os.Args[1:], func(ctx context.Context, apply func(string) error) error {
		if _, err := os.Stat(filepath.Join(dir, "cleaned")); os.IsNotExist(err) {
			if err = apply(next); err != nil {
				return err
			}
			<-ctx.Done()
			return os.WriteFile(filepath.Join(dir, "cleaned"), []byte("yes"), 0600)
		}
		cwd, err := os.Getwd()
		if err != nil {
			return err
		}
		result := struct {
			PID              int
			CWD, Environment string
			Args             []string
		}{os.Getpid(), cwd, os.Getenv("CONTROL_TEST_EXEC_ENV"), os.Args[1:]}
		if err = store.Write(filepath.Join(dir, "result.json"), result); err != nil {
			return err
		}
		if err = signalManagedReady(ctx); err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(result)
	})
}
