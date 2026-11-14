//go:build !windows

package commandexec

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
)

func detachFixture(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}

func TestEscapedDescendantOutputIsBounded(t *testing.T) {
	path := filepath.Join(t.TempDir(), "listener")
	cmd := fixtureCommand(t.Context(), "escaped", path)
	_, err := Run(t.Context(), cmd)
	if !errors.Is(err, exec.ErrWaitDelay) {
		t.Fatalf("escaped open output did not produce a bounded incomplete-output error: %v", err)
	}
	address, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatal(readErr)
	}
	stopFixture(address)
}

func TestTerminationSignalsGroupOnlyOnce(t *testing.T) {
	for _, killErr := range []error{nil, syscall.EPERM} {
		t.Run(map[bool]string{true: "success", false: "error"}[killErr == nil], func(t *testing.T) {
			var calls atomic.Int32
			owner := &ownership{kill: func(pid int, signal syscall.Signal) error {
				if pid != -123 || signal != syscall.SIGKILL {
					t.Error("incorrect process group")
				}
				calls.Add(1)
				return killErr
			}}
			process := &os.Process{Pid: 123}
			var callers sync.WaitGroup
			for range 32 {
				callers.Go(func() {
					if err := owner.terminate(process); !errors.Is(err, killErr) {
						t.Errorf("termination error changed: %v", err)
					}
				})
			}
			callers.Wait()
			// Simulate CommandContext's late cancellation after Wait has reaped
			// the helper. No real process identity is signalled by this test.
			if err := owner.terminate(process); !errors.Is(err, killErr) || calls.Load() != 1 {
				t.Fatalf("late cancellation sent another group kill: calls=%d err=%v", calls.Load(), err)
			}
		})
	}
}
