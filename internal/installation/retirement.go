package installation

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"

	"github.com/gofrs/flock"
	"github.com/koltyakov/control/internal/buildinfo"
	"github.com/koltyakov/control/internal/identity"
	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/node"
	"github.com/koltyakov/control/internal/store"
	"github.com/koltyakov/control/internal/update"
)

type installedNode struct {
	Config string `json:"config"`
	ID     string `json:"id"`
	Binary string `json:"binary"`
	Mode   string `json:"mode"`
}

func installationRecord(cfg node.Config) string {
	return filepath.Join(cfg.DataDir, "installation.json")
}

func binaryRegistration(p installedNode) string {
	name := profileName(p.Config)
	if runtime.GOOS != "windows" {
		// Unix profiles differing only in case are separate installations.
		hash := sha256.Sum256([]byte(filepath.Clean(p.Config)))
		name = hex.EncodeToString(hash[:8])
	}
	return filepath.Join(p.Binary+".profiles", name+".json")
}

// Remember the launcher, not a versioned managed child. The per-binary registry
// prevents removing an executable still used by another installed profile.
func rememberInstallation(ctx context.Context, binary, config, mode string, cfg node.Config) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	lock, err := lockBinaryRegistry(ctx, binary)
	if err != nil {
		return err
	}
	defer func() { _ = lock.Close() }()
	id, err := identity.Load(cfg.DataDir)
	if err != nil {
		return err
	}
	p := installedNode{Config: config, ID: id.ID, Binary: binary, Mode: mode}
	if err = store.Write(binaryRegistration(p), p); err != nil {
		return err
	}
	return store.Write(installationRecord(cfg), p)
}

// ScheduleRetiredUninstall starts an independent cleanup process. It must outlive
// both the node and its supervisor, including their service-manager process group.
// Unmanaged node processes have no installation to remove.
func ScheduleRetiredUninstall(config, id string) error {
	config, err := filepath.Abs(config)
	if err != nil {
		return err
	}
	cfg, err := node.LoadConfig(config)
	if err != nil {
		return err
	}
	var p installedNode
	if err = store.Read(installationRecord(cfg), &p); os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return err
	}
	if p.Config != config || p.ID != id {
		return errors.New("retired installation no longer matches the node profile")
	}
	source, err := os.Executable()
	if err != nil {
		return err
	}
	sha := buildinfo.Current().SHA256
	if len(sha) != 64 {
		return errors.New("cannot identify the running uninstall executable")
	}
	// Reuse one verified helper per executable, including failed-cleanup retries.
	dir := filepath.Join(cfg.DataDir, "uninstall-"+sha)
	helper := filepath.Join(dir, "control")
	if runtime.GOOS == "windows" {
		helper += ".exe"
	}
	if err = copyUninstallHelper(source, helper, sha); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return launchRetiredUninstall(ctx, helper, []string{"__uninstall", config, cfg.DataDir, id}, p)
}

func copyUninstallHelper(source, dest, sha string) error {
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	stat, err := in.Stat()
	if err != nil {
		return err
	}
	asset := update.Asset{Size: stat.Size(), SHA256: sha}
	if update.Verify(dest, asset) == nil {
		return nil
	}
	return update.SaveBinary(in, dest, asset)
}

func launchDetachedUninstall(helper string, args []string) error {
	cmd := exec.Command(helper, args...)
	cmd.Env = cleanEnvironment()
	detach(cmd)
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}

// FinishRetiredHelper releases a one-shot service-manager registration after the
// helper has written its result. Linux transient units collect themselves.
func FinishRetiredHelper(id string) {
	helper, err := os.Executable()
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	finishRetiredHelper(ctx, helper, id)
}

// UninstallRetired removes only an installation whose identity has a durable
// unregistration policy. The installation lock serializes cleanup with enrollment;
// the runtime and node locks ensure work has stopped before deleting the launcher.
func UninstallRetired(ctx context.Context, config, dataDir, id string) error {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	paths := []string{config + ".install.lock", filepath.Join(dataDir, "runtime.lock"), filepath.Join(dataDir, "node.lock")}
	for _, path := range paths {
		lock := flock.New(path)
		defer func() { _ = lock.Close() }()
		held, err := lock.TryLockContext(ctx, 100*time.Millisecond)
		if err != nil {
			return err
		}
		if !held {
			return errors.New("retired node did not release its installation locks")
		}
	}
	cfg, err := node.LoadConfig(config)
	if err != nil {
		return err
	}
	if cfg.DataDir != dataDir {
		return nil // A new invitation has already replaced this profile.
	}
	var p installedNode
	if err = store.Read(installationRecord(cfg), &p); err != nil {
		return err
	}
	if p.Config != config || p.ID != id || !filepath.IsAbs(p.Binary) {
		return errors.New("retired installation record does not match")
	}
	if err = ValidateServiceMode(p.Mode); err != nil {
		return err
	}
	if err = requireRetiredIdentity(cfg, id); err != nil {
		return err
	}
	if err = removeRetiredStartup(ctx, p); err != nil {
		return fmt.Errorf("remove retired startup: %w", err)
	}
	registry, err := lockBinaryRegistry(ctx, p.Binary)
	if err != nil {
		return err
	}
	defer func() { _ = registry.Close() }()
	shared, err := binaryStillUsed(p)
	if err != nil {
		return err
	}
	if !shared {
		if err = removeInstalledBinary(ctx, p.Binary); err != nil {
			return err
		}
	}
	if err = os.Remove(binaryRegistration(p)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return finishRetiredUninstall(ctx, p)
}

func requireRetiredIdentity(cfg node.Config, id string) error {
	key, err := os.ReadFile(filepath.Join(cfg.DataDir, "identity.key"))
	if err != nil {
		return err
	}
	if len(key) != ed25519.PrivateKeySize {
		return errors.New("invalid retired identity key")
	}
	var state model.MachineState
	if err = store.Read(filepath.Join(cfg.DataDir, "machine-state.json"), &state); err != nil {
		return err
	}
	if identity.ID(ed25519.PrivateKey(key).Public().(ed25519.PublicKey)) != id || !state.Unregistered {
		return errors.New("node identity has not been unregistered")
	}
	return nil
}

func lockBinaryRegistry(ctx context.Context, binary string) (*flock.Flock, error) {
	dir := binary + ".profiles"
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	lock := flock.New(filepath.Join(dir, "registry.lock"))
	held, err := lock.TryLockContext(ctx, 100*time.Millisecond)
	if err != nil || !held {
		_ = lock.Close()
		if err == nil {
			err = errors.New("installed binary registry is locked")
		}
		return nil, err
	}
	return lock, nil
}

func binaryStillUsed(p installedNode) (bool, error) {
	entries, err := os.ReadDir(p.Binary + ".profiles")
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	for _, entry := range entries {
		if entry.Name() == "registry.lock" || entry.Name() == filepath.Base(binaryRegistration(p)) {
			continue
		}
		// Even stopped profiles may be restarted. Preserve their shared launcher.
		// Enumeration is sufficient and does not require access to another
		// Windows service SID's private registration file.
		return true, nil
	}
	return false, nil
}

func removeInstalledBinary(ctx context.Context, binary string) error {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		err := os.Remove(binary)
		if err == nil || os.IsNotExist(err) {
			return nil
		}
		if runtime.GOOS != "windows" {
			return err
		}
		// SCM and the login-task wrapper may briefly retain the launcher handle.
		select {
		case <-ctx.Done():
			return fmt.Errorf("remove retired executable: %w", err)
		case <-ticker.C:
		}
	}
}
