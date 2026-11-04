//go:build !windows

package upload

import "os"

func syncParent(root *os.Root) error {
	f, err := root.Open(".")
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	return f.Sync()
}

func syncStage(root *os.Root) error {
	dir, err := root.OpenRoot(StagingDir)
	if err != nil {
		return err
	}
	defer func() { _ = dir.Close() }()
	return syncParent(dir)
}
