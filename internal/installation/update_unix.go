//go:build !windows

package installation

import "os"

func replaceCLI(staged, path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if err = os.Chmod(staged, info.Mode().Perm()); err != nil {
		return err
	}
	return os.Rename(staged, path)
}
