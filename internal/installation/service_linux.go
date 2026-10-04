package installation

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
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
func installService(ctx context.Context, binary, config string) error {
	if err := run(ctx, "systemctl", "--user", "show-environment"); err != nil {
		return err
	}
	path, err := servicePath()
	if err != nil {
		return err
	}
	content := fmt.Sprintf("# Managed by control\n[Unit]\nDescription=Control peer node\nAfter=network-online.target\n[Service]\nExecStart=%s node --config %s\nEnvironment=CONTROL_TOKEN=\nRestart=on-failure\nRestartSec=3\n[Install]\nWantedBy=default.target\n", systemdQuote(binary), systemdQuote(config))
	if err = writeManaged(path, []byte(content)); err != nil {
		return err
	}
	if err = run(ctx, "systemctl", "--user", "daemon-reload"); err != nil {
		return err
	}
	return run(ctx, "systemctl", "--user", "enable", "--now", "control-node.service")
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
