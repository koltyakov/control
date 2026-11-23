package node

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/koltyakov/control/internal/store"

	"github.com/shirou/gopsutil/v4/disk"
)

func artifactDataFile(name string) bool {
	return digestID.MatchString(name) || (strings.HasSuffix(name, ".partial") && digestID.MatchString(strings.TrimSuffix(name, ".partial"))) || strings.HasPrefix(name, ".import-")
}

// Persist removal intent before touching either file. Failed removals remain
// discoverable at restart; imports cannot mistake pending metadata for content.
func (n *Node) deleteArtifactLocked(a storedArtifact) error {
	if !a.Deleting {
		a.Deleting = true
		if err := store.Write(n.artifactPath(a.ID)+".json", a); err != nil {
			return err
		}
	}
	if err := n.removeArtifactData(n.artifactPath(a.ID)); err != nil {
		return err
	}
	return os.Remove(n.artifactPath(a.ID) + ".json")
}

func (n *Node) loadArtifactUsage() error {
	entries, err := os.ReadDir(filepath.Join(n.Config.DataDir, "artifacts"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !artifactDataFile(entry.Name()) || !entry.Type().IsRegular() {
			continue
		}
		// Import staging belongs to one process. Before this node serves,
		// any staging file was left by a crash and is never resumed, so
		// remove it rather than charge it against maxArtifactBytes.
		if strings.HasPrefix(entry.Name(), ".import-") {
			if err := os.Remove(filepath.Join(n.Config.DataDir, "artifacts", entry.Name())); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		n.artifactUsed += info.Size()
	}
	return nil
}

type artifactBudgetWriter struct {
	node    *Node
	writer  io.Writer
	written int64
	checked int64
}

func (w *artifactBudgetWriter) Write(p []byte) (int, error) {
	n := w.node
	// Check real free space at admission and each 1 MiB, outside the shared
	// budget lock so a slow statfs never stalls other transfers.
	if w.checked <= w.written {
		usage, err := disk.Usage(n.Config.DataDir)
		if err != nil {
			return 0, err
		}
		if usage.Free < n.Config.ArtifactReserveBytes+uint64(len(p)) {
			return 0, errors.New("artifact write would consume metadata disk reserve")
		}
		w.checked = w.written + 1<<20
	}
	// The byte accounting includes concurrent writers and partial files, not
	// just published blobs.
	n.artifactBudgetMu.Lock()
	if n.Config.MaxArtifactBytes > 0 && int64(len(p)) > n.Config.MaxArtifactBytes-n.artifactUsed {
		n.artifactBudgetMu.Unlock()
		return 0, errors.New("artifact storage budget exhausted; release unused artifacts or increase maxArtifactBytes")
	}
	n.artifactUsed += int64(len(p))
	n.artifactBudgetMu.Unlock()
	count, err := w.writer.Write(p)
	n.artifactBudgetMu.Lock()
	n.artifactUsed -= int64(len(p) - count)
	n.artifactBudgetMu.Unlock()
	w.written += int64(count)
	return count, err
}

func (n *Node) removeArtifactData(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("artifact data is not a regular file")
	}
	if err := os.Remove(path); err != nil {
		return err
	}
	n.artifactBudgetMu.Lock()
	n.artifactUsed -= info.Size()
	n.artifactBudgetMu.Unlock()
	return nil
}

// Retained task references protect outputs, results and inputs independently of
// owner holds. GC never follows links and serializes with same-ID transfers.
func (n *Node) artifactReferenced(id string) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	for _, t := range n.tasks {
		for _, a := range t.Artifacts {
			if a.ID == id {
				return true
			}
		}
		if t.ResultArtifact != nil && t.ResultArtifact.ID == id {
			return true
		}
		for _, input := range t.Spec.Inputs {
			if input.Artifact.ID == id {
				return true
			}
		}
	}
	return false
}

