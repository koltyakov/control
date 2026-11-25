package node

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/koltyakov/control/internal/identity"
	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/store"
)

func taggedSpec(t *testing.T, id, mode string, tags ...string) model.TaskSpec {
	spec := execSpec(t, id, mode)
	spec.Tags = tags
	return spec
}

func waitTerminalState(t *testing.T, n *Node, owner, id string) model.Task {
	t.Helper()
	task, err := n.waitTask(t.Context(), owner, id, 30*time.Second)
	if err != nil || !task.Terminal() {
		t.Fatal("task did not finish", id, task.State, err)
	}
	return task
}

func waitRunning(t *testing.T, n *Node, id string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		n.mu.Lock()
		state := n.tasks[id].State
		n.mu.Unlock()
		if state == "running" {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("task did not start running", id)
}

func pageIDs(t *testing.T, n *Node, owner, tag string) []string {
	t.Helper()
	page, err := n.taskMethod(t.Context(), owner, "tasks.page", model.JSON(map[string]any{"tag": tag}))
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, summary := range page.(model.TaskPage).Tasks {
		if tag != "" && !slices.Contains(summary.Tags, tag) {
			t.Fatal("filtered summary lacks its tag", summary)
		}
		ids = append(ids, summary.ID)
	}
	return ids
}

// Tags are validated, de-duplicated and sorted before the digest, so order
// does not matter but any other change is a conflicting specification.
func TestTaskTagsValidationAndIdempotentDigest(t *testing.T) {
	t.Parallel()
	n := localNode(t, nil)
	ctx := t.Context()
	for name, tags := range map[string][]string{
		"too many":    {"a", "b", "c", "d", "e", "f", "g", "h", "i"},
		"empty":       {""},
		"too long":    {strings.Repeat("x", model.MaxTaskTagLength+1)},
		"punctuation": {"batch/7"},
		"leading dot": {".hidden"},
		"space":       {"two words"},
		"non-ASCII":   {"пакет"},
	} {
		if _, err := n.startTaskContext(ctx, "owner", taggedSpec(t, "invalid", "brief", tags...)); err == nil {
			t.Fatalf("%s tags accepted", name)
		}
	}
	n.mu.Lock()
	if n.tasks["invalid"] != nil || n.taskIDs["invalid"] != "" {
		n.mu.Unlock()
		t.Fatal("rejected tags left a task or ID reservation")
	}
	n.mu.Unlock()
	longest := strings.Repeat("y", model.MaxTaskTagLength)
	task, err := n.startTaskContext(ctx, "owner", taggedSpec(t, "tagged", "brief", "nightly", "batch-7", "nightly", longest))
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"batch-7", "nightly", longest}; !slices.Equal(task.Spec.Tags, want) {
		t.Fatal("tags not normalized", task.Spec.Tags)
	}
	waitTerminalState(t, n, "owner", "tagged")
	same, err := n.startTaskContext(ctx, "owner", taggedSpec(t, "tagged", "brief", longest, "nightly", "batch-7"))
	if err != nil || same.ID != "tagged" || same.Created != task.Created {
		t.Fatal("reordered tags did not return the existing task", err)
	}
	for _, tags := range [][]string{nil, {"nightly"}, {"batch-7", "nightly", longest, "extra"}} {
		if _, err := n.startTaskContext(ctx, "owner", taggedSpec(t, "tagged", "brief", tags...)); err == nil || !strings.Contains(err.Error(), "different owner or specification") {
			t.Fatal("changed tags retagged or re-ran an existing task", tags, err)
		}
	}
	// An untagged specification encodes exactly as before tags existed, so
	// retained records and tombstones keep matching their stored digests.
	plain := execSpec(t, "plain", "brief")
	plain.Args, plain.TimeoutSeconds = model.JSON(map[string]any{"command": "true"}), 3600
	legacy := struct {
		ID             string          `json:"id,omitempty"`
		Capability     string          `json:"capability"`
		Args           json.RawMessage `json:"args"`
		Inputs         []model.Input   `json:"inputs,omitempty"`
		Outputs        []string        `json:"outputs,omitempty"`
		TimeoutSeconds int             `json:"timeoutSeconds,omitempty"`
		LeaseID        string          `json:"leaseId,omitempty"`
	}{plain.ID, plain.Capability, plain.Args, nil, nil, plain.TimeoutSeconds, ""}
	if string(model.JSON(plain)) != string(model.JSON(legacy)) {
		t.Fatal("untagged specification encoding changed", string(model.JSON(plain)))
	}
}

