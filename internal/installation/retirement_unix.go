//go:build !windows

package installation

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
)

// Unix allows unlinking a running executable.
func removeRunningExecutable(path string) error { return os.RemoveAll(filepath.Dir(path)) }

func checkRetiredServiceFile(path, configText string) (bool, error) {
	content, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !bytes.Contains(content, []byte("Managed by control")) || !bytes.Contains(content, []byte(configText)) {
		return false, errors.New("startup registration belongs to another profile")
	}
	return true, nil
}
