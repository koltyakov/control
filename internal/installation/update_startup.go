package installation

import (
	"crypto/ed25519"
	"errors"
	"os"
	"path/filepath"

	"github.com/koltyakov/control/internal/identity"
	"github.com/koltyakov/control/internal/node"
	"github.com/koltyakov/control/internal/store"
)

// UpdateStartup binds an update migration to one remembered installation.
type UpdateStartup struct{ Config, Binary, Mode, ID string }

func ReadUpdateStartup(config, directory string) (UpdateStartup, error) {
	config, err := filepath.Abs(config)
	if err != nil {
		return UpdateStartup{}, err
	}
	cfg, err := node.LoadConfig(config)
	if err != nil {
		return UpdateStartup{}, err
	}
	if cfg.DataDir != directory {
		return UpdateStartup{}, errors.New("startup migration profile has a different state directory")
	}
	var installed installedNode
	if err = store.Read(installationRecord(cfg), &installed); err != nil {
		return UpdateStartup{}, err
	}
	key, err := os.ReadFile(filepath.Join(directory, "identity.key"))
	if err != nil {
		return UpdateStartup{}, err
	}
	if len(key) != ed25519.PrivateKeySize {
		return UpdateStartup{}, errors.New("invalid installed identity")
	}
	id := identity.ID(ed25519.PrivateKey(key).Public().(ed25519.PublicKey))
	if installed.Config != config || installed.ID != id || !filepath.IsAbs(installed.Binary) {
		return UpdateStartup{}, errors.New("startup migration no longer matches the installed identity")
	}
	if installed.Mode != "auto" && installed.Mode != "system" && installed.Mode != "user" && installed.Mode != "process" {
		return UpdateStartup{}, errors.New("unsupported installed startup mode")
	}
	return UpdateStartup{Config: config, Binary: installed.Binary, Mode: installed.Mode, ID: installed.ID}, nil
}
