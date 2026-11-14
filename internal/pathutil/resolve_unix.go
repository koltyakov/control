//go:build !windows

// Package pathutil resolves existing filesystem paths for path-policy checks.
// These checks do not replace rooted I/O or sandbox unrestricted processes.
package pathutil

import "path/filepath"

// Resolve returns the absolute path after following existing filesystem links.
func Resolve(path string) (string, error) {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	return filepath.Abs(resolved)
}
