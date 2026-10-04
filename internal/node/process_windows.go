//go:build windows

package node

import (
	"os/exec"
	"strconv"
)

func configureProcess(cmd *exec.Cmd) {
	cmd.Cancel = func() error {
		// taskkill terminates descendants as well as the immediate CLI process.
		if err := exec.Command("taskkill.exe", "/T", "/F", "/PID", strconv.Itoa(cmd.Process.Pid)).Run(); err != nil {
			return cmd.Process.Kill()
		}
		return nil
	}
}
