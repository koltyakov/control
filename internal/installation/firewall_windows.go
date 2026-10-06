package installation

import (
	"context"
	"fmt"
	"path/filepath"
	"time"

	"github.com/koltyakov/control/internal/node"
	"golang.org/x/sys/windows"
)

func configureFirewall(ctx context.Context, binary, config string, cfg node.Config) error {
	port, err := gatewayPort(cfg.Gateway)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	if windows.GetCurrentProcessToken().IsElevated() {
		return userPowerShell(ctx, windowsFirewallScript(binary, config, WindowsRuntimePath(cfg.DataDir), port))
	}
	// Elevate only this operation, not enrollment or the user-login node. Pass
	// absolute paths so selecting a different admin account cannot select its
	// profile or Windows' unrelated System32\control.exe.
	args := `--config "` + filepath.Clean(config) + `" service firewall`
	script := fmt.Sprintf(`$p = Start-Process -FilePath %s -ArgumentList %s -Verb RunAs -Wait -PassThru
if ($p.ExitCode -ne 0) { throw ('Control firewall setup failed with exit code ' + $p.ExitCode) }`, powershellLiteral(binary), powershellLiteral(args))
	if err := userPowerShell(ctx, script); err != nil {
		return fmt.Errorf("configure Windows firewall; approve the UAC prompt or run the installed CLI's service firewall command in Administrator PowerShell: %w", err)
	}
	return nil
}
