// Package processutil contains platform-specific subprocess settings.
package processutil

import (
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
)

// HideWindow prevents background console children from opening a desktop window.
func HideWindow(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.HideWindow = true
	cmd.SysProcAttr.CreationFlags |= windows.CREATE_NO_WINDOW
}
