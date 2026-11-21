package node

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/store"
)

func TestArtifactListPagesOnRequestAndKeepsLegacyArray(t *testing.T) {
	t.Parallel()
	n := localNode(t, nil)
	for _, name := range []string{"first", "second", "third"} {
		if _, err := n.importArtifact(t.Context(), strings.NewReader(name), name); err != nil {
			t.Fatal(err)
		}
	}
	legacy, err := n.artifactMethod(t.Context(), n.Identity.ID, "artifacts.list", model.JSON(map[string]any{}))
	if err != nil {
		t.Fatal(err)
	}
	if all, ok := legacy.([]model.Artifact); !ok || len(all) != 3 || all[0].SHA256 == "" || all[0].Node == "" {
		t.Fatalf("unpaged list must keep full references: %#v", legacy)
	}
	result, err := n.artifactMethod(t.Context(), n.Identity.ID, "artifacts.list", model.JSON(map[string]any{"limit": 2}))
	if err != nil {
		t.Fatal(err)
	}
	page, ok := result.(model.ArtifactPage)
	if !ok || len(page.Artifacts) != 2 || page.Total != 3 || page.NextCursor == "" || page.Node != n.Identity.ID {
		t.Fatalf("unexpected first page: %#v", result)
	}
	if encoded := string(model.JSON(page)); strings.Contains(encoded, "sha256") || strings.Count(encoded, n.Identity.ID) != 1 {
		t.Fatalf("summaries repeat implied fields: %s", encoded)
	}
	result, err = n.artifactMethod(t.Context(), n.Identity.ID, "artifacts.list", model.JSON(map[string]any{"limit": 2, "cursor": page.NextCursor}))
	if err != nil {
		t.Fatal(err)
	}
	if next := result.(model.ArtifactPage); len(next.Artifacts) != 1 || next.NextCursor != "" {
		t.Fatalf("unexpected last page: %#v", next)
	}
}

func TestArtifactRetentionExpiresOnlyUnusedOldArtifacts(t *testing.T) {
	t.Parallel()
	n := localNode(t, func(c *Config) { c.ArtifactRetentionHours = 24 })
	imported := map[string]model.Artifact{}
	for _, name := range []string{"old", "referenced", "read", "fresh"} {
		a, err := n.importArtifact(t.Context(), strings.NewReader(name+" content"), name)
		if err != nil {
			t.Fatal(err)
		}
		imported[name] = a
		if name == "fresh" {
			continue
		}
		stored, err := n.storedArtifact(a.ID)
		if err != nil {
			t.Fatal(err)
		}
		stored.Created = time.Now().Add(-48 * time.Hour)
		if err = store.Write(n.artifactPath(a.ID)+".json", stored); err != nil {
			t.Fatal(err)
		}
	}
	task := recentTask("uses-artifact", time.Now())
	task.Artifacts = []model.Artifact{imported["referenced"]}
	n.tasks[task.ID] = task
	unpin, err := n.pinArtifact(t.Context(), imported["read"].ID)
	if err != nil {
		t.Fatal(err)
	}
	defer unpin()
	if err := n.pruneArtifacts(t.Context(), time.Now()); err != nil {
		t.Fatal(err)
	}
	list, err := n.artifactMethod(t.Context(), n.Identity.ID, "artifacts.list", json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	remaining := map[string]bool{}
	for _, a := range list.([]model.Artifact) {
		remaining[a.Name] = true
	}
	if remaining["old"] || !remaining["referenced"] || !remaining["read"] || !remaining["fresh"] {
		t.Fatalf("retention removed the wrong artifacts: %v", remaining)
	}
	if want := imported["referenced"].Size + imported["read"].Size + imported["fresh"].Size; n.artifactUsed != want {
		t.Fatal("expired artifact bytes were not released", n.artifactUsed, want)
	}
}

func TestArtifactRetentionDisabledByDefault(t *testing.T) {
	t.Parallel()
	n := localNode(t, nil)
	a, err := n.importArtifact(t.Context(), strings.NewReader("kept"), "kept")
	if err != nil {
		t.Fatal(err)
	}
	if err := n.pruneArtifacts(t.Context(), a.Created.Add(10*365*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := n.storedArtifact(a.ID); err != nil {
		t.Fatal("default configuration expired an artifact", err)
	}
}

func TestArtifactRetentionConfigBounds(t *testing.T) {
	t.Parallel()
	for hours, valid := range map[int]bool{-1: false, 0: true, 1: true, 87600: true, 87601: false} {
		cfg := Config{Name: "local", Gateway: "http://127.0.0.1:1", Token: testToken, DataDir: t.TempDir(), WorkDir: t.TempDir(), ArtifactRetentionHours: hours}
		if err := cfg.defaults(); (err == nil) != valid {
			t.Errorf("artifactRetentionHours %d: valid=%v, error %v", hours, valid, err)
		}
	}
}

func TestArtifactRetentionCountsFromLatestDeduplicatedHold(t *testing.T) {
	t.Parallel()
	n := localNode(t, func(c *Config) { c.ArtifactRetentionHours = 24 })
	a, err := n.importArtifact(t.Context(), strings.NewReader("shared content"), "first")
	if err != nil {
		t.Fatal(err)
	}
	stored, err := n.storedArtifact(a.ID)
	if err != nil {
		t.Fatal(err)
	}
	stored.Created = time.Now().Add(-48 * time.Hour)
	if err = store.Write(n.artifactPath(a.ID)+".json", stored); err != nil {
		t.Fatal(err)
	}
	// Re-exporting identical bytes hands out the same artifact again.
	again, err := n.importArtifact(t.Context(), strings.NewReader("shared content"), "second")
	if err != nil || again.ID != a.ID {
		t.Fatal("identical content was not deduplicated", again.ID, err)
	}
	if err := n.pruneArtifacts(t.Context(), time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := n.storedArtifact(a.ID); err != nil {
		t.Fatal("retention expired content that was just handed out again", err)
	}
	if err := n.pruneArtifacts(t.Context(), time.Now().Add(25*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := n.storedArtifact(a.ID); err == nil {
		t.Fatal("retention never expired the unused artifact")
	}
}
