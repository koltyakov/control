package shellpty

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

// shellNames are the accepted Windows shells. Windows PowerShell is always
// present on supported versions; pwsh is PowerShell 7 when installed. bash is
// not offered: WSL's bash.exe enters a separate Linux environment, and Git
// for Windows bash needs its login profile to set up a usable PATH.
var shellNames = []string{"powershell", "cmd", "pwsh"}

// Shells start without profiles or cmd AutoRun commands, so worker-local
// startup scripts and registry settings do not change what a Control session
// runs. They keep the node's environment apart from TERM.
func resolveShell(name string) (shell, error) {
	system, err := windows.GetSystemDirectory()
	if err != nil {
		return shell{}, fmt.Errorf("system directory: %w", err)
	}
	var candidates, flags []string
	switch name {
	case "cmd":
		candidates, flags = []string{filepath.Join(system, "cmd.exe")}, []string{"/D"}
	case "powershell":
		candidates = []string{filepath.Join(system, `WindowsPowerShell\v1.0\powershell.exe`)}
		flags = []string{"-NoLogo", "-NoProfile"}
	case "pwsh":
		candidates, flags = pwshCandidates(system), []string{"-NoLogo", "-NoProfile"}
	default:
		return shell{}, fmt.Errorf("%w %q; supported shells: %v", ErrUnsupportedShell, name, shellNames)
	}
	for _, path := range candidates {
		if executable(path) {
			return shell{path: path, args: append([]string{path}, flags...)}, nil
		}
	}
	return shell{}, fmt.Errorf("%w: %s", ErrShellNotFound, name)
}

// pwshCandidates checks the node's PATH, which covers MSI, winget, and Store
// installations, then the default MSI location. Relative PATH matches are
// rejected by LookPath, and system-directory matches are not PowerShell 7.
func pwshCandidates(system string) []string {
	var candidates []string
	if path, err := exec.LookPath("pwsh.exe"); err == nil && filepath.IsAbs(path) && !within(path, system) {
		candidates = append(candidates, path)
	}
	if programs, err := windows.KnownFolderPath(windows.FOLDERID_ProgramFiles, 0); err == nil {
		candidates = append(candidates, filepath.Join(programs, `PowerShell\7\pwsh.exe`))
	}
	return candidates
}

func within(path, dir string) bool {
	rel, err := filepath.Rel(dir, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, `..\`) && !filepath.IsAbs(rel)
}

// executable accepts regular files and the reparse points Windows uses for
// Store app execution aliases, which CreateProcess can start.
func executable(path string) bool {
	info, err := os.Lstat(path)
	return err == nil && !info.IsDir() && strings.EqualFold(filepath.Ext(path), ".exe")
}
