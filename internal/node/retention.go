package node

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
	ID         string    `json:"id"`
	Owner      string    `json:"owner"`
	Spec       string    `json:"spec"`
	Capability string    `json:"capability"`
	State      string    `json:"state"`
	Error      string    `json:"error,omitempty"`
	Created    time.Time `json:"created"`
	Updated    time.Time `json:"updated"`
	Pruned     time.Time `json:"pruned"`
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
	for scanner.Scan() {
		var t taskTombstone
		if json.Unmarshal(scanner.Bytes(), &t) != nil || !safeID.MatchString(t.ID) {
			stale = true
			continue
		}
		if _, exists := n.tasks[t.ID]; exists || now.Sub(t.Pruned) > tombstoneTTL {
			stale = true
			continue
		}
		if _, duplicate := n.tombstones[t.ID]; duplicate {
			stale = true
		}
		n.tombstones[t.ID] = t
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
	if len(entries) > maxTombstones {
		for _, t := range entries[maxTombstones:] {
			delete(n.tombstones, t.ID)
		}
		entries = entries[:maxTombstones]
	}
	n.mu.Unlock()
	var b bytes.Buffer
	for i := len(entries) - 1; i >= 0; i-- {
		b.Write(model.JSON(entries[i]))
		b.WriteByte('\n')
	}
	return store.Bytes(n.tombstonePath(), b.Bytes(), 0600)
}

// pruneTasks removes terminal task records, logs, and workspaces beyond the
// configured age or count. Running and reserved tasks, and tasks still named
// by a live delegation, are kept. Artifacts are separate and never pruned here.
func (n *Node) pruneTasks(now time.Time) error {
	n.pruneMu.Lock()
	defer n.pruneMu.Unlock()
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
			victims = append(victims, taskTombstone{ID: task.ID, Owner: task.Owner, Spec: specDigest(task.Spec), Capability: task.Spec.Capability, State: task.State, Error: task.Error, Created: task.Created, Updated: task.Updated, Pruned: now.UTC()})
		}
	}
	n.mu.Unlock()
	if len(victims) == 0 {
		return nil
	}
	// Make the tombstones durable before removing any record they replace.
	var b bytes.Buffer
	for _, t := range victims {
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
	compact := len(n.tombstones) > maxTombstones
	n.mu.Unlock()
	for _, id := range removed {
		_ = os.Remove(n.taskPath(id))
		_ = os.Remove(n.logPath(id))
		// The workspace lives under the filesystem root; never follow links out.
		_ = n.root.RemoveAll(filepath.Join("tasks", id))
	}
	slog.Debug("pruned task records", "count", len(removed))
	if compact {
		return n.compactTombstones()
	}
	return nil
}

func (n *Node) runRetention(ctx context.Context) {
	ticker := time.NewTicker(retentionInterval)
	defer ticker.Stop()
	for {
		if err := n.pruneTasks(time.Now()); err != nil {
			slog.Warn("task retention", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
