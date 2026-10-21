package node

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/store"
)

const (
	retentionInterval = 10 * time.Minute
	// Tombstones keep pruned task IDs from being executed again by a retried
	// submission. They are small and outlive the full records by far.
	maxTombstones = 50000
	tombstoneTTL  = 180 * 24 * time.Hour
)

// taskTombstone is the durable remainder of a pruned task: enough to answer a
// reconciliation query and to reject or deduplicate a resubmitted ID.
type taskTombstone struct {
	ID             string    `json:"id"`
	Owner          string    `json:"owner"`
	Spec           string    `json:"spec"`
	Capability     string    `json:"capability"`
	State          string    `json:"state"`
	Error          string    `json:"error,omitempty"`
	Created        time.Time `json:"created"`
	Updated        time.Time `json:"updated"`
	Pruned         time.Time `json:"pruned"`
	CleanupPending bool      `json:"cleanupPending,omitempty"`
	Checksum       string    `json:"checksum,omitempty"`
}

func (t taskTombstone) checksum() string {
	t.Checksum = ""
	sum := sha256.Sum256(model.JSON(t))
	return hex.EncodeToString(sum[:])
}

func (t taskTombstone) task() model.Task {
	return model.Task{ID: t.ID, Owner: t.Owner, Spec: model.TaskSpec{ID: t.ID, Capability: t.Capability}, State: t.State, Error: t.Error, Created: t.Created, Updated: t.Updated, Pruned: true}
}

func (n *Node) tombstonePath() string {
	return filepath.Join(n.Config.DataDir, "tasks", "pruned.jsonl")
}

// loadTombstones tolerates a torn final line from an interrupted append. A
// task record that still exists takes precedence over its tombstone.
func (n *Node) loadTombstones() error {
	f, err := os.Open(n.tombstonePath())
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 1<<20)
	now, stale := time.Now(), false
	// Retain the delimiter so only an incomplete final append can be ignored.
	scanner.Split(func(data []byte, eof bool) (int, []byte, error) {
		if i := bytes.IndexByte(data, '\n'); i >= 0 {
			return i + 1, data[:i+1], nil
		}
		if eof && len(data) > 0 {
			return len(data), data, nil
		}
		return 0, nil, nil
	})
	line := 0
	for scanner.Scan() {
		line++
		raw := scanner.Bytes()
		var t taskTombstone
		if err := json.Unmarshal(raw, &t); err != nil {
			if bytes.HasSuffix(raw, []byte{'\n'}) {
				return fmt.Errorf("corrupt task tombstone at line %d; preserve the journal and recover it before starting: %w", line, err)
			}
			stale = true
			continue
		}
		if !safeID.MatchString(t.ID) || t.Owner == "" || !digestID.MatchString(t.Spec) || !t.task().Terminal() || t.Pruned.IsZero() || (t.Checksum != "" && t.Checksum != t.checksum()) {
			return fmt.Errorf("invalid task tombstone at line %d; journal was not modified", line)
		}
		if task := n.tasks[t.ID]; task != nil {
			if task.Owner != t.Owner || specDigest(task.Spec) != t.Spec {
				return fmt.Errorf("task %s conflicts with its tombstone", t.ID)
			}
			if t.CleanupPending {
				delete(n.tasks, t.ID)
			} else {
				stale = true
				continue
			}
		}
		if !t.CleanupPending && now.Sub(t.Pruned) > tombstoneTTL {
			stale = true
			continue
		}
		if prior, duplicate := n.tombstones[t.ID]; duplicate {
			if prior.Owner != t.Owner || prior.Spec != t.Spec {
				return fmt.Errorf("conflicting task tombstone at line %d", line)
			}
			stale = true
		}
		n.tombstones[t.ID] = t
		if !bytes.HasSuffix(raw, []byte{'\n'}) {
			stale = true
		}
	}
	if err = scanner.Err(); err != nil {
		return err
	}
	if stale || len(n.tombstones) > maxTombstones {
		return n.compactTombstones()
	}
	return nil
}

// compactTombstones rewrites the file with the newest retained entries.
// Caller serializes it with pruneMu or runs before Start.
func (n *Node) compactTombstones() error {
	n.mu.Lock()
	entries := make([]taskTombstone, 0, len(n.tombstones))
	for _, t := range n.tombstones {
		entries = append(entries, t)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Pruned.After(entries[j].Pruned) })
	kept := entries[:0]
	for _, t := range entries {
		if len(kept) < maxTombstones || t.CleanupPending {
			kept = append(kept, t)
		}
	}
	entries = kept
	n.mu.Unlock()
	var b bytes.Buffer
	for i := len(entries) - 1; i >= 0; i-- {
		entries[i].Checksum = entries[i].checksum()
		b.Write(model.JSON(entries[i]))
		b.WriteByte('\n')
	}
	if err := store.Bytes(n.tombstonePath(), b.Bytes(), 0600); err != nil {
		return err
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	n.tombstones = make(map[string]taskTombstone, len(entries))
	for _, t := range entries {
		n.tombstones[t.ID] = t
	}
	return nil
}

