//go:build linux || darwin

package update

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

func machineCachePath() (string, error) { return "/var/tmp/control-update-cache-v1", nil }

func mkdirMachineCache(parent *os.Root, name string) (bool, error) {
	err := parent.Mkdir(name, 0777)
	if errors.Is(err, os.ErrExist) {
		return false, nil
	}
	return err == nil, err
}

func shareMachineCache(root *os.Root) error {
	f, err := root.Open(".")
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	return f.Chmod(0777)
}

func cacheReadFlags() int { return os.O_RDONLY | unix.O_NOFOLLOW | unix.O_NONBLOCK }

func tryMachineCacheLock(f *os.File) (bool, error) {
	err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
		return false, nil
	}
	return err == nil, err
}