// A record persisted before tags existed has no tags field and must still
// match an identical untagged resubmission after an upgrade.
func TestTaskTagsPreserveLegacyRecordDigests(t *testing.T) {
	cfg := Config{Name: "local", Gateway: "http://127.0.0.1:1", Token: testToken, DataDir: t.TempDir(), WorkDir: t.TempDir()}
	n := openLocalNode(t, cfg)
	_ = n.Close()
	now := time.Now().UTC()
	legacy := `{"id":"legacy","owner":"owner","spec":{"id":"legacy","capability":"exec.run","args":{"command":"true"},"timeoutSeconds":3600},"state":"succeeded","created":"` + now.Format(time.RFC3339Nano) + `","updated":"` + now.Format(time.RFC3339Nano) + `"}`
	var record model.Task
	if err := json.Unmarshal([]byte(legacy), &record); err != nil {
		t.Fatal(err)
	}
	if err := store.Write(n.taskPath("legacy"), json.RawMessage(legacy)); err != nil {
		t.Fatal(err)
	}
	n = openLocalNode(t, cfg)
	spec := model.TaskSpec{ID: "legacy", Capability: "exec.run", Args: model.JSON(map[string]any{"command": "true"})}
	if task, err := n.startTaskContext(t.Context(), "owner", spec); err != nil || task.State != "succeeded" {
		t.Fatal("legacy record no longer matches its specification", task.State, err)
	}
	spec.Tags = []string{"late"}
	if _, err := n.startTaskContext(t.Context(), "owner", spec); err == nil {
		t.Fatal("a tagged resubmission retagged a legacy record")
	}
}

