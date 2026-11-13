package main

import (
	"context"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"

	"github.com/koltyakov/control/internal/buildinfo"
	"github.com/koltyakov/control/internal/store"
	"github.com/koltyakov/control/internal/update"
)

type managedReadyKey struct{}

type managedReady struct {
	ID     string `json:"id"`
	SHA256 string `json:"sha256"`
}

func validManagedReady(marker managedReady) bool {
	id, err := hex.DecodeString(marker.ID)
	return err == nil && len(id) == 16 && update.ValidDigest(marker.SHA256)
}

func managedReadyPath(dir, id string) string {
	return filepath.Join(dir, "runtime-ready-"+id+".json")
}

func withManagedReady(ctx context.Context, dir string) context.Context {
	return context.WithValue(ctx, managedReadyKey{}, dir)
}

// Called only after startup checks and binding the actual service listener.
func signalManagedReady(ctx context.Context) error {
	dir, ok := ctx.Value(managedReadyKey{}).(string)
	if !ok {
		return nil
	}
	var marker managedReady
	if err := store.Read(filepath.Join(dir, "runtime-starting.json"), &marker); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if !validManagedReady(marker) || marker.SHA256 != buildinfo.Current().SHA256 {
		return nil // A manually restored previous selection must still start.
	}
	if err := store.Write(managedReadyPath(dir, marker.ID), marker); err != nil {
		return err
	}
	// The serving process owns runtime.lock here. Consume this pointer before a
	// later handoff can publish another one; helpers remove only their own ack.
	return os.Remove(filepath.Join(dir, "runtime-starting.json"))
}
