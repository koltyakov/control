package installation

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

func tunnelLaunchPlist(binary, dir string) string {
	label := "com.koltyakov.control.tunnels." + profileName(dir)
	log := xmlText(filepath.Join(dir, "service.log"))
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!-- Managed by control -->
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict><key>Label</key><string>%s</string>
<key>ProgramArguments</key><array><string>%s</string><string>__tunnels</string><string>%s</string></array>
<key>RunAtLoad</key><true/><key>KeepAlive</key><true/>
<key>ThrottleInterval</key><integer>5</integer>
<key>StandardOutPath</key><string>%s</string><key>StandardErrorPath</key><string>%s</string></dict></plist>
`, label, xmlText(binary), xmlText(dir), log, log)
}

func installTunnelService(ctx context.Context, binary, dir string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	label := "com.koltyakov.control.tunnels." + profileName(dir)
	path := filepath.Join(home, "Library", "LaunchAgents", label+".plist")
	if err = writeManaged(path, []byte(tunnelLaunchPlist(binary, dir))); err != nil {
		return err
	}
	domain := fmt.Sprintf("gui/%d", os.Getuid())
	if exec.CommandContext(ctx, "launchctl", "print", domain+"/"+label).Run() == nil {
		return run(ctx, "launchctl", "kickstart", domain+"/"+label)
	}
	return run(ctx, "launchctl", "bootstrap", domain, path)
}
