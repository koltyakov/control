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
)

const (
	retentionInterval = 10 * time.Minute
	// Tombstones keep pruned task IDs from being executed again by a retried
	// submission. They are small and outlive the full records by far.
	maxTombstones = 50000
	journalSlack  = 1024
	tombstoneTTL  = 180 * 24 * time.Hour
)

// File-removal failures are independent. A journal failure must stop pruning
// before further tombstones are appended or more task records are removed.
var errTaskCleanupPersistence = errors.New("persist task cleanup")

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

// Older writers could cut Error inside a UTF-8 rune. encoding/json wrote the
// resulting invalid bytes as \ufffd, but reading and re-encoding writes the
// literal replacement rune. Verify the old writer's exact bytes, accepting
// no other difference from the canonical encoding.
func (t taskTombstone) hasLegacyChecksum(raw []byte) bool {
	if !digestID.MatchString(t.Checksum) || !bytes.Contains(raw, []byte(`\ufffd`)) {
		return false
	}
	raw, ok := bytes.CutSuffix(bytes.TrimSuffix(raw, []byte{'\n'}), []byte(`,"checksum":"`+t.Checksum+`"}`))
	if !ok {
		return false
	}
	checksum := t.Checksum
	t.Checksum = ""
	canonical := model.JSON(t)
	const errorField = `,"error":`
	i := bytes.Index(canonical, []byte(errorField))
	if i < 0 {
		return false
	}
	i += len(errorField)
	encodedError := model.JSON(t.Error)
	suffix := canonical[i+len(encodedError) : len(canonical)-1]
	rest, ok := bytes.CutPrefix(raw, canonical[:i])
	if !ok {
		return false
	}
	const replacement = "\uFFFD"
	const escaped = `\ufffd`
	for len(encodedError) > 0 {
		// A literal replacement rune was already valid before the old writer.
		if bytes.HasPrefix(encodedError, []byte(replacement)) && bytes.HasPrefix(rest, []byte(escaped)) {
			rest, encodedError = rest[len(escaped):], encodedError[len(replacement):]
		} else if len(rest) > 0 && rest[0] == encodedError[0] {
			rest, encodedError = rest[1:], encodedError[1:]
		} else {
			return false
		}
	}
	if !bytes.Equal(rest, suffix) {
		return false
	}
	hash := sha256.New()
	_, _ = hash.Write(raw)
	_, _ = hash.Write([]byte{'}'})
	return hex.EncodeToString(hash.Sum(nil)) == checksum
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
		if !safeID.MatchString(t.ID) || t.Owner == "" || !digestID.MatchString(t.Spec) || !t.task().Terminal() || t.Pruned.IsZero() {
			return fmt.Errorf("invalid task tombstone at line %d; journal was not modified", line)
		}
		if t.Checksum != "" && t.Checksum != t.checksum() {
			if !t.hasLegacyChecksum(raw) {
				return fmt.Errorf("invalid task tombstone checksum at line %d; journal was not modified", line)
			}
			stale = true
		}
		if prior, duplicate := n.tombstones[t.ID]; duplicate {
			if prior.Owner != t.Owner || prior.Spec != t.Spec {
				return fmt.Errorf("conflicting task tombstone at line %d", line)
			}
			// A synced completion supersedes its pending intent. Both use
			// the existing checksummed format, including for older readers.
			expected := prior
			expected.CleanupPending, expected.Checksum = false, t.Checksum
			if prior.CleanupPending && expected == t {
				n.journalExtra++
			} else {
				stale = true
			}
		}
		n.tombstones[t.ID] = t
		if !bytes.HasSuffix(raw, []byte{'\n'}) {
			stale = true
		}
	}
	if err = scanner.Err(); err != nil {
		return err
	}
	// Resolve task-file precedence and expiry against the final intent.
	// An earlier pending line must not hide a surviving full record after a
	// completed cleanup, or resurrect an expired tombstone.
	completed := 0
	for id, t := range n.tombstones {
		if task := n.tasks[id]; task != nil {
			if task.Owner != t.Owner || n.taskDigestLocked(task) != t.Spec {
				return fmt.Errorf("task %s conflicts with its tombstone", id)
			}
			if !t.CleanupPending {
				delete(n.tombstones, id)
				stale = true
				continue
			}
			delete(n.tasks, id)
			delete(n.coldTasks, id)
		}
		if !t.CleanupPending && now.Sub(t.Pruned) > tombstoneTTL {
			delete(n.tombstones, id)
			stale = true
			continue
		}
		if err := n.reserveTaskIDLocked(id); err != nil {
			return fmt.Errorf("task tombstone %s; preserve task records and the journal and recover conflicting IDs before starting: %w", id, err)
		}
		if !t.CleanupPending {
			completed++
		}
	}
	if stale || completed > maxTombstones+journalSlack || n.journalNeedsCompaction() {
		// Windows refuses replacement while the read handle is open. Finish
		// reading and close it before atomically rewriting the same journal.
		if err := f.Close(); err != nil {
			return err
		}
		return n.compactTombstones()
	}
	return nil
}

// Amortize replacements even for a busy node near the history limit.
// Pending intents survive every compaction. Only new completions can make
// progress toward reclaiming the space they occupy.
func (n *Node) journalNeedsCompaction() bool {
	return n.journalExtra >= journalSlack || n.journalExtra > 0 && len(n.tombstones) > maxTombstones+journalSlack
}

