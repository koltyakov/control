package commandexec

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

func detachFixture(_ *exec.Cmd) {}

func TestRunCommandNestedJob(t *testing.T) {
	// Simulate an enclosing Task Scheduler/service job without changing the
	// test process's job membership or any real service registrations.
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	owner := &ownership{job: job}
	defer owner.close()
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation, uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		t.Fatal(err)
	}
	gate, release, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = gate.Close(); _ = release.Close() }()
	cmd := fixtureCommand(t.Context(), "gated-runner", filepath.Join(t.TempDir(), "listener"))
	var stderr bytes.Buffer
	cmd.Stdin, cmd.Stderr = gate, &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if err := owner.adopt(cmd.Process); err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatal(err)
	}
	if _, err := release.Write([]byte{1}); err != nil {
		_ = owner.terminate(cmd.Process)
		_ = cmd.Wait()
		t.Fatal(err)
	}
	_ = release.Close()
	if err := cmd.Wait(); err != nil {
		t.Fatalf("command in enclosing job: %v, %s", err, stderr.String())
	}
}
