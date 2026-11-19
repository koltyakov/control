package installation

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/gofrs/flock"
	"github.com/koltyakov/control/internal/node"
	"github.com/koltyakov/control/internal/store"
)

const (
	userSystemdService    = "control-node.service"
	userSystemdUnitFormat = "# Managed by control\n[Unit]\nDescription=Control peer node\nAfter=network-online.target\n[Service]\nExecStart=%s node --config %s\nEnvironment=CONTROL_TOKEN=\nRestart=on-failure\nRestartSec=3\n[Install]\nWantedBy=default.target\n"
)

func detach(cmd *exec.Cmd)                      { cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true} }
func addUserPath(dir string) error              { return nil }
func finishUninstall(ctx context.Context) error { return nil }
func servicePath() (string, error) {
	base, err := os.UserConfigDir()
	return filepath.Join(base, "systemd", "user", userSystemdService), err
}
func systemdQuote(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`, `%`, `%%`, `$`, `$$`).Replace(s) + `"`
}
func checkUserStartup(ctx context.Context) error {
	if err := run(ctx, "systemctl", "--user", "show-environment"); err != nil {
		return fmt.Errorf("user startup needs this user's systemd manager; run the command from the user's own login, not su or sudo: %w", err)
	}
	return nil
}
func UserStartupWarning() string { return "" }
func installService(ctx context.Context, binary, config string) error {
	path, err := servicePath()
	if err != nil {
		return err
	}
	if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	lock, err := lockUserSystemdUnit(ctx, path)
	if err != nil {
		return err
	}
	defer func() { _ = lock.Close() }()
	if _, err = checkUserSystemdUnit(path, config); err != nil {
		return err
	}
	if err = checkUserStartup(ctx); err != nil {
		return err
	}
	if err = store.Bytes(path, []byte(userSystemdUnit(binary, config)), 0600); err != nil {
		return err
	}
	if err = run(ctx, "systemctl", "--user", "daemon-reload"); err != nil {
		return err
	}
	return run(ctx, "systemctl", "--user", "enable", "--now", userSystemdService)
}

func userSystemdUnit(binary, config string) string {
	return fmt.Sprintf(userSystemdUnitFormat, systemdQuote(binary), systemdQuote(config))
}

func checkUserSystemdUnit(path, config string) (bool, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	unit := string(data)
	if !strings.HasPrefix(unit, "# Managed by control\n") {
		return false, fmt.Errorf("existing service file is not managed by control: %s", path)
	}
	// The launcher may change during an explicit refresh. Verify the generated
	// unit and its exact config argument without requiring the old launcher path.
	_, command, _ := strings.Cut(unit, "\nExecStart=")
	line, _, _ := strings.Cut(command, "\n")
	quotedConfig := systemdQuote(config)
	quotedBinary, ok := strings.CutSuffix(line, " node --config "+quotedConfig)
	binary, unquoteErr := strconv.Unquote(quotedBinary)
	binary = strings.NewReplacer("%%", "%", "$$", "$").Replace(binary)
	if !ok || unquoteErr != nil || !filepath.IsAbs(binary) || unit != userSystemdUnit(binary, config) {
		return false, fmt.Errorf("startup registration belongs to another profile or has been customized: %s", path)
	}
	return true, nil
}

// Every profile under the login shares this unit, not just the same binary.
func lockUserSystemdUnit(ctx context.Context, path string) (*flock.Flock, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	lock := flock.New(path + ".lock")
	held, err := lock.TryLockContext(ctx, 100*time.Millisecond)
	if err != nil || !held {
		_ = lock.Close()
		if err == nil {
			return nil, fmt.Errorf("user startup registration is locked: %s", path)
		}
		return nil, fmt.Errorf("lock user startup registration: %w", err)
	}
	return lock, nil
}

func uninstallService(ctx context.Context, config string) error {
	path, err := servicePath()
	if err != nil {
		return err
	}
	lock, err := lockUserSystemdUnit(ctx, path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer func() { _ = lock.Close() }()
	if owned, err := checkUserSystemdUnit(path, config); err != nil || !owned {
		return err
	}
	if err = run(ctx, "systemctl", "--user", "disable", userSystemdService); err != nil {
		return err
	}
	if err = os.Remove(path); err != nil {
		return err
	}
	return run(ctx, "systemctl", "--user", "daemon-reload")
}

func systemdSystemUnit(binary, config string) string {
	return fmt.Sprintf("# Managed by control\n[Unit]\nDescription=Control peer node\nWants=network-online.target\nAfter=network-online.target\n[Service]\nExecStart=%s node --config %s\nEnvironment=CONTROL_TOKEN=\nRestart=on-failure\nRestartSec=3\nUMask=0077\n[Install]\nWantedBy=multi-user.target\n", systemdQuote(binary), systemdQuote(config))
}

// Private launchers under /usr/local/lib otherwise inherit lib_t and can leave
// systemd's child in init_t, which cannot connect to the gateway. Label only the
// launcher as an executable and let the installed SELinux policy choose its domain.
func labelSystemServiceBinary(ctx context.Context, binary, selinuxRoot string) error {
	if _, err := os.Stat(filepath.Join(selinuxRoot, "enforce")); os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return fmt.Errorf("check SELinux for system startup: %w", err)
	}
	if err := run(ctx, "chcon", "-t", "bin_t", "--", binary); err != nil {
		return fmt.Errorf("label system service executable for SELinux: %w", err)
	}
	return nil
}

func systemService(ctx context.Context, operation, binary, config string, _ node.Config) error {
	name := "control-node-" + systemServiceID(config) + ".service"
	path := filepath.Join("/etc/systemd/system", name)
	if operation == "start" {
		if err := labelSystemServiceBinary(ctx, binary, "/sys/fs/selinux"); err != nil {
			return err
		}
		if err := writeManaged(path, []byte(systemdSystemUnit(binary, config))); err != nil {
			return err
		}
		if err := run(ctx, "systemctl", "daemon-reload"); err != nil {
			return err
		}
		return run(ctx, "systemctl", "enable", "--now", name)
	}
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return err
	}
	if operation == "stop" {
		return run(ctx, "systemctl", "stop", name)
	}
	if err := run(ctx, "systemctl", "disable", "--now", name); err != nil {
		return err
	}
	if err := os.Remove(path); err != nil {
		return err
	}
	return run(ctx, "systemctl", "daemon-reload")
}
