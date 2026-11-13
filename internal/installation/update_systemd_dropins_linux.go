package installation

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// Fedora's standard service-wide vendor override changes only stop-failure
// handling. Rejecting every drop-in strands otherwise standard installations.
// Accept only protected vendor files with this inert setting, never arbitrary
// overrides, execution hooks, account/environment changes, or unit-specific jobs.
func legacySystemdDropIns(value, scope string) (map[string][32]byte, error) {
	paths := strings.Fields(value)
	if len(paths) > 8 {
		return nil, errors.New("too many systemd drop-ins for legacy startup migration")
	}
	result := make(map[string][32]byte, len(paths))
	for _, path := range paths {
		if !legacySystemdVendorPath(path, scope) {
			return nil, errors.New("legacy migration does not support custom systemd drop-ins")
		}
		for directory := filepath.Dir(path); ; directory = filepath.Dir(directory) {
			info, err := os.Lstat(directory)
			if err != nil {
				return nil, err
			}
			owner, ok := info.Sys().(*syscall.Stat_t)
			if !ok || !info.IsDir() || owner.Uid != 0 || info.Mode().Perm()&0022 != 0 {
				return nil, errors.New("vendor systemd drop-in directory is not protected by root")
			}
			if directory == "/" {
				break
			}
		}
		data, err := readLegacySystemdDropIn(path)
		if err != nil {
			return nil, fmt.Errorf("validate vendor systemd drop-in: %w", err)
		}
		if !safeLegacySystemdDropIn(data) {
			return nil, errors.New("vendor systemd drop-in changes unsupported startup behavior")
		}
		if _, exists := result[path]; exists {
			return nil, errors.New("duplicate systemd drop-in in loaded startup")
		}
		result[path] = sha256.Sum256(data)
	}
	return result, nil
}

func legacySystemdVendorPath(path, scope string) bool {
	return (scope == "user" || scope == "system") && filepath.Clean(path) == path &&
		filepath.Dir(path) == filepath.Join("/usr/lib/systemd", scope, "service.d") && filepath.Ext(path) == ".conf"
}

func protectedSystemdDropIn(mode os.FileMode, uid uint32) bool {
	return mode.IsRegular() && mode.Perm()&0022 == 0 && uid == 0
}

func readLegacySystemdDropIn(path string) ([]byte, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	defer func() { _ = file.Close() }()
	before, err := file.Stat()
	if err != nil {
		return nil, err
	}
	owner, ok := before.Sys().(*syscall.Stat_t)
	if !ok || !protectedSystemdDropIn(before.Mode(), owner.Uid) || before.Size() > 16<<10 {
		return nil, errors.New("vendor systemd drop-in is not a bounded root-owned regular file")
	}
	data, err := io.ReadAll(io.LimitReader(file, (16<<10)+1))
	if err != nil {
		return nil, err
	}
	after, err := file.Stat()
	if err != nil {
		return nil, err
	}
	afterOwner, ok := after.Sys().(*syscall.Stat_t)
	if len(data) > 16<<10 || int64(len(data)) != before.Size() || after.Size() != before.Size() ||
		after.ModTime() != before.ModTime() || after.Mode() != before.Mode() || !ok || !protectedSystemdDropIn(after.Mode(), afterOwner.Uid) {
		return nil, errors.New("vendor systemd drop-in changed while being validated")
	}
	return data, nil
}

func safeLegacySystemdDropIn(data []byte) bool {
	service := false
	for _, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		if line == "[Service]" {
			service = true
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !service || !ok || strings.TrimSpace(key) != "TimeoutStopFailureMode" || strings.TrimSpace(value) != "abort" {
			return false
		}
	}
	return true
}
