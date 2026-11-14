//go:build !windows

package commandexec

import (
	"errors"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"syscall"
)

type ownership struct {
	once sync.Once
	err  error
	kill func(int, syscall.Signal) error
}

func prepare(cmd *exec.Cmd, status, gate *os.File) (*ownership, [2]string, error) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.ExtraFiles = []*os.File{status, gate}
	owner := &ownership{kill: syscall.Kill}
	cmd.Cancel = func() error { return owner.terminate(cmd.Process) }
	return owner, [2]string{"3", "4"}, nil
}

func (o *ownership) adopt(_ *os.Process) error { return nil }
func (o *ownership) close()                    {}

func (o *ownership) terminate(process *os.Process) error {
	// Cmd.Wait reaps the process before joining CommandContext's cancellation
	// watcher. Only the first caller may signal this group; all later callbacks
	// must be no-ops even if they arrive after reaping.
	o.once.Do(func() {
		o.err = o.kill(-process.Pid, syscall.SIGKILL)
		if errors.Is(o.err, syscall.ESRCH) {
			o.err = nil
		}
	})
	return o.err
}

func helperFiles(statusArg, gateArg string) (*os.File, *os.File, error) {
	statusFD, err := strconv.Atoi(statusArg)
	if err != nil || statusFD != 3 || gateArg != "4" {
		return nil, nil, errors.New("invalid command handles")
	}
	syscall.CloseOnExec(3)
	syscall.CloseOnExec(4)
	return os.NewFile(3, "command-status"), os.NewFile(4, "command-gate"), nil
}

func hideChild(_ *exec.Cmd) {}

func terminateHelper() {
	_ = syscall.Kill(-os.Getpid(), syscall.SIGKILL)
}
