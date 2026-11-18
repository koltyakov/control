package node

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/store"
)

func TestArtifactLookupReadsOnlyVisibleRequestedRecord(t *testing.T) {
	n := localNode(t, nil)
	id := strings.Repeat("a", 64)
	a := storedArtifact{Artifact: model.Artifact{ID: id}, Owners: []string{"alice"}}
	if err := store.Write(n.artifactPath(id)+".json", a); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(n.artifactPath(strings.Repeat("b", 64))+".json", []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, owner := range []string{"alice", "bob", n.Identity.ID} {
		ctx := context.WithValue(t.Context(), activityContextKey{}, activityContext{owner: owner})
		v, err := n.artifactMethod(ctx, owner, "artifacts.list", model.JSON(map[string]string{"id": id}))
		if err != nil {
			t.Fatal(err)
		}
		records := v.([]model.Artifact)
		if owner == "bob" && len(records) != 0 || owner != "bob" && (len(records) != 1 || records[0].ID != id) {
			t.Fatal(owner, records)
		}
	}
}

func TestArtifactListingHandlesDisappearingMetadata(t *testing.T) {
	dir := t.TempDir()
	for _, a := range []storedArtifact{
		{Artifact: model.Artifact{ID: "deleted"}, Owners: []string{"owner"}},
		{Artifact: model.Artifact{ID: "remaining"}, Owners: []string{"owner"}},
		{Artifact: model.Artifact{ID: "hidden"}, Owners: []string{"other"}},
	} {
		if err := store.Write(filepath.Join(dir, a.ID+".json"), a); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	deleted := filepath.Join(dir, "deleted.json")
	if err := os.Remove(deleted); err != nil {
		t.Fatal(err)
	}
	read := func(name string, dst *storedArtifact) error {
		return store.Read(filepath.Join(dir, name), dst)
	}
	got, err := listArtifacts(entries, "owner", "node", read)
	if err != nil || len(got) != 1 || got[0].ID != "remaining" {
		t.Fatalf("listed artifacts = %v, err = %v", got, err)
	}
	if _, err := listArtifacts(entries, "owner", "node", func(string, *storedArtifact) error {
		return os.ErrPermission
	}); !errors.Is(err, os.ErrPermission) {
		t.Fatalf("list hid permission error: %v", err)
	}
	if err := os.WriteFile(deleted, []byte(`{`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := listArtifacts(entries, "owner", "node", read); err == nil {
		t.Fatal("list hid malformed metadata")
	}
}
