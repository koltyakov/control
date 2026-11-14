package node

import (
	"bytes"
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/store"
)

var errInjectedJournal = errors.New("injected journal failure")

func journalSpec(id string) model.TaskSpec {
	return model.TaskSpec{ID: id, Capability: "exec.run", Args: model.JSON(map[string]any{"command": "true"}), TimeoutSeconds: 3600}
}

// addExpiredTask stores a terminal task record old enough to be pruned.
func addExpiredTask(t *testing.T, n *Node, id string) {
	t.Helper()
	updated := time.Now().UTC().Add(-2 * time.Hour)
	task := &model.Task{ID: id, Owner: "owner", Spec: journalSpec(id), State: "succeeded", Created: updated, Updated: updated}
	if err := store.Write(n.taskPath(id), task); err != nil {
		t.Fatal(err)
	}
	n.mu.Lock()
	n.tasks[id] = task
	n.mu.Unlock()
}

// holdTask makes a retained task temporarily ineligible, so a prune pass has
// no new candidates and can only perform journal recovery.
func holdTask(n *Node, id string, held bool) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if held {
		n.cancels[id] = func() {}
	} else {
		delete(n.cancels, id)
	}
}

func assertPrunedDeduplicated(t *testing.T, n *Node, id string) {
	t.Helper()
	again, err := n.startTaskContext(context.Background(), "owner", journalSpec(id))
	if err != nil || !again.Pruned {
		t.Fatal("pruned submission was not deduplicated", again, err)
	}
	if _, err := os.Stat(n.taskPath(id)); !os.IsNotExist(err) {
		t.Fatal("pruned task executed again or kept its record", err)
	}
}

func TestTombstoneAppendFailureRecoversBeforeAppending(t *testing.T) {
	for _, tc := range []struct {
		name string
		// write simulates the bytes a failed append left behind.
		write func(path string, data []byte) error
	}{
		{"partial write", func(path string, data []byte) error { return store.Append(path, data[:len(data)/2]) }},
		{"full write then sync failure", func(path string, data []byte) error { return store.Append(path, data) }},
		{"nothing written", func(string, []byte) error { return nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n := localNode(t, func(c *Config) { c.TaskRetentionHours = 1 })
			// An adopted tombstone and its pending intent must survive recovery.
			addExpiredTask(t, n, "adopted")
			if err := n.pruneTasks(time.Now()); err != nil {
				t.Fatal(err)
			}
			addExpiredTask(t, n, "victim")
			appends := 0
			n.journalAppend = func(path string, data []byte) error {
				appends++
				if err := tc.write(path, data); err != nil {
					return err
				}
				return errInjectedJournal
			}
			if err := n.pruneTasks(time.Now()); !errors.Is(err, errInjectedJournal) || !errors.Is(err, errTaskCleanupPersistence) {
				t.Fatal("append failure was not reported as a persistence failure", err)
			}
			n.mu.Lock()
			_, adopted := n.tombstones["victim"]
			retained := n.tasks["victim"] != nil
			n.mu.Unlock()
			if adopted || !retained || !n.journalDirty {
				t.Fatal("failed append adopted its victim or did not mark the journal dirty", adopted, retained, n.journalDirty)
			}
			if _, err := os.Stat(n.taskPath("victim")); err != nil {
				t.Fatal("failed append removed its victim record", err)
			}
			// Recovery runs even with no new candidate, and never appends first.
			holdTask(n, "victim", true)
			n.journalAppend = func(string, []byte) error { appends++; return errInjectedJournal }
			before := appends
			if err := n.pruneTasks(time.Now()); err != nil {
				t.Fatal("dirty journal was not recovered without new candidates", err)
			}
			if appends != before || n.journalDirty {
				t.Fatal("recovery appended or left the journal dirty", appends-before, n.journalDirty)
			}
			journal, err := os.ReadFile(n.tombstonePath())
			if err != nil || bytes.Contains(journal, []byte("victim")) || !bytes.Contains(journal, []byte("adopted")) {
				t.Fatalf("recovered journal does not match adopted state: %q %v", journal, err)
			}
			n.journalAppend = store.Append
			holdTask(n, "victim", false)
			if err := n.pruneTasks(time.Now()); err != nil {
				t.Fatal(err)
			}
			cfg := n.Config
			if err := n.Close(); err != nil {
				t.Fatal(err)
			}
			reopened := openLocalNode(t, cfg)
			for _, id := range []string{"adopted", "victim"} {
				assertPrunedDeduplicated(t, reopened, id)
			}
		})
	}
}

