package upload

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// CheckPath keeps upload metadata and staging out of ordinary filesystem RPCs,
// including existing symlink aliases. This is not a sandbox against programs
// running with the node user's unrestricted filesystem permissions.
func (m *Manager) CheckPath(name string) error {
	full := filepath.Join(m.workDir, name)
	for _, candidate := range []string{full, resolveExisting(full)} {
		for _, part := range strings.Split(filepath.ToSlash(candidate), "/") {
			if strings.EqualFold(part, StagingDir) {
				return Error("upload_private_path", "upload staging is private; use upload status")
			}
		}
		for _, dir := range []string{m.dir, m.privateDir} {
			rel, err := filepath.Rel(dir, candidate)
			if err == nil && (rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))) {
				return Error("upload_private_path", "upload metadata is private; use upload status")
			}
		}
	}
	return nil
}

func resolveExisting(name string) string {
	resolved, err := filepath.EvalSymlinks(name)
	if err == nil {
		return resolved
	}
	if !errors.Is(err, os.ErrNotExist) || filepath.Dir(name) == name {
		return name
	}
	return filepath.Join(resolveExisting(filepath.Dir(name)), filepath.Base(name))
}
