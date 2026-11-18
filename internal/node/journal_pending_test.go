package node

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/store"
)

func TestCleanupCompletionAppendRecovery(t *testing.T) {
	for _, partial := range []bool{true, false} {
		n := localNode(t, func(c *Config) { c.TaskRetentionHours = 1 })
		addExpiredTask(t, n, "expired")
		writes := 0
		n.journalAppend = func(path string, b []byte) error {
			writes++
			if writes == 1 {
				return store.Append(path, b)
			}
			if partial {
				b = b[:len(b)/2]
			}
			if err := store.Append(path, b); err != nil {
				return err
			}
			return errInjectedJournal
		}
		if err := n.pruneTasks(time.Now()); !errors.Is(err, errTaskCleanupPersistence) || !n.journalDirty || !n.tombstones["expired"].CleanupPending {
			t.Fatal("uncertain completion forgot intent", err)
		}
		cfg := n.Config
		if err := n.Close(); err != nil {
			t.Fatal(err)
		}
		n = openLocalNode(t, cfg)
		if n.tombstones["expired"].CleanupPending != partial {
			t.Fatal("restart adopted incorrect completion", partial)
		}
		if err := n.pruneTasks(time.Now()); err != nil {
			t.Fatal(err)
		}
		assertPrunedDeduplicated(t, n, "expired")
	}
}

func TestSmallCleanupDoesNotReplaceHistory(t *testing.T) {
	n := localNode(t, func(c *Config) { c.TaskRetentionHours = 1 })
	addExpiredTask(t, n, "first")
	if err := n.pruneTasks(time.Now()); err != nil {
		t.Fatal(err)
	}
	first, err := os.ReadFile(n.tombstonePath())
	if err != nil {
		t.Fatal(err)
	}
	n.journalReplace = func(string, []byte, os.FileMode) error { return errInjectedJournal }
	addExpiredTask(t, n, "second")
	if err = n.pruneTasks(time.Now()); err != nil {
		t.Fatal(err)
	}
	all, err := os.ReadFile(n.tombstonePath())
	if err != nil || !bytes.HasPrefix(all, first) || bytes.Count(all, []byte{'\n'}) != 4 {
		t.Fatal("expected two append-only intent/completion pairs", err)
	}
}

func TestCompletedCleanupPairWithSurvivingTaskFile(t *testing.T) {
	n := localNode(t, func(c *Config) { c.TaskRetentionHours = 1 })
	addExpiredTask(t, n, "survivor")
	record, err := os.ReadFile(n.taskPath("survivor"))
	if err != nil {
		t.Fatal(err)
	}
	if err = n.pruneTasks(time.Now()); err != nil {
		t.Fatal(err)
	}
	// Model a directory removal that did not survive restart.
	if err = store.Bytes(n.taskPath("survivor"), record, 0600); err != nil {
		t.Fatal(err)
	}
	cfg := n.Config
	if err = n.Close(); err != nil {
		t.Fatal(err)
	}
	n = openLocalNode(t, cfg)
	got, err := n.ownedTask("owner", "survivor")
	if err != nil || got.Pruned || !model.SameJSON(record, model.JSON(got)) {
		t.Fatal("earlier intent shadowed the retained record", err)
	}
}

func TestDirtyJournalPreservesAdoptedCleanupIntent(t *testing.T) {
	n := localNode(t, func(c *Config) { c.TaskRetentionHours = 1 })
	addExpiredTask(t, n, "pending")
	blocker := filepath.Join(n.logPath("pending"), "blocker")
	if err := os.MkdirAll(filepath.Dir(blocker), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(blocker, []byte("retain cleanup intent"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := n.pruneTasks(time.Now()); err == nil {
		t.Fatal("nonempty cleanup directory did not block removal")
	}
	if _, err := os.Stat(n.taskPath("pending")); !os.IsNotExist(err) {
		t.Fatal("fixture did not remove the adopted task record", err)
	}
	addExpiredTask(t, n, "victim")
	n.journalAppend = func(path string, data []byte) error {
		if err := store.Append(path, data[:len(data)/2]); err != nil {
			return err
		}
		return errInjectedJournal
	}
	if err := n.pruneTasks(time.Now()); !errors.Is(err, errInjectedJournal) {
		t.Fatal("partial append failure was not reported", err)
	}
	holdTask(n, "victim", true)
	n.journalAppend = func(string, []byte) error {
		t.Error("recovery appended instead of replacing the dirty journal")
		return errInjectedJournal
	}
	if err := n.pruneTasks(time.Now()); err == nil || errors.Is(err, errTaskCleanupPersistence) || n.journalDirty {
		t.Fatal("journal recovery did not preserve the independent cleanup failure", err, n.journalDirty)
	}
	if !n.tombstones["pending"].CleanupPending {
		t.Fatal("recovery discarded adopted cleanup intent")
	}
	cfg := n.Config
	if err := n.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := openLocalNode(t, cfg)
	if !reopened.tombstones["pending"].CleanupPending {
		t.Fatal("restart lost intent for a task record already removed")
	}
	assertPrunedDeduplicated(t, reopened, "pending")
	if err := os.Remove(blocker); err != nil {
		t.Fatal(err)
	}
	if err := reopened.pruneTasks(time.Now()); err != nil {
		t.Fatal("cleanup did not finish after the blocker was removed", err)
	}
	if reopened.tombstones["pending"].CleanupPending {
		t.Fatal("completed cleanup intent was not durably cleared")
	}
}
