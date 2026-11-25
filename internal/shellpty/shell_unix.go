//go:build !windows

package shellpty

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// shellNames are the accepted Unix shells. bash is the default; pwsh is
// PowerShell 7 when it is installed.
var shellNames = []string{"bash", "pwsh"}

// Well-known locations are checked after the node's PATH, which a service
// manager may have reduced to system directories.
var shellLocations = map[string][]string{
	"bash": {"/bin/bash", "/usr/bin/bash", "/usr/local/bin/bash", "/opt/homebrew/bin/bash"},
	"pwsh": {"/usr/bin/pwsh", "/usr/local/bin/pwsh", "/opt/microsoft/powershell/7/pwsh", "/opt/homebrew/bin/pwsh", "/snap/bin/pwsh"},
}

// Shells start interactively without profiles or rc files, so worker-local
// startup scripts do not change what a Control session runs. They keep the
// node's environment apart from TERM.
var shellArgs = map[string][]string{
	"bash": {"bash", "--noprofile", "--norc", "-i"},
	"pwsh": {"pwsh", "-NoLogo", "-NoProfile"},
}

func resolveShell(name string) (shell, error) {
	args, ok := shellArgs[name]
	if !ok {
		return shell{}, fmt.Errorf("%w %q; supported shells: %v", ErrUnsupportedShell, name, shellNames)
	}
	candidates := shellLocations[name]
	// LookPath reports exec.ErrDot instead of returning a relative match.
	if path, err := exec.LookPath(name); err == nil && filepath.IsAbs(path) {
		candidates = append([]string{path}, candidates...)
	}
	for _, path := range candidates {
		if executable(path) {
			return shell{path: path, args: append([]string(nil), args...)}, nil
		}
	}
	return shell{}, fmt.Errorf("%w: %s", ErrShellNotFound, name)
}

func executable(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0
}
