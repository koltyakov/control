package installation

import (
	"context"
	"crypto/ed25519"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/gofrs/flock"
	"github.com/koltyakov/control/internal/buildinfo"
	"github.com/koltyakov/control/internal/identity"
	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/node"
	"github.com/koltyakov/control/internal/store"
)

func startupInstallation(config, id string) (installedNode, node.Config, error) {
	config, err := filepath.Abs(config)
	if err != nil {
		return installedNode{}, node.Config{}, err
	}
	cfg, err := node.LoadConfig(config)
	if err != nil {
		return installedNode{}, cfg, err
	}
	var p installedNode
	if err = store.Read(installationRecord(cfg), &p); err != nil {
		return p, cfg, errors.New("no installed startup profile; run the command locally")
	}
	key, err := os.ReadFile(filepath.Join(cfg.DataDir, "identity.key"))
	if err != nil {
		return p, cfg, err
	}
	if len(key) != ed25519.PrivateKeySize || p.Config != config || p.ID != id || identity.ID(ed25519.PrivateKey(key).Public().(ed25519.PublicKey)) != id || !filepath.IsAbs(p.Binary) || cfg.HybridSystem {
		return p, cfg, errors.New("the running identity does not match its installed User profile")
	}
	var state model.MachineState
	if err := store.Read(filepath.Join(cfg.DataDir, "machine-state.json"), &state); err != nil && !os.IsNotExist(err) {
		return p, cfg, err
	}
	if state.Unregistered || state.Disabled {
		return p, cfg, errors.New("the installed machine is disabled or retired")
	}
	return p, cfg, nil
}

// PrepareStartupChange never prompts for elevation. Only Windows supports an
// identity-preserving System migration. Hybrid and Unix need local installers.
func PrepareStartupChange(ctx context.Context, config, id, mode string) error {
	if mode == "hybrid" {
		return errors.New("Hybrid needs a protected companion installation under the intended user; run its installer locally")
	}
	if mode != "system" || runtime.GOOS != "windows" {
		return errors.New("Unix System startup needs a separate root-owned profile; run its installer locally")
	}
	if err := CheckServiceMode("auto"); err != nil {
		return err
	}
	p, cfg, err := startupInstallation(config, id)
	if err != nil {
		return err
	}
	if p.Mode != "user" {
		return errors.New("only an installed User startup profile can be migrated remotely")
	}
	source, err := os.Executable()
	if err != nil {
		return err
	}
	sha := buildinfo.Current().SHA256
	if len(sha) != 64 {
		return errors.New("cannot verify the startup migration helper")
	}
	helper := filepath.Join(cfg.DataDir, "startup-"+sha, "control.exe")
	if err = ctx.Err(); err != nil {
		return err
	}
	if err = copyUninstallHelper(source, helper, sha); err != nil {
		return err
	}
	return launchDetachedUninstall(helper, []string{"__startup-system", p.Config, cfg.DataDir, id})
}

// ApplyStartupChange runs only in the detached helper after the old supervisor
// exits. It validates the profile again and retains all identity and work files.
func ApplyStartupChange(ctx context.Context, config, dataDir, id string) error {
	if runtime.GOOS != "windows" {
		return errors.New("remote System migration is Windows-only")
	}
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	installLock := flock.New(config + ".install.lock")
	defer func() { _ = installLock.Close() }()
	if held, err := installLock.TryLockContext(ctx, 100*time.Millisecond); err != nil || !held {
		return errors.New("another installation is changing this profile")
	}
	lock := flock.New(filepath.Join(dataDir, "runtime.lock"))
	defer func() { _ = lock.Close() }()
	held, err := lock.TryLockContext(ctx, 100*time.Millisecond)
	if err != nil || !held {
		return errors.New("the previous supervisor did not stop; inspect the machine before recovery")
	}
	p, cfg, err := startupInstallation(config, id)
	if err != nil {
		return err
	}
	if cfg.DataDir != dataDir || p.Mode != "user" {
		return errors.New("the startup profile changed before migration")
	}
	// Service starts the new supervisor, which needs this same lock.
	if err = lock.Unlock(); err != nil {
		return err
	}
	return Service(ctx, "start", p.Binary, p.Config, "auto")
}
