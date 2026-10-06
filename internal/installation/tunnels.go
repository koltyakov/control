package installation

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
)

// StartTunnelService installs a separate user startup entry, never a fleet node.
func StartTunnelService(ctx context.Context, binary, dir string) error {
	var err error
	binary, err = filepath.Abs(binary)
	if err != nil {
		return err
	}
	dir, err = filepath.Abs(dir)
	if err != nil {
		return err
	}
	mode := os.Getenv("CONTROL_TUNNEL_SERVICE_MODE")
	if mode != "" && mode != "user" && mode != "process" {
		return errors.New("CONTROL_TUNNEL_SERVICE_MODE must be user or process")
	}
	if mode != "process" {
		return installTunnelService(ctx, binary, dir)
	}
	log, err := os.OpenFile(filepath.Join(dir, "service.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer func() { _ = log.Close() }()
	cmd := exec.Command(binary, "__tunnels", dir)
	cmd.Env = cleanEnvironment()
	cmd.Stdout, cmd.Stderr = log, log
	detach(cmd)
	if err = cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}
