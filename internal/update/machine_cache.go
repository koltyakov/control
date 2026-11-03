package update

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// The cache contains untrusted bytes, never executable runtime selections. A
// single OS lock covers lookup, download, publication and pruning across users.
// Keep the lock inode permanently, even when pruning all cached binaries. Nodes
// only ever fetch the selected version, so one older entry covers a rollback.
const machineCacheEntries = 2

func downloadCached(ctx context.Context, dir, path string, asset Asset, fetch func() error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if Verify(path, asset) == nil {
		return nil
	}
	if dir == "" {
		var err error
		dir, err = machineCachePath()
		if err != nil {
			return fetch()
		}
	}
	var root *os.Root
	err := retryCachePermission(ctx, func() error {
		var err error
		root, err = openMachineCache(dir)
		return err
	})
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		slog.Debug("machine update cache unavailable", "error", err)
		return fetch()
	}
	defer func() { _ = root.Close() }()
	// An unavailable or hostile cache must not indefinitely prevent updates.
	wait, cancel := context.WithTimeout(ctx, 5*time.Minute)
	lock, err := lockMachineCache(wait, root)
	cancel()
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		slog.Debug("machine update cache lock unavailable", "error", err)
		return fetch()
	}
	defer func() { _ = lock.Close() }()
	if err := ctx.Err(); err != nil {
		return err
	}
	// Another updater using this same profile may have finished while we waited.
	if Verify(path, asset) == nil {
		return nil
	}
	name := asset.SHA256 + ".blob"
	if cached, err := openCacheFile(root, name); err == nil {
		// Hash the copy in private storage. Never verify a shared pathname and
		// then execute it, or link a writable shared inode into a profile.
		err = SaveBinary(cached, path, asset)
		_ = cached.Close()
		if err == nil {
			pruneMachineCache(ctx, root, name)
			return nil
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := fetch(); err != nil {
		return err
	}
	// Cache publication is best effort. The private, verified download remains
	// usable if disk permissions or capacity prevent sharing it.
	if err := publishMachineCache(root, name, path, asset); err != nil {
		slog.Debug("machine update cache publication failed", "error", err)
	}
	pruneMachineCache(ctx, root, name)
	return nil
}

func openMachineCache(path string) (*os.Root, error) {
	parent, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	defer func() { _ = parent.Close() }()
	name := filepath.Base(path)
	created, err := mkdirMachineCache(parent, name)
	if err != nil {
		return nil, err
	}
	root, err := parent.OpenRoot(name)
	if err != nil {
		return nil, err
	}
	// Check the opened directory, not just a pathname before opening it. The
	// parent Root also prevents a substituted symlink escaping that parent.
	entry, err := parent.Lstat(name)
	opened, statErr := root.Stat(".")
	if err != nil || statErr != nil || !entry.IsDir() || entry.Mode()&os.ModeSymlink != 0 || !os.SameFile(entry, opened) {
		_ = root.Close()
		return nil, errors.New("machine update cache is not a regular directory")
	}
	if created {
		if err := shareMachineCache(root); err != nil {
			_ = root.Close()
			return nil, err
		}
	}
	return root, nil
}

func openCacheFile(root *os.Root, name string) (*os.File, error) {
	f, err := root.OpenFile(name, cacheReadFlags(), 0)
	if err != nil {
		return nil, err
	}
	entry, err := root.Lstat(name)
	opened, statErr := f.Stat()
	if err != nil || statErr != nil || !entry.Mode().IsRegular() || !opened.Mode().IsRegular() || !os.SameFile(entry, opened) {
		_ = f.Close()
		return nil, errors.New("machine update cache entry is not a regular file")
	}
	return f, nil
}

func lockMachineCache(ctx context.Context, root *os.Root) (*os.File, error) {
	var f *os.File
	err := retryCachePermission(ctx, func() error {
		var err error
		f, err = root.OpenFile("download.lock", os.O_CREATE|os.O_EXCL|os.O_RDWR, 0644)
		if errors.Is(err, os.ErrExist) {
			f, err = openCacheFile(root, "download.lock")
		} else if err == nil {
			// Do not change a shared pathname's permissions or truncate an existing
			// lock file. The descriptor refers to our exclusively created inode.
			err = f.Chmod(0644)
			if err != nil {
				_ = f.Close()
			}
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		if err = ctx.Err(); err != nil {
			break
		}
		var held bool
		held, err = tryMachineCacheLock(f)
		if err != nil || held {
			if held {
				return f, nil
			}
			break
		}
		select {
		case <-ctx.Done():
		case <-ticker.C:
		}
	}
	_ = f.Close()
	return nil, err
}

// Unix creation respects umask until the creator grants cross-account access
// through its opened descriptor. Allow a concurrent account to finish that
// brief initialization instead of immediately downloading a second copy.
func retryCachePermission(ctx context.Context, action func() error) error {
	deadline := time.Now().Add(250 * time.Millisecond)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := action()
		if !errors.Is(err, os.ErrPermission) || time.Now().After(deadline) {
			return err
		}
		timer := time.NewTimer(25 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func publishMachineCache(root *os.Root, name, path string, asset Asset) error {
	source, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = source.Close() }()
	temp := ".download-" + rand.Text()
	f, err := root.OpenFile(temp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	defer func() { _ = root.Remove(temp) }()
	defer func() { _ = f.Close() }()
	hash := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, hash), io.LimitReader(source, asset.Size+1))
	if err != nil {
		return err
	}
	if n != asset.Size || hex.EncodeToString(hash.Sum(nil)) != asset.SHA256 {
		return errors.New("machine update cache checksum mismatch")
	}
	if err := f.Chmod(0644); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return root.Rename(temp, name)
}

// Retain the requested binary plus the newest other. Scan in bounded batches
// and keep only the retained candidates in memory. Never follow symlinks or
// remove lock files, directories, or unrelated entries in this writable cache.
func pruneMachineCache(ctx context.Context, root *os.Root, current string) {
	dir, err := root.Open(".")
	if err != nil {
		return
	}
	defer func() { _ = dir.Close() }()
	var keep []os.FileInfo
	for ctx.Err() == nil {
		entries, err := dir.ReadDir(128)
		for _, entry := range entries {
			name := entry.Name()
			if strings.HasPrefix(name, ".download-") && entry.Type().IsRegular() {
				// No other cache writer can own a temporary file under this lock.
				_ = root.Remove(name)
				continue
			}
			if name == current || len(name) != 69 || name[64:] != ".blob" || !ValidDigest(name[:64]) || !entry.Type().IsRegular() {
				continue
			}
			info, err := entry.Info()
			if err != nil || !info.Mode().IsRegular() {
				continue
			}
			keep = append(keep, info)
			if len(keep) < machineCacheEntries {
				continue
			}
			oldest := 0
			for i := range keep {
				if keep[i].ModTime().Before(keep[oldest].ModTime()) {
					oldest = i
				}
			}
			_ = root.Remove(keep[oldest].Name())
			keep = append(keep[:oldest], keep[oldest+1:]...)
		}
		if err != nil {
			return
		}
	}
}