// referencedArtifacts returns every artifact ID a retained task references.
func (n *Node) referencedArtifacts() map[string]bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	ids := map[string]bool{}
	for _, t := range n.tasks {
		for _, a := range t.Artifacts {
			ids[a.ID] = true
		}
		if t.ResultArtifact != nil {
			ids[t.ResultArtifact.ID] = true
		}
		for _, input := range t.Spec.Inputs {
			ids[input.Artifact.ID] = true
		}
	}
	return ids
}

func (n *Node) pruneArtifacts(ctx context.Context, now time.Time) error {
	dir := filepath.Join(n.Config.DataDir, "artifacts")
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var failures []error
	retention := time.Duration(n.Config.ArtifactRetentionHours) * time.Hour
	expiredCount := 0
	// One snapshot of task references per pass screens every candidate. A
	// reference added later is caught by the exact check before deletion, and
	// one dropped later only defers deletion to the next pass.
	var referenced map[string]bool
	keep := func(id string) bool {
		if referenced == nil {
			referenced = n.referencedArtifacts()
		}
		return referenced[id] || n.artifactReferenced(id)
	}
	defer func() {
		if expiredCount > 0 {
			slog.Info("artifact retention removed expired artifacts", "count", expiredCount, "retentionHours", n.Config.ArtifactRetentionHours)
		}
	}()
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		name := entry.Name()
		if strings.HasSuffix(name, ".json") && digestID.MatchString(strings.TrimSuffix(name, ".json")) {
			id := strings.TrimSuffix(name, ".json")
			unlock, err := n.lockTransfer(ctx, id)
			if err != nil {
				return err
			}
			a, err := n.storedArtifact(id)
			// Age-based expiry is opt-in and only adds candidates; the same
			// reference, reader and grant checks protect content still in use.
			expired := retention > 0 && !a.Created.IsZero() && now.Sub(a.retainedSince()) >= retention
			if err == nil && (a.Deleting || expired) && !keep(id) && !n.artifactReading(id) && !n.artifactGranted(id) {
				err = n.deleteArtifactLocked(a)
				if err == nil && !a.Deleting {
					expiredCount++
				}
			}
			unlock()
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				failures = append(failures, err)
			}
			continue
		}
		if !artifactDataFile(name) || !entry.Type().IsRegular() {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			failures = append(failures, err)
			continue
		}
		if now.Sub(info.ModTime()) < time.Duration(n.Config.PartialRetentionHours)*time.Hour {
			continue
		}
		id := strings.TrimSuffix(name, ".partial")
		unlock, err := n.lockTransfer(ctx, id)
		if err != nil {
			return err
		}
		n.artifactBudgetMu.Lock()
		staging := n.artifactStaging[name]
		n.artifactBudgetMu.Unlock()
		metadataErr := os.ErrNotExist
		if !strings.HasPrefix(name, ".import-") {
			_, metadataErr = n.storedArtifact(id)
		}
		// A transfer may have refreshed a partial while GC waited for its lock.
		current, statErr := os.Lstat(filepath.Join(dir, name))
		old := statErr == nil && current.Mode().IsRegular() && now.Sub(current.ModTime()) >= time.Duration(n.Config.PartialRetentionHours)*time.Hour
		unreferenced := errors.Is(metadataErr, os.ErrNotExist) && !n.artifactReferenced(id)
		// A published blob can leave a redundant partial after an interrupted
		// import. References protect the blob, not this unused staging copy.
		redundant := false
		if strings.HasSuffix(name, ".partial") && metadataErr == nil {
			blob, err := os.Lstat(n.artifactPath(id))
			redundant = err == nil && blob.Mode().IsRegular()
		}
		if old && !staging && !n.artifactReading(id) && (unreferenced || redundant) {
			err = n.removeArtifactData(filepath.Join(dir, name))
			if err != nil {
				failures = append(failures, fmt.Errorf("cleanup artifact %s: %w", name, err))
			}
		}
		unlock()
	}
	return errors.Join(failures...)
}