func TestTaskTagFilterAndCancelTagged(t *testing.T) {
	t.Parallel()
	n := localNode(t, func(c *Config) { c.MaxTasks = 1 })
	ctx := t.Context()
	start := func(owner string, spec model.TaskSpec) {
		t.Helper()
		if _, err := n.startTaskContext(ctx, owner, spec); err != nil {
			t.Fatal(err)
		}
	}
	start("owner", taggedSpec(t, "done", "brief", "batch"))
	waitTerminalState(t, n, "owner", "done")
	start("owner", taggedSpec(t, "running", "sleep", "batch", "extra"))
	waitRunning(t, n, "running")
	start("owner", taggedSpec(t, "queued", "sleep", "batch"))
	start("owner", taggedSpec(t, "other-tag", "sleep", "other"))
	start("owner", execSpec(t, "untagged", "sleep"))
	start("intruder", taggedSpec(t, "foreign", "sleep", "batch"))
	t.Cleanup(func() {
		for _, id := range []string{"running", "queued", "other-tag", "untagged", "foreign"} {
			n.mu.Lock()
			if cancel := n.cancels[id]; cancel != nil {
				cancel()
			}
			n.mu.Unlock()
		}
	})

	if got := pageIDs(t, n, "owner", "batch"); !slices.Equal(got, []string{"done", "queued", "running"}) {
		t.Fatal("tag filter", got)
	}
	if got := pageIDs(t, n, "owner", ""); !slices.Equal(got, []string{"done", "other-tag", "queued", "running", "untagged"}) {
		t.Fatal("unfiltered page", got)
	}
	page, _ := n.taskMethod(ctx, "owner", "tasks.page", model.JSON(map[string]any{"tag": "batch", "limit": 1}))
	if p := page.(model.TaskPage); len(p.Tasks) != 1 || p.NextCursor != "done" || !slices.Equal(p.Tasks[0].Tags, []string{"batch"}) {
		t.Fatal("tagged page bound or cursor", p)
	}
	listed, err := n.taskMethod(ctx, "owner", "tasks.list", model.JSON(map[string]any{"tag": "extra"}))
	if tasks := listed.([]model.Task); err != nil || len(tasks) != 1 || tasks[0].ID != "running" || !slices.Equal(tasks[0].Spec.Tags, []string{"batch", "extra"}) {
		t.Fatal("tagged legacy list", listed, err)
	}
	if _, err := n.taskMethod(ctx, "owner", "tasks.page", model.JSON(map[string]any{"tag": "bad tag"})); err == nil {
		t.Fatal("invalid filter accepted")
	}
	if _, err := n.cancelTagged(ctx, "owner", model.JSON(map[string]any{"tag": ""})); err == nil {
		t.Fatal("cancel without a tag accepted")
	}

	// The node's own identity may inspect every task but cancels only its own.
	self, err := n.dispatch(ctx, n.Identity.ID, model.CancelTaggedTasks, model.JSON(map[string]any{"tag": "batch"}))
	if err != nil || len(self.(model.TaggedCancellation).Tasks) != 0 || self.(model.TaggedCancellation).AlreadyTerminal != 0 {
		t.Fatal("local identity reached another owner's tasks", self, err)
	}

	result, err := n.cancelTagged(ctx, "owner", model.JSON(map[string]any{"tag": "batch"}))
	if err != nil {
		t.Fatal(err)
	}
	want := []model.TaggedCancelResult{{ID: "queued", Outcome: model.TaggedCancelRequested, State: "queued"}, {ID: "running", Outcome: model.TaggedCancelRequested, State: "running"}}
	if result.Tag != "batch" || result.AlreadyTerminal != 1 || !slices.Equal(result.Tasks, want) {
		t.Fatalf("cancel outcomes %+v", result)
	}
	for _, id := range []string{"queued", "running"} {
		if task := waitTerminalState(t, n, "owner", id); task.State != "cancelled" {
			t.Fatal("tagged task not cancelled", id, task.State)
		}
	}
	for owner, id := range map[string]string{"owner": "other-tag", "intruder": "foreign"} {
		n.mu.Lock()
		state := n.tasks[id].State
		n.mu.Unlock()
		if state == "cancelled" || state == "succeeded" || state == "failed" {
			t.Fatal("cancel-by-tag touched an unrelated task", owner, id, state)
		}
	}
	again, err := n.cancelTagged(ctx, "owner", model.JSON(map[string]any{"tag": "batch"}))
	if err != nil || len(again.Tasks) != 0 || again.AlreadyTerminal != 3 {
		t.Fatalf("repeated cancel was not idempotent: %+v %v", again, err)
	}
}

