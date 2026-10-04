package installation

import (
	"context"
	"os/exec"
	"strings"
	"syscall"

	"golang.org/x/sys/windows/registry"
)

func detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: 0x00000008 | 0x00000200, HideWindow: true}
}
func finishUninstall(ctx context.Context) error { return nil }
func addUserPath(dir string) error {
	key, _, err := registry.CreateKey(registry.CURRENT_USER, `Environment`, registry.QUERY_VALUE|registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer key.Close()
	value, kind, err := key.GetStringValue("Path")
	if err != nil && err != registry.ErrNotExist {
		return err
	}
	for _, part := range strings.Split(value, ";") {
		if strings.EqualFold(part, dir) {
			return nil
		}
	}
	if value != "" {
		value += ";"
	}
	value += dir
	if kind == registry.EXPAND_SZ {
		return key.SetExpandStringValue("Path", value)
	}
	return key.SetStringValue("Path", value)
}
func installService(ctx context.Context, binary, config string) error {
	key, _, err := registry.CreateKey(registry.CURRENT_USER, `Software\Microsoft\Windows\CurrentVersion\Run`, registry.SET_VALUE|registry.QUERY_VALUE)
	if err != nil {
		return err
	}
	defer key.Close()
	command := syscall.EscapeArg(binary) + " --token \"\" node --config " + syscall.EscapeArg(config)
	if err = key.SetStringValue("ControlNode", command); err != nil {
		return err
	}
	return startDetached(binary, config)
}
func uninstallService(ctx context.Context, config string) error {
	key, err := registry.OpenKey(registry.CURRENT_USER, `Software\Microsoft\Windows\CurrentVersion\Run`, registry.SET_VALUE)
	if err == registry.ErrNotExist {
		return nil
	}
	if err != nil {
		return err
	}
	defer key.Close()
	err = key.DeleteValue("ControlNode")
	if err == registry.ErrNotExist {
		return nil
	}
	return err
}
