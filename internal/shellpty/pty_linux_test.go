package shellpty

import (
	"os"
	"regexp"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestExitKillsRemainingSessionProcesses(t *testing.T) {
	fd, err := unix.PidfdOpen(os.Getpid(), 0)
	if err != nil {
		t.Skipf("pidfds unavailable: %v", err)
	}
	err = unix.PidfdSendSignal(fd, 0, nil, 0)
	_ = unix.Close(fd)
	if err != nil {
		t.Skipf("pidfd signalling unavailable: %v", err)
	}
	tty := startBash(t, t.Context(), 80, 24, "", "")
	out := record(tty)
	// nohup ignores the terminal hangup; disown keeps this test from
	// depending on Bash's own job cleanup.
	write(t, tty, "set +H\n")
	write(t, tty, "nohup sleep 10 >/dev/null 2>&1 & echo PID:$!; disown; exit 0\n")
	match := regexp.MustCompile(`PID:(\d+)`).FindStringSubmatch(out.eof(t))
	if match == nil {
		t.Fatal("no background PID")
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		// Containers without an init process can leave a killed orphan
		// unreaped. A zombie has already stopped executing.
		state, _, ok := processStat(match[1])
		if !ok || state == 'Z' || state == 'X' {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("background process %s is still running", match[1])
		}
		time.Sleep(20 * time.Millisecond)
	}
	if code, err := waitResult(t, tty); code != 0 || err != nil {
		t.Fatalf("Wait = %d, %v; want 0, nil", code, err)
	}
}
