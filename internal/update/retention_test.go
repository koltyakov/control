package update

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func retentionAsset(t *testing.T, repo *Repository, content string, age time.Duration) Asset {
	t.Helper()
	sum := sha256.Sum256([]byte(content))
	asset := Asset{OS: "linux", Arch: "amd64", File: AssetName("linux", "amd64"), Size: int64(len(content)), SHA256: hex.EncodeToString(sum[:])}
	if err := repo.Upload(strings.NewReader(content), asset); err != nil {
		t.Fatal(err)
	}
	when := time.Now().Add(-age)
	if err := os.Chtimes(repo.Blob(asset.SHA256), when, when); err != nil {
		t.Fatal(err)
	}
	stage := filepath.Join(repo.dir, "staged", asset.SHA256)
	if err := os.MkdirAll(stage, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stage, asset.File), []byte(content), 0700); err != nil {
		t.Fatal(err)
	}
	return asset
}

func TestPruneCompletedUpdatePreservesReferencesAndUploads(t *testing.T) {
	base := t.TempDir()
	repo, err := OpenRepository(filepath.Join(base, "updates"))
	if err != nil {
		t.Fatal(err)
	}
	current := retentionAsset(t, repo, "current", 2*time.Hour)
	obsolete := retentionAsset(t, repo, "obsolete", 2*time.Hour)
	running := retentionAsset(t, repo, "same-version runtime", 2*time.Hour)
	previous := retentionAsset(t, repo, "previous", 2*time.Hour)
	requested := retentionAsset(t, repo, "requested", 2*time.Hour)
	invitation := retentionAsset(t, repo, "invitation", 2*time.Hour)
	pending := retentionAsset(t, repo, "pending release", 2*time.Hour)
	fresh := retentionAsset(t, repo, "fresh unpublished upload", 0)
	for name, asset := range map[string]Asset{"runtime.json": running, "runtime-previous.json": previous, "runtime-request.json": requested} {
		data, _ := json.Marshal(map[string]any{"path": filepath.Join(repo.dir, "staged", asset.SHA256, asset.File), "asset": asset})
		if err := os.WriteFile(filepath.Join(base, name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	unknown := filepath.Join(repo.dir, "blobs", ".update-in-progress")
	if err := os.WriteFile(unknown, []byte("partial"), 0600); err != nil {
		t.Fatal(err)
	}
	d, err := repo.Publish(Manifest{Version: "v1", Assets: []Asset{current}}, "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.SetPhase(d.ID, "complete", nil); err != nil {
		t.Fatal(err)
	}
	release := repo.Pin([]Asset{pending})
	result, err := repo.Prune(t.Context(), d.ID, []string{invitation.SHA256})
	if err != nil || result != (PruneResult{Blobs: 1, Staged: 2}) {
		t.Fatalf("cleanup: %+v, %v", result, err)
	}
	if _, err := os.Stat(repo.Blob(obsolete.SHA256)); !os.IsNotExist(err) {
		t.Fatalf("obsolete blob retained: %v", err)
	}
	for _, asset := range []Asset{current, running, previous, requested, invitation, pending, fresh} {
		if err := Verify(repo.Blob(asset.SHA256), asset); err != nil {
			t.Fatalf("referenced or fresh binary deleted: %s: %v", asset.File, err)
		}
		if asset.SHA256 != fresh.SHA256 {
			if _, err := os.Stat(filepath.Join(repo.dir, "staged", asset.SHA256, asset.File)); err != nil {
				t.Fatalf("referenced staged binary deleted: %v", err)
			}
		}
	}
	if _, err := os.Stat(unknown); err != nil {
		t.Fatalf("temporary upload deleted: %v", err)
	}
	release()
	release() // Releasing pins is idempotent.
	result, err = repo.Prune(t.Context(), d.ID, []string{invitation.SHA256})
	if err != nil || result != (PruneResult{Blobs: 1, Staged: 1}) {
		t.Fatalf("released pin not pruned: %+v, %v", result, err)
	}
	when := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(repo.Blob(fresh.SHA256), when, when); err != nil {
		t.Fatal(err)
	}
	result, err = repo.Prune(t.Context(), d.ID, []string{invitation.SHA256})
	if err != nil || result.Blobs != 1 {
		t.Fatalf("expired unpublished upload retained: %+v, %v", result, err)
	}
}

func TestPruneFailsClosedBeforeCompletionAndOnInvalidRuntime(t *testing.T) {
	for _, phase := range []string{"staging", "waiting-idle", "installing", "error", "complete"} {
		t.Run(phase, func(t *testing.T) {
			base := t.TempDir()
			repo, err := OpenRepository(filepath.Join(base, "updates"))
			if err != nil {
				t.Fatal(err)
			}
			asset := retentionAsset(t, repo, "selected", 2*time.Hour)
			old := retentionAsset(t, repo, "old", 2*time.Hour)
			d, err := repo.Publish(Manifest{Version: "v1", Assets: []Asset{asset}}, "test")
			if err != nil {
				t.Fatal(err)
			}
			if err := repo.SetPhase(d.ID, phase, nil); err != nil {
				t.Fatal(err)
			}
			if phase == "complete" {
				if err := os.WriteFile(filepath.Join(base, "runtime-previous.json"), []byte("{broken"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := repo.Prune(t.Context(), d.ID, nil); err == nil {
				t.Fatal("unsafe cleanup succeeded")
			}
			if err := Verify(repo.Blob(old.SHA256), old); err != nil {
				t.Fatal("unsafe cleanup deleted old binary", err)
			}
		})
	}
}

func TestPruneCancellationSupersessionAndConfinedRemoval(t *testing.T) {
	repo, err := OpenRepository(filepath.Join(t.TempDir(), "updates"))
	if err != nil {
		t.Fatal(err)
	}
	asset := retentionAsset(t, repo, "selected", 2*time.Hour)
	old := retentionAsset(t, repo, "old", 2*time.Hour)
	d, err := repo.Publish(Manifest{Version: "v1", Assets: []Asset{asset}}, "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.SetPhase(d.ID, "complete", nil); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := repo.Prune(ctx, d.ID, nil); err == nil {
		t.Fatal("cancelled cleanup succeeded")
	}
	if _, err := repo.Prune(t.Context(), "superseded", nil); err == nil {
		t.Fatal("superseded cleanup succeeded")
	}
	if err := Verify(repo.Blob(old.SHA256), old); err != nil {
		t.Fatal("cancelled or superseded cleanup deleted binary", err)
	}
	outside := filepath.Join(t.TempDir(), "keep")
	if err := os.WriteFile(outside, []byte("unrelated"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Dir(outside), filepath.Join(repo.dir, "staged", old.SHA256, "link")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := os.Symlink(filepath.Dir(outside), filepath.Join(repo.dir, "staged", strings.Repeat("f", 64))); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Prune(t.Context(), d.ID, nil); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(outside); err != nil || string(data) != "unrelated" {
		t.Fatalf("cleanup escaped update storage: %q, %v", data, err)
	}
}
