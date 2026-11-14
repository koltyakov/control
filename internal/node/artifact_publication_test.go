package node

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/koltyakov/control/internal/model"
)

func ownerContext(owner string) context.Context {
	return context.WithValue(context.Background(), activityContextKey{}, activityContext{owner: owner})
}

func deleteArtifactAs(n *Node, owner, id string) error {
	_, err := n.artifactMethod(ownerContext(owner), owner, "artifacts.delete", model.JSON(map[string]any{"id": id}))
	return err
}

func artifactReaders(n *Node) int {
	n.artifactMu.Lock()
	defer n.artifactMu.Unlock()
	total := 0
	for _, count := range n.artifactReaders {
		total += count
	}
	return total
}

// A published output must stay undeletable until its task record refers to
// it. The pin is taken under the same transfer lock as publication, so a
// deletion queued behind the import can never slip in before the pin.
func TestPublishedOutputPinBlocksDeletionUntilReleased(t *testing.T) {
	n := localNode(t, nil)
	owner := "alice"
	// Same-owner content that a new task output will deduplicate onto.
	existing, err := n.importArtifact(ownerContext(owner), strings.NewReader("deduplicated output"), "old.txt")
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(n.Config.WorkDir, "tasks", "publisher")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{"dedup.txt": "deduplicated output", "fresh.txt": "fresh output"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	// Queue a same-owner deletion behind the deduplicated content's lock, so
	// it races publication exactly at the lock handoff.
	release := holdPipelineTransfer(t, n, existing.ID)
	published := make(chan struct {
		artifacts []model.Artifact
		pins      []func()
		err       error
	}, 1)
	ctx := ownerContext(owner)
	go func() {
		a, pins, err := n.publishTaskOutputs(ctx, "publisher", []string{"dedup.txt", "fresh.txt", "missing.txt"})
		published <- struct {
			artifacts []model.Artifact
			pins      []func()
			err       error
		}{a, pins, err}
	}()
	deleted := make(chan error, 1)
	go func() { deleted <- deleteArtifactAs(n, owner, existing.ID) }()
	time.Sleep(50 * time.Millisecond) // Let both contenders queue on the lock.
	release()
	result := <-published
	if result.err == nil || len(result.artifacts) != 2 || len(result.pins) != 2 {
		t.Fatal("partial publication did not return its pinned outputs", result.artifacts, len(result.pins), result.err)
	}
	if result.artifacts[0].ID != existing.ID {
		t.Fatal("output did not deduplicate", result.artifacts[0].ID)
	}
	<-deleted // Either order is valid; the published content must survive it.
	for _, a := range result.artifacts {
		if _, err := n.artifact(a.ID); err != nil {
			t.Fatal("pinned output was deleted before its task referenced it", a.ID, err)
		}
		if err := deleteArtifactAs(n, owner, a.ID); err == nil || !strings.Contains(err.Error(), "reader") {
			t.Fatal("deletion ignored a publication pin", a.ID, err)
		}
	}
	for _, unpin := range result.pins {
		unpin()
		unpin() // Releases are idempotent.
	}
	if readers := artifactReaders(n); readers != 0 {
		t.Fatal("publication pins leaked", readers)
	}
	for _, a := range result.artifacts {
		if err := deleteArtifactAs(n, owner, a.ID); err != nil {
			t.Fatal("released output could not be deleted", a.ID, err)
		}
	}
}

// Terminal publication releases pins on success, partial failure, large
// results, and the in-memory fallback after a failed completion write. The
// in-memory reference then protects content without a durable-result claim.
func TestTaskPublicationReleasesPinsAfterTerminalReference(t *testing.T) {
	n, _ := pipelineNode(t)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	owner := n.Identity.ID
	for id, args := range map[string]pipelineArgs{
		"published":    {Output: "published output"},
		"partial-fail": {Output: "partial output", Fail: true},
		"large-result": {LargeResult: true},
	} {
		if _, err := n.startTaskContext(ctx, owner, pipelineSpec(id, args)); err != nil {
			t.Fatal(err)
		}
		state := "succeeded"
		if args.Fail {
			state = "failed"
		}
		pipelineFinished(t, n, ctx, owner, id, state)
	}
	if readers := artifactReaders(n); readers != 0 {
		t.Fatal("terminal tasks left publication pins", readers)
	}
	err := n.Register(provider{cap: model.Capability{Name: "test.unpersisted", InputSchema: schema(nil)}, run: func(_ context.Context, _ json.RawMessage, e Execution) (any, error) {
		if err := os.WriteFile(filepath.Join(e.Dir, "output.txt"), []byte("unpersisted output"), 0600); err != nil {
			return nil, err
		}
		// A nonempty directory at the record path makes the completion write
		// fail on every platform.
		path := n.taskPath("unpersisted")
		if err := os.Remove(path); err != nil {
			return nil, err
		}
		if err := os.MkdirAll(path, 0700); err != nil {
			return nil, err
		}
		return nil, os.WriteFile(filepath.Join(path, "blocker"), []byte("x"), 0600)
	}})
	if err != nil {
		t.Fatal(err)
	}
	spec := model.TaskSpec{ID: "unpersisted", Capability: "test.unpersisted", Outputs: []string{"output.txt"}}
	if _, err := n.startTaskContext(ctx, owner, spec); err != nil {
		t.Fatal(err)
	}
	got := pipelineFinished(t, n, ctx, owner, spec.ID, "failed")
	if !strings.Contains(got.Error, "persist task completion") || len(got.Artifacts) != 1 {
		t.Fatal("completion write failure was not reported with its in-memory outputs", got.Error, got.Artifacts)
	}
	if readers := artifactReaders(n); readers != 0 {
		t.Fatal("persistence failure leaked publication pins", readers)
	}
	if err := deleteArtifactAs(n, owner, got.Artifacts[0].ID); err == nil || !strings.Contains(err.Error(), "referenced") {
		t.Fatal("unpersisted task output lost its in-memory reference", err)
	}
}