// pruneTasks removes terminal task records, logs, and workspaces beyond the
// configured age or count. Running and reserved tasks, and tasks still named
// by a live delegation, are kept. Artifacts are separate and never pruned here.
func (n *Node) pruneTasks(now time.Time) error {
	n.pruneMu.Lock()
	defer n.pruneMu.Unlock()
	if err := n.retryTaskCleanup(); err != nil {
		return err
	}
	retention := time.Duration(n.Config.TaskRetentionHours) * time.Hour
	protected := map[string]bool{}
	n.delegationMu.Lock()
	for _, s := range n.delegations {
		if id := s.grant.TaskID(); id != "" {
			protected[id] = true
		}
	}
	n.delegationMu.Unlock()
	n.mu.Lock()
	candidates := make([]*model.Task, 0, len(n.tasks))
	for id, task := range n.tasks {
		if task.Terminal() && n.cancels[id] == nil && n.accepting[id] == nil && !protected[id] {
			candidates = append(candidates, task)
		}
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].Updated.Before(candidates[j].Updated) })
	excess := 0
	if n.Config.MaxRetainedTasks > 0 {
		excess = len(candidates) - n.Config.MaxRetainedTasks
	}
	var victims []taskTombstone
	for i, task := range candidates {
		if i < excess || (retention > 0 && now.Sub(task.Updated) > retention) {
			victims = append(victims, taskTombstone{ID: task.ID, Owner: task.Owner, Spec: specDigest(task.Spec), Capability: task.Spec.Capability, State: task.State, Error: task.Error, Created: task.Created, Updated: task.Updated, Pruned: now.UTC(), CleanupPending: true})
		}
	}
	n.mu.Unlock()
	if len(victims) == 0 {
		return nil
	}
	// Make the tombstones durable before removing any record they replace.
	var b bytes.Buffer
	for i := range victims {
		if len(victims[i].Error) > 4096 {
			victims[i].Error = victims[i].Error[:4096] + " [truncated]"
		}
		victims[i].Checksum = victims[i].checksum()
		t := victims[i]
		b.Write(model.JSON(t))
		b.WriteByte('\n')
	}
	if err := store.Append(n.tombstonePath(), b.Bytes()); err != nil {
		return err
	}
	removed := make([]string, 0, len(victims))
	n.mu.Lock()
	for _, t := range victims {
		if task := n.tasks[t.ID]; task != nil && task.Terminal() {
			delete(n.tasks, t.ID)
			n.tombstones[t.ID] = t
			removed = append(removed, t.ID)
		}
	}
	if len(removed) > 0 {
		n.rebuildRecentTasksLocked()
	}
	n.mu.Unlock()
	slog.Debug("pruned task records", "count", len(removed))
	return n.retryTaskCleanup()
}

// A pending tombstone is also the durable cleanup intent. Keep it until every
// removal succeeds, including across restart and tombstone count compaction.
func (n *Node) retryTaskCleanup() error {
	n.mu.Lock()
	var pending []taskTombstone
	for _, t := range n.tombstones {
		if t.CleanupPending {
			pending = append(pending, t)
		}
	}
	n.mu.Unlock()
	var failures []error
	changed := false
	for _, t := range pending {
		var err error
		for _, path := range []string{n.taskPath(t.ID), n.logPath(t.ID)} {
			if e := os.Remove(path); e != nil && !errors.Is(e, os.ErrNotExist) {
				err = errors.Join(err, e)
			}
		}
		err = errors.Join(err, n.root.RemoveAll(filepath.Join("tasks", t.ID)))
		if err != nil {
			failures = append(failures, fmt.Errorf("cleanup task %s: %w", t.ID, err))
			continue
		}
		n.mu.Lock()
		t.CleanupPending = false
		t.Checksum = t.checksum()
		n.tombstones[t.ID] = t
		n.mu.Unlock()
		changed = true
	}
	if changed {
		if err := n.compactTombstones(); err != nil {
			// Keep retry intent in memory until the journal replacement commits.
			n.mu.Lock()
			for _, t := range pending {
				n.tombstones[t.ID] = t
			}
			n.mu.Unlock()
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

func (n *Node) runRetention(ctx context.Context) {
	ticker := time.NewTicker(retentionInterval)
	defer ticker.Stop()
	for {
		if err := n.pruneTasks(time.Now()); err != nil {
			slog.Warn("task retention", "error", err)
		}
		if err := n.pruneArtifacts(ctx, time.Now()); err != nil && ctx.Err() == nil {
			slog.Warn("artifact retention", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
