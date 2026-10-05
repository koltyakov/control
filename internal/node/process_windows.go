//go:build windows

package node

import (
	"os/exec"
	"strconv"

	"github.com/koltyakov/control/internal/processutil"
)

func configureProcess(cmd *exec.Cmd) {
	processutil.HideWindow(cmd)
	cmd.Cancel = func() error {
		// taskkill terminates descendants as well as the immediate CLI process.
		kill := exec.Command("taskkill.exe", "/T", "/F", "/PID", strconv.Itoa(cmd.Process.Pid))
		processutil.HideWindow(kill)
		if err := kill.Run(); err != nil {
			return cmd.Process.Kill()
		}
		return nil
	}
}
