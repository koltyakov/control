package node

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/store"
)

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
