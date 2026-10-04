// Package installation manages per-user node configuration and background startup.
package installation

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"

	"github.com/koltyakov/control/internal/node"
	"github.com/koltyakov/control/internal/store"
)

type AdminProfile struct {
	Gateway string `json:"gateway"`
	Key     string `json:"key"`
}

func Home() string {
	if path := os.Getenv("CONTROL_HOME"); path != "" {
		return path
	}
	path, err := os.UserConfigDir()
	if err != nil {
		return ".control"
	}
	return filepath.Join(path, "control")
}
func ConfigPath() string {
	if path := os.Getenv("CONTROL_CONFIG"); path != "" {
		return path
	}
	return filepath.Join(Home(), "node.json")
}
func BinPath() (string, error) {
	dir := os.Getenv("CONTROL_INSTALL_DIR")
	if dir == "" {
		if runtime.GOOS == "windows" {
			dir = filepath.Join(os.Getenv("LOCALAPPDATA"), "Programs", "control")
		} else {
			home, err := os.UserHomeDir()
			if err != nil {
				return "", err
			}
			dir = filepath.Join(home, ".local", "bin")
		}
	}
	name := "control"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	return filepath.Abs(filepath.Join(dir, name))
}
func ReadConfig(path string) (node.Config, error) {
	cfg, err := node.LoadConfig(path)
	if errors.Is(err, os.ErrNotExist) {
		return node.Config{}, nil
	}
	return cfg, err
}
func ReadAdmin() (AdminProfile, error) {
	var p AdminProfile
	err := store.Read(filepath.Join(Home(), "admin.json"), &p)
	if errors.Is(err, os.ErrNotExist) {
		err = nil
	}
	return p, err
}
func SaveAdmin(p AdminProfile) error { return store.Write(filepath.Join(Home(), "admin.json"), p) }

func API(cfg node.Config) string {
	address := cfg.Listen
	if address == "" {
		address = "127.0.0.1:7331"
	}
	return "http://" + address
}