// A worker holding a grant can never cancel by tag: grants are bound to one
// task ID, and the method can neither be reached nor delegated.
func TestCancelTaggedIsNotDelegable(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	n := &Node{Identity: &identity.Identity{ID: "self"}}
	grant := &delegationState{grant: model.Delegation{Method: "tasks.start", Params: model.JSON(model.TaskSpec{ID: "one", Capability: "exec.run", Tags: []string{"batch"}})}, ctx: ctx}
	delegated := context.WithValue(ctx, authorityContextKey{}, authority{owner: "owner", grant: grant})
	if err := n.authorize(delegated, "self", model.CancelTaggedTasks); err == nil {
		t.Fatal("task grant authorized cancel-by-tag")
	}
	if _, err := n.cancelTagged(delegated, "owner", model.JSON(map[string]any{"tag": "batch"})); err == nil || !strings.Contains(err.Error(), "delegated grant") {
		t.Fatal("cancel-by-tag ran under a grant", err)
	}
	direct := context.WithValue(ctx, authorityContextKey{}, authority{owner: "owner"})
	if _, err := n.delegationMethod(direct, "client", "access.grant", model.JSON(model.Delegation{Method: model.CancelTaggedTasks, Params: model.JSON(map[string]any{"tag": "batch"})})); err == nil || !strings.Contains(err.Error(), "cannot be delegated") {
		t.Fatal("cancel-by-tag grant issued", err)
	}
	if requiresAdmission(model.CancelTaggedTasks) || observational(model.CancelTaggedTasks) || !trackOperation(model.CancelTaggedTasks) {
		t.Fatal("cancel-by-tag classification: recovery like tasks.cancel, tracked as an operation")
	}
}

func TestNodeDescribeAdvertisesTaskTags(t *testing.T) {
	n := localNode(t, nil)
	description, err := n.dispatch(t.Context(), n.Identity.ID, "node.describe", model.JSON(map[string]any{}))
	if err != nil || !model.TaskTagsAdvertised(model.JSON(description)) {
		t.Fatal("node.describe lacks task tags", err)
	}
}

// Tags persist with the record, survive interruption at restart and are
// removed with the record by retention. Tombstones keep only the digest.
func TestTaskTagsPersistAndRetention(t *testing.T) {
	t.Parallel()
	cfg := Config{Name: "local", Gateway: "http://127.0.0.1:1", Token: testToken, DataDir: t.TempDir(), WorkDir: t.TempDir(), MaxRetainedTasks: 1}
	n := openLocalNode(t, cfg)
	_ = n.Close()
	now := time.Now().UTC()
	spec := func(id string) model.TaskSpec {
		return model.TaskSpec{ID: id, Capability: "exec.run", Args: model.JSON(map[string]any{"command": "true"}), TimeoutSeconds: 3600, Tags: []string{"batch", "nightly"}}
	}
	for id, state := range map[string]string{"old": "succeeded", "new": "succeeded", "unfinished": "running"} {
		updated := now
		if id == "old" {
			updated = now.Add(-time.Hour)
		}
		if err := store.Write(n.taskPath(id), &model.Task{ID: id, Owner: "owner", Spec: spec(id), State: state, Created: updated, Updated: updated}); err != nil {
			t.Fatal(err)
		}
	}
	n = openLocalNode(t, cfg)
	if got := pageIDs(t, n, "owner", "nightly"); !slices.Equal(got, []string{"new", "old", "unfinished"}) {
		t.Fatal("tags lost across restart", got)
	}
	if task, err := n.ownedTask("owner", "unfinished"); err != nil || task.State != "interrupted" || !slices.Equal(task.Spec.Tags, []string{"batch", "nightly"}) {
		t.Fatal("interrupted record lost its tags", task, err)
	}
	if err := n.pruneTasks(now); err != nil {
		t.Fatal(err)
	}
	if got := pageIDs(t, n, "owner", "batch"); slices.Contains(got, "old") {
		t.Fatal("pruned task still listed by tag", got)
	}
	n.mu.Lock()
	_, pruned := n.tombstones["old"]
	n.mu.Unlock()
	if !pruned {
		t.Fatal("old task was not pruned")
	}
	resubmitted := spec("old")
	resubmitted.Tags = []string{"nightly", "batch"}
	if task, err := n.startTaskContext(t.Context(), "owner", resubmitted); err != nil || !task.Pruned || len(task.Spec.Tags) != 0 {
		t.Fatal("pruned tagged task did not reconcile from its tombstone digest", task, err)
	}
	resubmitted.Tags = []string{"batch"}
	if _, err := n.startTaskContext(t.Context(), "owner", resubmitted); err == nil {
		t.Fatal("pruned task accepted changed tags")
	}
}
