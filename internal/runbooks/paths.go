// Package runbooks discovers bounded, reference-only operational instructions.
// It never executes instructions or resolves credentials.
package runbooks

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
)

const ProjectDirectory = ".control/runbooks"

type Root struct {
	Source string
	Path   string
	Error  string
}

// UserDirectory follows the approved OS-native, per-user data locations. It is
// deliberately independent of node profiles, CONTROL_HOME, and editor versions.
func UserDirectory() (string, error) {
	key := ""
	switch runtime.GOOS {
	case "windows":
		key = "LOCALAPPDATA"
	case "linux":
		key = "XDG_STATE_HOME"
	}
	if key != "" && filepath.IsAbs(os.Getenv(key)) {
		return filepath.Join(os.Getenv(key), "control", "runbooks"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(home) {
		return "", errors.New("user home must be absolute for global runbook discovery")
	}
	return userDirectory(runtime.GOOS, home, os.Getenv), nil
}

func userDirectory(platform, home string, env func(string) string) string {
	base := func(key, fallback string) string {
		if p := env(key); filepath.IsAbs(p) {
			return p
		}
		return fallback
	}
	switch platform {
	case "windows":
		return filepath.Join(base("LOCALAPPDATA", filepath.Join(home, "AppData", "Local")), "control", "runbooks")
	case "darwin":
		return filepath.Join(home, "Library", "Application Support", "control", "runbooks")
	default:
		return filepath.Join(base("XDG_STATE_HOME", filepath.Join(home, ".local", "state")), "control", "runbooks")
	}
}

func CommonDirectory() string {
	switch runtime.GOOS {
	case "windows":
		base := os.Getenv("PROGRAMDATA")
		if !filepath.IsAbs(base) {
			base = `C:\ProgramData`
		}
		return filepath.Join(base, "control", "runbooks")
	case "darwin":
		return "/Library/Application Support/control/runbooks"
	default:
		return "/var/lib/control/runbooks"
	}
}

func LocalRoots() []Root {
	return []Root{{Source: "project", Path: ProjectDirectory}, userRoot("global")}
}

func WorkerRoots() []Root {
	return []Root{userRoot("worker-user"), {Source: "worker-common", Path: CommonDirectory()}}
}

func userRoot(source string) Root {
	dir, err := UserDirectory()
	root := Root{Source: source, Path: dir}
	if err != nil {
		root.Error = err.Error()
	}
	return root
}
