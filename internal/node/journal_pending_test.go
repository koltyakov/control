package node

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/koltyakov/control/internal/store"
)

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
