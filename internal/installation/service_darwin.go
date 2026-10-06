package installation

import (
	"context"
	"encoding/xml"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/koltyakov/control/internal/node"
)

func detach(cmd *exec.Cmd)         { cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true} }
func addUserPath(dir string) error { return nil }
func finishUninstall(ctx context.Context) error {
	target := fmt.Sprintf("gui/%d/com.koltyakov.control", os.Getuid())
	if exec.CommandContext(ctx, "launchctl", "print", target).Run() != nil {
		return nil
	}
	return run(ctx, "launchctl", "bootout", target)
}
func servicePath() (string, error) {
	home, err := os.UserHomeDir()
	return filepath.Join(home, "Library", "LaunchAgents", "com.koltyakov.control.plist"), err
}
func xmlText(s string) string {
	var b strings.Builder
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}
func installService(ctx context.Context, binary, config string) error {
	path, err := servicePath()
	if err != nil {
		return err
	}
	log := xmlText(filepath.Join(filepath.Dir(config), "node.log"))
	content := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!-- Managed by control -->
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict><key>Label</key><string>com.koltyakov.control</string>
<key>ProgramArguments</key><array><string>%s</string><string>node</string><string>--config</string><string>%s</string></array>
<key>EnvironmentVariables</key><dict><key>CONTROL_TOKEN</key><string></string></dict>
<key>RunAtLoad</key><true/><key>KeepAlive</key><dict><key>SuccessfulExit</key><false/></dict>
<key>StandardOutPath</key><string>%s</string><key>StandardErrorPath</key><string>%s</string></dict></plist>
`, xmlText(binary), xmlText(config), log, log)
	if err = writeManaged(path, []byte(content)); err != nil {
		return err
	}
	domain := fmt.Sprintf("gui/%d", os.Getuid())
	if exec.CommandContext(ctx, "launchctl", "print", domain+"/com.koltyakov.control").Run() == nil {
		return run(ctx, "launchctl", "kickstart", domain+"/com.koltyakov.control")
	}
	return run(ctx, "launchctl", "bootstrap", domain, path)
}
func uninstallService(ctx context.Context, config string) error {
	path, err := servicePath()
	if err != nil {
		return err
	}
	if _, err = os.Stat(path); os.IsNotExist(err) {
		return nil
	}
	// Remove the login entry first; the local API stops the process gracefully.
	if err = os.Remove(path); err != nil {
		return err
	}
	return nil
}

func systemLaunchPlist(binary, config string) string {
	label := "com.koltyakov.control." + systemServiceID(config)
	log := xmlText(filepath.Join(filepath.Dir(config), "node.log"))
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!-- Managed by control -->
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict><key>Label</key><string>%s</string>
<key>ProgramArguments</key><array><string>%s</string><string>node</string><string>--config</string><string>%s</string></array>
<key>EnvironmentVariables</key><dict><key>CONTROL_TOKEN</key><string></string></dict>
<key>RunAtLoad</key><true/><key>KeepAlive</key><dict><key>SuccessfulExit</key><false/></dict>
<key>StandardOutPath</key><string>%s</string><key>StandardErrorPath</key><string>%s</string></dict></plist>
`, label, xmlText(binary), xmlText(config), log, log)
}

func systemService(ctx context.Context, operation, binary, config string, _ node.Config) error {
	label := "com.koltyakov.control." + systemServiceID(config)
	path := filepath.Join("/Library/LaunchDaemons", label+".plist")
	target := "system/" + label
	if operation == "start" {
		if err := writeManaged(path, []byte(systemLaunchPlist(binary, config))); err != nil {
			return err
		}
		if exec.CommandContext(ctx, "launchctl", "print", target).Run() == nil {
			return run(ctx, "launchctl", "kickstart", target)
		}
		return run(ctx, "launchctl", "bootstrap", "system", path)
	}
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return err
	}
	if exec.CommandContext(ctx, "launchctl", "print", target).Run() == nil {
		if err := run(ctx, "launchctl", "bootout", target); err != nil {
			return err
		}
	}
	if operation == "uninstall" {
		return os.Remove(path)
	}
	return nil
}
