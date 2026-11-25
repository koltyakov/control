package shellpty

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"strconv"
	"time"

	"golang.org/x/sys/unix"
)

const (
	getTermios = unix.TCGETS
	setTermios = unix.TCSETS
)

// openPTY returns a non-blocking master, which the runtime poller can
// interrupt on Close, and its blocking slave. Both are close-on-exec.
func openPTY() (*os.File, *os.File, error) {
	fd, err := unix.Open("/dev/ptmx", unix.O_RDWR|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("open pseudo-terminal: %w", err)
	}
	if err := unix.IoctlSetPointerInt(fd, unix.TIOCSPTLCK, 0); err != nil {
		_ = unix.Close(fd)
		return nil, nil, fmt.Errorf("unlock pseudo-terminal: %w", err)
	}
	slave, err := openPeer(fd)
	if err != nil {
		_ = unix.Close(fd)
		return nil, nil, err
	}
	if err := unix.SetNonblock(fd, true); err != nil {
		_ = unix.Close(fd)
		_ = slave.Close()
		return nil, nil, err
	}
	return os.NewFile(uintptr(fd), "/dev/ptmx"), slave, nil
}

// openPeer opens the slave from the master descriptor when the kernel
// supports TIOCGPTPEER (4.13+), avoiding a path lookup in /dev/pts.
func openPeer(master int) (*os.File, error) {
	flags := uintptr(unix.O_RDWR | unix.O_NOCTTY | unix.O_CLOEXEC)
	if fd, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(master), unix.TIOCGPTPEER, flags); errno == 0 {
		return os.NewFile(fd, "pts"), nil
	}
	n, err := unix.IoctlGetUint32(master, unix.TIOCGPTN)
	if err != nil {
		return nil, fmt.Errorf("pseudo-terminal number: %w", err)
	}
	path := "/dev/pts/" + strconv.FormatUint(uint64(n), 10)
	fd, err := unix.Open(path, unix.O_RDWR|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	return os.NewFile(uintptr(fd), path), nil
}

// waitAndCleanSession leaves the shell unreaped while signalling its session,
// so the kernel cannot reuse the session ID for an unrelated process.
func waitAndCleanSession(leader int) {
	var info unix.Siginfo
	for {
		err := unix.Waitid(unix.P_PID, leader, &info, unix.WEXITED|unix.WNOWAIT, nil)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return
		}
		break
	}
	// Rescan to catch children forked during an earlier pass, within a fixed
	// budget. An older kernel without pidfds safely falls back to PTY hangup.
	for range 50 {
		if !signalSession(leader) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func signalSession(leader int) bool {
	dir, err := os.Open("/proc")
	if err != nil {
		return false
	}
	defer func() { _ = dir.Close() }()
	signalled := false
	for {
		names, err := dir.Readdirnames(128)
		for _, name := range names {
			pid, parseErr := strconv.Atoi(name)
			if parseErr == nil && pid > 0 && inSession(name, leader) && signalSessionMember(pid, leader) {
				signalled = true
			}
		}
		if err != nil {
			return signalled
		}
	}
}

func signalSessionMember(pid, leader int) bool {
	fd, err := unix.PidfdOpen(pid, 0)
	if err != nil {
		return false
	}
	defer func() { _ = unix.Close(fd) }()
	// Use the kernel's own PID namespace to recheck membership. If the old
	// process went away before pidfd_open, this checks the new occupant.
	// Later reuse of the numeric PID cannot retarget the signal.
	session, err := unix.Getsid(pid)
	return err == nil && session == leader && unix.PidfdSendSignal(fd, unix.SIGKILL, nil, 0) == nil
}

func inSession(name string, leader int) bool {
	state, session, ok := processStat(name)
	return ok && state != 'Z' && state != 'X' && session == leader
}

func processStat(name string) (byte, int, bool) {
	stat, err := os.ReadFile("/proc/" + name + "/stat")
	if err != nil {
		return 0, 0, false
	}
	return parseStat(stat)
}

// parseStat reads the state and session fields of /proc/PID/stat.
// The command name may contain spaces and parentheses, so fields are counted
// from its last closing parenthesis.
func parseStat(stat []byte) (byte, int, bool) {
	end := bytes.LastIndexByte(stat, ')')
	if end < 0 {
		return 0, 0, false
	}
	// Fields after the name: state ppid pgrp session tty_nr ...
	fields := bytes.Fields(stat[end+1:])
	if len(fields) < 4 || len(fields[0]) != 1 {
		return 0, 0, false
	}
	session, err := strconv.Atoi(string(fields[3]))
	if err != nil {
		return 0, 0, false
	}
	return fields[0][0], session, true
}
