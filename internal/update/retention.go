package update

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/koltyakov/control/internal/store"
)

// Unpublished uploads have time to finish a multi-platform push before pruning.
const uploadRetention = time.Hour

type PruneResult struct {
	Blobs  int
	Staged int
}

// Prune removes obsolete gateway binaries only after durable rollout completion.
// Callers must serialize invitation creation and deployment selection with it.
func (r *Repository) Prune(ctx context.Context, id string, retained []string) (PruneResult, error) {
	r.mu.Lock()
	locked := true
	defer func() {
		if locked {
			r.mu.Unlock()
		}
	}()
	var result PruneResult
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if r.current == nil || r.current.ID != id || r.current.Phase != "complete" {
		return result, errors.New("update is not complete or was superseded")
	}
	keep := map[string]bool{}
	for _, hash := range retained {
		keep[hash] = true
	}
	for _, asset := range r.current.Manifest.Assets {
		keep[asset.SHA256] = true
	}
	for hash := range r.pins {
		keep[hash] = true
	}
	// Fail closed on unreadable runtime records before removing any files.
	for _, name := range []string{"runtime.json", "runtime-previous.json", "runtime-request.json"} {
		var runtime struct {
			Path  string `json:"path"`
			Asset Asset  `json:"asset"`
		}
		if err := store.Read(filepath.Join(filepath.Dir(r.dir), name), &runtime); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return result, fmt.Errorf("read %s for update cleanup: %w", name, err)
		}
		if runtime.Path == "" || (runtime.Asset.SHA256 != "" && !ValidDigest(runtime.Asset.SHA256)) {
			return result, fmt.Errorf("invalid %s for update cleanup", name)
		}
		keep[runtime.Asset.SHA256] = true
		r.retainPath(keep, runtime.Path)
	}
	executable, err := os.Executable()
	if err != nil {
		return result, err
	}
	r.retainPath(keep, executable)
	// Scan and remove without holding r.mu, so Pin and Current callers do not
	// wait on directory I/O. Each removal rechecks, under r.mu, that this
	// rollout is still current and the hash was not pinned meanwhile.
	r.mu.Unlock()
	locked = false
	remove := func(hash string, del func() error) (bool, error) {
		r.mu.Lock()
		defer r.mu.Unlock()
		if r.current == nil || r.current.ID != id || r.pins[hash] != 0 {
			return false, nil
		}
		return true, del()
	}
	root, err := os.OpenRoot(r.dir)
	if err != nil {
		return result, err
	}
	defer func() { _ = root.Close() }()
	var failures []error
	cutoff := time.Now().Add(-uploadRetention)
	for _, directory := range []string{"blobs", "staged"} {
		f, err := root.Open(directory)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return result, errors.Join(append(failures, err)...)
		}
		for {
			entries, readErr := f.ReadDir(128)
			for _, entry := range entries {
				if err := ctx.Err(); err != nil {
					_ = f.Close()
					return result, errors.Join(append(failures, err)...)
				}
				if directory == "blobs" && strings.HasPrefix(entry.Name(), ".update-") && entry.Type().IsRegular() {
					// An interrupted download's temporary file; active ones
					// keep a fresh modification time.
					if info, err := entry.Info(); err == nil && info.ModTime().Before(cutoff) {
						if err := root.Remove(filepath.Join(directory, entry.Name())); err != nil && !errors.Is(err, os.ErrNotExist) {
							failures = append(failures, err)
						}
					}
					continue
				}
				if !ValidDigest(entry.Name()) || keep[entry.Name()] || entry.Type()&os.ModeSymlink != 0 {
					continue
				}
				path := filepath.Join(directory, entry.Name())
				if directory == "blobs" {
					info, err := entry.Info()
					if err != nil {
						failures = append(failures, err)
						continue
					}
					if !info.Mode().IsRegular() || !info.ModTime().Before(cutoff) {
						continue
					}
					removed, err := remove(entry.Name(), func() error { return root.Remove(path) })
					if err != nil {
						failures = append(failures, err)
					} else if removed {
						result.Blobs++
					}
				} else if entry.IsDir() {
					removed, err := remove(entry.Name(), func() error { return root.RemoveAll(path) })
					if err != nil {
						failures = append(failures, err)
					} else if removed {
						result.Staged++
					}
				}
			}
			if readErr != nil {
				_ = f.Close()
				if !errors.Is(readErr, io.EOF) {
					failures = append(failures, readErr)
				}
				break
			}
		}
	}
	return result, errors.Join(failures...)
}

func (r *Repository) retainPath(keep map[string]bool, path string) {
	relative, err := filepath.Rel(filepath.Join(r.dir, "staged"), path)
	if err != nil {
		return
	}
	parts := strings.Split(relative, string(filepath.Separator))
	if len(parts) > 1 && ValidDigest(parts[0]) {
		keep[parts[0]] = true
	}
}
