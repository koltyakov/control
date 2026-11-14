package commandexec

import (
	"errors"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"syscall"
	"unsafe"

	"github.com/koltyakov/control/internal/processutil"
	"golang.org/x/sys/windows"
)

type ownership struct {
	mu      sync.Mutex
	job     windows.Handle
	adopted bool
	stopped bool
}

func prepare(cmd *exec.Cmd, status, gate *os.File) (*ownership, [2]string, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, [2]string{}, err
	}
	owner := &ownership{job: job}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err = windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation, uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		owner.close()
		return nil, [2]string{}, err
	}
	for _, file := range []*os.File{status, gate} {
		if err = windows.SetHandleInformation(windows.Handle(file.Fd()), windows.HANDLE_FLAG_INHERIT, windows.HANDLE_FLAG_INHERIT); err != nil {
			owner.close()
			return nil, [2]string{}, err
		}
	}
	processutil.HideWindow(cmd)
	cmd.SysProcAttr.AdditionalInheritedHandles = []syscall.Handle{syscall.Handle(status.Fd()), syscall.Handle(gate.Fd())}
	cmd.Cancel = func() error { return owner.terminate(cmd.Process) }
	return owner, [2]string{strconv.FormatUint(uint64(status.Fd()), 10), strconv.FormatUint(uint64(gate.Fd()), 10)}, nil
}

func (o *ownership) adopt(process *os.Process) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.stopped {
		return errors.New("command cancelled before ownership")
	}
	handle, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(process.Pid))
	if err != nil {
		return err
	}
	defer func() { _ = windows.CloseHandle(handle) }()
	if err := windows.AssignProcessToJobObject(o.job, handle); err != nil {
		return err
	}
	o.adopted = true
	return nil
}

func (o *ownership) terminate(process *os.Process) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.stopped = true
	if o.adopted {
		return windows.TerminateJobObject(o.job, 1)
	}
	err := process.Kill()
	if errors.Is(err, os.ErrProcessDone) {
		return nil
	}
	return err
}

func (o *ownership) close() {
	_ = windows.CloseHandle(o.job)
}

func helperFiles(statusArg, gateArg string) (*os.File, *os.File, error) {
	statusHandle, err := strconv.ParseUint(statusArg, 10, 64)
	if err != nil || statusHandle == 0 {
		return nil, nil, errors.New("invalid command status handle")
	}
	gateHandle, err := strconv.ParseUint(gateArg, 10, 64)
	if err != nil || gateHandle == 0 || gateHandle == statusHandle {
		return nil, nil, errors.New("invalid command gate handle")
	}
	for _, handle := range []uint64{statusHandle, gateHandle} {
		if err := windows.SetHandleInformation(windows.Handle(handle), windows.HANDLE_FLAG_INHERIT, 0); err != nil {
			return nil, nil, err
		}
	}
	return os.NewFile(uintptr(statusHandle), "command-status"), os.NewFile(uintptr(gateHandle), "command-gate"), nil
}

func hideChild(cmd *exec.Cmd) { processutil.HideWindow(cmd) }

// The job's only handle belongs to the parent. Parent exit closes it and kills
// this helper and its descendants; the helper never inherits that handle.
func terminateHelper() {}
