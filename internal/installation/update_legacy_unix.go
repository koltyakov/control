//go:build !windows

package installation

import (
	"context"
	"crypto/sha256"
	gobuildinfo "debug/buildinfo"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/gofrs/flock"
	"github.com/koltyakov/control/internal/store"
	"github.com/koltyakov/control/internal/update"
	"golang.org/x/sys/unix"
)

// RefreshLegacyUnixStartup runs before the new child opens work admission. The old
// parent cannot exec at its child's request, but its original service manager can
// restart a refreshed entry point after propagating a deliberate child failure.
// Keep the job, its account, installation identity, and persisted selection intact.
func RefreshLegacyUnixStartup(ctx context.Context, profile UpdateStartup, directory string, selected update.RuntimeBinary) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	installLock := flock.New(profile.Config + ".install.lock")
	defer func() { _ = installLock.Close() }()
	if held, err := installLock.TryLock(); err != nil || !held {
		return errors.New("another installation is changing this profile")
	}
	registry, err := lockBinaryRegistry(ctx, profile.Binary)
	if err != nil {
		return err
	}
	defer func() { _ = registry.Close() }()
	cliLock := flock.New(profile.Binary + ".update.lock")
	defer func() { _ = cliLock.Close() }()
	if held, err := cliLock.TryLock(); err != nil || !held {
		return errors.New("another CLI update is changing the startup executable")
	}
	checkProfile := func() error {
		fresh, err := ReadUpdateStartup(profile.Config, directory)
		if err != nil || fresh != profile {
			return errors.New("installation identity or startup binding changed during migration")
		}
		var current update.RuntimeBinary
		if err := store.Read(filepath.Join(directory, "runtime.json"), &current); err != nil {
			return err
		}
		if current != selected {
			return errors.New("selected runtime changed during startup migration")
		}
		if _, err := os.Lstat(filepath.Join(directory, "runtime-request.json")); !errors.Is(err, os.ErrNotExist) {
			return errors.New("legacy supervisor has an unresolved child-restart request")
		}
		return ctx.Err()
	}
	if err = checkProfile(); err != nil {
		return err
	}
	checkParent, err := legacyUnixBinding(ctx, profile)
	if err != nil {
		return err
	}
	if err = update.ValidateExecutable(ctx, selected.Path, selected.Version, selected.Asset); err != nil {
		return err
	}
	return refreshLegacyUnixBinary(ctx, profile.Binary, selected, func() error {
		if err := checkProfile(); err != nil {
			return err
		}
		if err := checkParent(); err != nil {
			return err
		}
		metadata, err := gobuildinfo.ReadFile(profile.Binary)
		if err != nil || metadata.Path != "github.com/koltyakov/control/cmd/control" {
			return errors.New("startup executable is not an installed Control binary")
		}
		return ctx.Err()
	})
}

func refreshLegacyUnixBinary(ctx context.Context, binary string, selected update.RuntimeBinary, recheck func() error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	original, err := os.Lstat(binary)
	if err != nil {
		return err
	}
	owner, ok := original.Sys().(*syscall.Stat_t)
	if !ok || !original.Mode().IsRegular() || original.Size() <= 0 || original.Size() > update.MaxBinarySize || original.Mode().Perm()&0022 != 0 ||
		original.Mode()&(os.ModeSetuid|os.ModeSetgid) != 0 || owner.Uid != uint32(os.Geteuid()) {
		return errors.New("startup executable must be a non-shared-write regular file owned by this execution account")
	}
	file, err := os.Open(binary)
	if err != nil {
		return err
	}
	hash := sha256.New()
	_, err = io.Copy(hash, file)
	closeErr := file.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	previous := update.Asset{Size: original.Size(), SHA256: hex.EncodeToString(hash.Sum(nil))}
	dir, err := os.MkdirTemp(filepath.Dir(binary), ".control-startup-*")
	if err != nil {
		return err
	}
	staged := filepath.Join(dir, "control")
	defer func() { _ = os.Remove(staged); _ = os.Remove(dir) }()
	source, err := os.Open(selected.Path)
	if err != nil {
		return err
	}
	err = prepareLegacyUnixImage(ctx, binary, source, staged, selected.Asset)
	_ = source.Close()
	if err != nil {
		return err
	}
	if err = os.Chown(staged, int(owner.Uid), int(owner.Gid)); err != nil {
		return err
	}
	if err = os.Chmod(staged, original.Mode().Perm()); err != nil {
		return err
	}
	if err = copyStartupXattrs(binary, staged); err != nil {
		return err
	}
	stagedInfo, err := os.Stat(staged)
	if err != nil || stagedInfo.Mode().Perm() != original.Mode().Perm() {
		return errors.New("startup attributes changed executable permissions")
	}
	prepared, err := os.Open(staged)
	if err != nil {
		return err
	}
	err = prepared.Sync()
	closeErr = prepared.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err = recheck(); err != nil {
		return err
	}
	fresh, err := os.Lstat(binary)
	if err != nil || !os.SameFile(original, fresh) {
		return errors.New("startup executable changed during migration")
	}
	freshOwner, ok := fresh.Sys().(*syscall.Stat_t)
	if !ok || fresh.Mode() != original.Mode() || freshOwner.Uid != owner.Uid || freshOwner.Gid != owner.Gid {
		return errors.New("startup executable permissions changed during migration")
	}
	if err = update.Verify(binary, previous); err != nil {
		return err
	}
	backup := binary + ".before-single-process-" + previous.SHA256[:16]
	if err = os.Link(binary, backup); err != nil {
		if !errors.Is(err, os.ErrExist) || update.Verify(backup, previous) != nil {
			return fmt.Errorf("retain original startup executable: %w", err)
		}
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	if err = os.Rename(staged, binary); err != nil {
		return err
	}
	folder, err := os.Open(filepath.Dir(binary))
	if err != nil {
		return err
	}
	defer func() { _ = folder.Close() }()
	return folder.Sync()
}

// Preserve the original startup file's labels and ACLs, not the downloaded
// source's metadata. In particular, replacing a bin_t SELinux launcher with an
// inherited lib_t file would strand a successful byte update at the next boot.
func copyStartupXattrs(source, destination string) error {
	var names [64 << 10]byte
	n, err := unix.Listxattr(source, names[:])
	if errors.Is(err, unix.ENOTSUP) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, name := range strings.Split(string(names[:n]), "\x00") {
		if name == "" {
			continue
		}
		var value [64 << 10]byte
		n, err := unix.Getxattr(source, name, value[:])
		if err != nil {
			return err
		}
		var current [64 << 10]byte
		count, readErr := unix.Getxattr(destination, name, current[:])
		if readErr == nil && string(current[:count]) == string(value[:n]) {
			continue
		}
		if err = unix.Setxattr(destination, name, value[:n], 0); err != nil {
			return fmt.Errorf("preserve startup attribute %s: %w", name, err)
		}
	}
	return nil
}