// A full append whose sync failed may be durable. A restart before recovery
// must finish that prune from the journal rather than reject or forget it.
func TestTombstoneAppendFailureBeforeRestartRemainsLoadable(t *testing.T) {
	for _, partial := range []bool{false, true} {
		n := localNode(t, func(c *Config) { c.TaskRetentionHours = 1 })
		addExpiredTask(t, n, "victim")
		n.journalAppend = func(path string, data []byte) error {
			if partial {
				data = data[:len(data)-len(data)/3]
			}
			if err := store.Append(path, data); err != nil {
				return err
			}
			return errInjectedJournal
		}
		if err := n.pruneTasks(time.Now()); !errors.Is(err, errInjectedJournal) {
			t.Fatal(err)
		}
		cfg := n.Config
		if err := n.Close(); err != nil {
			t.Fatal(err)
		}
		reopened := openLocalNode(t, cfg)
		reopened.mu.Lock()
		_, pruned := reopened.tombstones["victim"]
		retained := reopened.tasks["victim"] != nil
		reopened.mu.Unlock()
		if partial == pruned || partial != retained {
			t.Fatal("restart did not reconcile the failed append", partial, pruned, retained)
		}
		if pruned {
			// The loaded tombstone is durable cleanup intent; finish it.
			if err := reopened.pruneTasks(time.Now()); err != nil {
				t.Fatal(err)
			}
			assertPrunedDeduplicated(t, reopened, "victim")
		}
	}
}

// A replacement that renamed the journal but failed its directory sync leaves
// the durable version uncertain. Appends stay blocked until a rewrite succeeds.
func TestTombstoneCompactionFailureBlocksAppends(t *testing.T) {
	n := localNode(t, func(c *Config) { c.TaskRetentionHours = 1 })
	addExpiredTask(t, n, "victim")
	appends := 0
	n.journalAppend = func(path string, data []byte) error {
		appends++
		_ = store.Append(path, data[:len(data)/2])
		return errInjectedJournal
	}
	if err := n.pruneTasks(time.Now()); !errors.Is(err, errInjectedJournal) {
		t.Fatal(err)
	}
	n.journalAppend = func(path string, data []byte) error { appends++; return store.Append(path, data) }
	n.journalReplace = func(path string, data []byte, mode os.FileMode) error {
		if err := store.Bytes(path, data, mode); err != nil {
			return err
		}
		return errInjectedJournal // Renamed, but the directory sync "failed".
	}
	for range 2 {
		if err := n.pruneTasks(time.Now()); !errors.Is(err, errTaskCleanupPersistence) || !errors.Is(err, errInjectedJournal) {
			t.Fatal("failed recovery was not reported", err)
		}
		if appends != 1 || !n.journalDirty {
			t.Fatal("appended while the journal was uncertain", appends, n.journalDirty)
		}
		if _, err := os.Stat(n.taskPath("victim")); err != nil {
			t.Fatal("victim removed before its tombstone was durable", err)
		}
	}
	n.journalReplace = store.Bytes
	if err := n.pruneTasks(time.Now()); err != nil || n.journalDirty || appends != 2 {
		t.Fatal("recovery did not resume pruning", err, n.journalDirty, appends)
	}
	cfg := n.Config
	if err := n.Close(); err != nil {
		t.Fatal(err)
	}
	assertPrunedDeduplicated(t, openLocalNode(t, cfg), "victim")
}
