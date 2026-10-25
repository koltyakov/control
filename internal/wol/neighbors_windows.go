package wol

import (
	"context"
	"os"
	"path/filepath"
)

func readNeighbors(ctx context.Context) ([]neighbor, error) {
	data, err := neighborCommand(ctx, filepath.Join(os.Getenv("SystemRoot"), "System32", "arp.exe"), "-a")
	if err != nil {
		return nil, err
	}
	return parseWindowsNeighbors(data), nil
}
