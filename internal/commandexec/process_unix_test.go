//go:build !windows

package commandexec

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func detachFixture(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}

func selfCPU() time.Duration {
	var usage syscall.Rusage
	if syscall.Getrusage(syscall.RUSAGE_SELF, &usage) != nil {
		return 0
	}
	return time.Duration(usage.Utime.Nano() + usage.Stime.Nano())
}

// A program that never started has no rusage on Unix: only wall time is
// known, and CPU and memory are omitted rather than reported as zero.
func TestRunWithUsageOmitsUnknownReadings(t *testing.T) {
	missing := exec.CommandContext(t.Context(), filepath.Join(t.TempDir(), "absent-command"))
	exit, usage, err := RunWithUsage(t.Context(), missing)
	if exit != -1 || err == nil || usage == nil || usage.WallMs == nil {
		t.Fatalf("exit=%d err=%v usage=%+v", exit, err, usage)
	}
	if encoded, _ := json.Marshal(usage); !strings.HasPrefix(string(encoded), `{"wallMs":`) || strings.Count(string(encoded), ":") != 1 {
		t.Fatalf("unknown readings were reported: %s", encoded)
	}
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
