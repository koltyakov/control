package installation

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
)

func tunnelSystemdUnit(binary, dir string) string {
	return fmt.Sprintf("# Managed by control\n[Unit]\nDescription=Control persistent tunnels\nAfter=network-online.target\n[Service]\nExecStart=%s __tunnels %s\nRestart=always\nRestartSec=5\nUMask=0077\n[Install]\nWantedBy=default.target\n", systemdQuote(binary), systemdQuote(dir))
}

func installTunnelService(ctx context.Context, binary, dir string) error {
	if err := run(ctx, "systemctl", "--user", "show-environment"); err != nil {
		return err
	}
	base, err := os.UserConfigDir()
	if err != nil {
		return err
	}
	name := "control-tunnels-" + profileName(dir) + ".service"
	if err = writeManaged(filepath.Join(base, "systemd", "user", name), []byte(tunnelSystemdUnit(binary, dir))); err != nil {
		return err
	}
	if err = run(ctx, "systemctl", "--user", "daemon-reload"); err != nil {
		return err
	}
	return run(ctx, "systemctl", "--user", "enable", "--now", name)
}