// compactTombstones rewrites the file with the newest retained entries.
// Caller serializes it with pruneMu or runs before Start. It is also the only
// recovery from a dirty journal: the rewrite contains exactly the adopted
// tombstones, including pending cleanup intent, and discards torn or
// unadopted appended bytes. Unadopted victims still have their task records.
func (n *Node) compactTombstones() error {
	n.mu.Lock()
	entries := make([]taskTombstone, 0, len(n.tombstones))
	for _, t := range n.tombstones {
		entries = append(entries, t)
	}
	n.mu.Unlock()
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Pruned.Equal(entries[j].Pruned) {
			return entries[i].ID < entries[j].ID
		}
		return entries[i].Pruned.After(entries[j].Pruned)
	})
	kept := entries[:0]
	for _, t := range entries {
		if len(kept) < maxTombstones || t.CleanupPending {
			kept = append(kept, t)
		}
	}
	entries = kept
	var b bytes.Buffer
	for i := len(entries) - 1; i >= 0; i-- {
		entries[i].Checksum = entries[i].checksum()
		b.Write(model.JSON(entries[i]))
		b.WriteByte('\n')
	}
	// A failed replacement may already have renamed the new journal without a
	// durable directory entry, so the on-disk version is uncertain. Keep the
	// journal dirty: no append may follow until a complete rewrite succeeds.
	if err := n.journalReplace(n.tombstonePath(), b.Bytes(), 0600); err != nil {
		n.journalDirty = true
		return err
	}
	n.journalDirty = false
	n.journalExtra = 0
	n.mu.Lock()
	defer n.mu.Unlock()
	old := n.tombstones
	n.tombstones = make(map[string]taskTombstone, len(entries))
	for _, t := range entries {
		n.tombstones[t.ID] = t
	}
	for id := range old {
		n.releaseTaskIDLocked(id)
	}
	return nil
}

// pruneTasks removes terminal task records, logs, and workspaces beyond the
// configured age or count. Running and reserved tasks, and tasks still named
// by a live delegation, are kept. Artifacts are separate and never pruned here.
func (n *Node) pruneTasks(now time.Time) error {
	n.pruneMu.Lock()
	defer n.pruneMu.Unlock()
	// Recover a dirty journal before anything else, even when nothing new is
	// eligible. Appending after torn or uncommitted bytes could turn them into
	// a corrupt interior record that blocks startup.
	if n.journalDirty {
		if err := n.compactTombstones(); err != nil {
			return fmt.Errorf("%w: recover tombstone journal: %w", errTaskCleanupPersistence, err)
		}
	}
	cleanupErr := n.retryTaskCleanup()
	if errors.Is(cleanupErr, errTaskCleanupPersistence) {
		return cleanupErr
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
			victims = append(victims, taskTombstone{ID: task.ID, Owner: task.Owner, Spec: n.taskDigestLocked(task), Capability: task.Spec.Capability, State: task.State, Error: task.Error, Created: task.Created, Updated: task.Updated, Pruned: now.UTC(), CleanupPending: true})
		}
	}
	n.mu.Unlock()
	if len(victims) == 0 {
		return cleanupErr
	}
	// Make the tombstones durable before removing any record they replace.
	var b bytes.Buffer
	for i := range victims {
		victims[i].Error = persistedTaskError(victims[i].Error)
		victims[i].Checksum = victims[i].checksum()
		t := victims[i]
		b.Write(model.JSON(t))
		b.WriteByte('\n')
	}
	// Any append error leaves the journal state unknown: none, some, or all of
	// the bytes may be durable. Do not adopt these victims or remove their
	// records; recovery rewrites the journal from adopted state first.
	if err := n.journalAppend(n.tombstonePath(), b.Bytes()); err != nil {
		n.journalDirty = true
		return errors.Join(cleanupErr, fmt.Errorf("%w: append task tombstones: %w", errTaskCleanupPersistence, err))
	}
	removed := make([]string, 0, len(victims))
	var detached []*coldTask
	n.mu.Lock()
	for _, t := range victims {
		if task := n.tasks[t.ID]; task != nil && task.Terminal() {
			delete(n.tasks, t.ID)
			if c := n.coldTasks[t.ID]; c != nil {
				detached = append(detached, c)
				delete(n.coldTasks, t.ID)
			}
			n.tombstones[t.ID] = t
			removed = append(removed, t.ID)
		}
	}
	if len(removed) > 0 {
		n.rebuildRecentTasksLocked()
	}
	n.mu.Unlock()
	for _, c := range detached {
		c.readers.Wait()
	}
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
	var completed []taskTombstone
	var records bytes.Buffer
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
		t.CleanupPending = false
		t.Checksum = t.checksum()
		completed = append(completed, t)
		records.Write(model.JSON(t))
		records.WriteByte('\n')
	}
	if len(completed) > 0 {
		if err := n.journalAppend(n.tombstonePath(), records.Bytes()); err != nil {
			n.journalDirty = true
			return errors.Join(errors.Join(failures...), fmt.Errorf("%w: append cleanup completions: %w", errTaskCleanupPersistence, err))
		}
		n.mu.Lock()
		for _, t := range completed {
			n.tombstones[t.ID] = t
		}
		n.mu.Unlock()
		n.journalExtra += len(completed)
		if n.journalNeedsCompaction() {
			if err := n.compactTombstones(); err != nil {
				failures = append(failures, fmt.Errorf("%w: %w", errTaskCleanupPersistence, err))
			}
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
		if err := n.uploads.Prune(ctx, time.Now()); err != nil && ctx.Err() == nil {
			slog.Warn("upload retention", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
