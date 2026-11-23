package update

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/koltyakov/control/internal/store"
)

// prune runs on the updater's command loop, so its staged download cannot race
// cleanup. Runtime requests remain protected until the restart handoff consumes them.
func (a *Updater) prune(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	keep := map[string]bool{a.options.Software.SHA256: true}
	retainPath := func(path string) {
		relative, err := filepath.Rel(filepath.Join(a.options.Dir, "updates"), path)
		if err == nil {
			parts := strings.Split(relative, string(filepath.Separator))
			if len(parts) > 1 && ValidDigest(parts[0]) {
				keep[parts[0]] = true
			}
		}
	}
	if a.staged != "" {
		retainPath(a.staged)
	}
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	retainPath(executable)
	// Read every record before deleting anything. A failed startup keeps the
	// previous selection, even when runtime.json already points at the candidate.
	confirmed := false
	// Hash the running binary only when something would be deleted. Cleanup runs
	// every minute, and an idle node usually has nothing to remove.
	verifyCurrent := func() error { return nil }
	for _, name := range []string{"runtime.json", "runtime-request.json", "runtime-previous.json"} {
		var runtime struct {
			Path    string `json:"path"`
			Version string `json:"version"`
			Asset   Asset  `json:"asset"`
		}
		if err := store.Read(filepath.Join(a.options.Dir, name), &runtime); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return fmt.Errorf("read %s for node update cleanup: %w", name, err)
		}
		if runtime.Path == "" || (runtime.Asset.SHA256 != "" && !ValidDigest(runtime.Asset.SHA256)) {
			return fmt.Errorf("invalid %s for node update cleanup", name)
		}
		if name == "runtime.json" {
			confirmed = runtime.Version == a.options.Software.Version && Matches(a.options.Software, runtime.Asset)
			if confirmed {
				path, asset := runtime.Path, runtime.Asset
				verifyCurrent = sync.OnceValue(func() error {
					if err := Verify(path, asset); err != nil {
						return fmt.Errorf("verify current runtime for node update cleanup: %w", err)
					}
					return nil
				})
			}
		}
		if name != "runtime-previous.json" || !confirmed {
			keep[runtime.Asset.SHA256] = true
			retainPath(runtime.Path)
		}
	}
	root, err := os.OpenRoot(a.options.Dir)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	if err := ctx.Err(); err != nil {
		return err
	}
	if confirmed {
		if _, err := root.Lstat("runtime-previous.json"); err == nil {
			if err := verifyCurrent(); err != nil {
				return err
			}
			if err := root.Remove("runtime-previous.json"); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	info, err := root.Lstat("updates")
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("node update storage is not a directory")
	}
	storage, err := root.OpenRoot("updates")
	if err != nil {
		return err
	}
	defer func() { _ = storage.Close() }()
	f, err := storage.Open(".")
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	var failures []error
	for {
		entries, readErr := f.ReadDir(128)
		for _, entry := range entries {
			if err := ctx.Err(); err != nil {
				return errors.Join(append(failures, err)...)
			}
			if ValidDigest(entry.Name()) && keep[entry.Name()] && entry.IsDir() && entry.Type()&os.ModeSymlink == 0 {
				// A retained version can still hold temporary files from an
				// interrupted download of its binary.
				if err := pruneUpdateTemps(storage, entry.Name(), time.Now().Add(-time.Hour)); err != nil {
					failures = append(failures, err)
				}
				continue
			}
			if !ValidDigest(entry.Name()) || keep[entry.Name()] || !entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
				continue
			}
			if err := verifyCurrent(); err != nil {
				return errors.Join(append(failures, err)...)
			}
			if err := storage.RemoveAll(entry.Name()); err != nil {
				failures = append(failures, err)
			}
		}
		if readErr != nil {
			if !errors.Is(readErr, io.EOF) {
				failures = append(failures, readErr)
			}
			return errors.Join(failures...)
		}
	}
}

// pruneUpdateTemps removes interrupted download temporaries in dir that have
// not been written since cutoff. Active downloads keep a fresh modification
// time. Symlinks and other entries are never followed or removed.
func pruneUpdateTemps(root *os.Root, dir string, cutoff time.Time) error {
	f, err := root.Open(dir)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	entries, err := f.ReadDir(64)
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	var failures []error
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), ".update-") || !entry.Type().IsRegular() {
			continue
		}
		if info, err := entry.Info(); err == nil && info.ModTime().Before(cutoff) {
			if err := root.Remove(filepath.Join(dir, entry.Name())); err != nil && !errors.Is(err, os.ErrNotExist) {
				failures = append(failures, err)
			}
		}
	}
	return errors.Join(failures...)
}
