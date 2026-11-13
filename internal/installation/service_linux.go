package installation

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/koltyakov/control/internal/node"
)

func detach(cmd *exec.Cmd)                      { cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true} }
func addUserPath(dir string) error              { return nil }
func finishUninstall(ctx context.Context) error { return nil }
func servicePath() (string, error) {
	base, err := os.UserConfigDir()
	return filepath.Join(base, "systemd", "user", "control-node.service"), err
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
	if err := run(ctx, "systemctl", "--user", "show-environment"); err != nil {
		return err
	}
	path, err := servicePath()
	if err != nil {
		return err
	}
	if err = writeManaged(path, []byte(userSystemdUnit(binary, config))); err != nil {
		return err
	}
	if err = run(ctx, "systemctl", "--user", "daemon-reload"); err != nil {
		return err
	}
	return run(ctx, "systemctl", "--user", "enable", "--now", "control-node.service")
}

func userSystemdUnit(binary, config string) string {
	return fmt.Sprintf("# Managed by control\n[Unit]\nDescription=Control peer node\nAfter=network-online.target\n[Service]\nExecStart=%s node --config %s\nEnvironment=CONTROL_TOKEN=\nRestart=on-failure\nRestartSec=3\n[Install]\nWantedBy=default.target\n", systemdQuote(binary), systemdQuote(config))
}
func uninstallService(ctx context.Context, config string) error {
	path, err := servicePath()
	if err != nil {
		return err
	}
	if _, err = os.Stat(path); os.IsNotExist(err) {
		return nil
	}
	if err = run(ctx, "systemctl", "--user", "disable", "control-node.service"); err != nil {
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
