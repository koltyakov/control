package update

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/koltyakov/control/internal/testutil"
)

func TestMachineCacheRejectsWindowsReparsePoints(t *testing.T) {
	data := []byte("trusted bytes")
	asset := cacheFixture(data)
	for _, kind := range []string{"junction", "reparse-file"} {
		for _, entry := range []string{"root", "lock", "blob"} {
			t.Run(kind+"/"+entry, func(t *testing.T) {
				cache := filepath.Join(t.TempDir(), "cache")
				root, err := openMachineCache(cache)
				if err != nil {
					t.Fatal(err)
				}
				_ = root.Close()
				outside := t.TempDir()
				keep := filepath.Join(outside, "keep")
				if err := os.WriteFile(keep, data, 0600); err != nil {
					t.Fatal(err)
				}
				link := cache
				switch entry {
				case "root":
					if err := os.Remove(cache); err != nil {
						t.Fatal(err)
					}
				case "lock":
					link = filepath.Join(cache, "download.lock")
				case "blob":
					link = filepath.Join(cache, asset.SHA256+".blob")
				}
				if kind == "junction" {
					testutil.Junction(t, outside, link)
				} else {
					testutil.ReparseFile(t, link)
				}
				calls := 0
				private := filepath.Join(t.TempDir(), "binary")
				err = downloadCached(t.Context(), cache, private, asset, func() error {
					calls++
					return SaveBinary(bytes.NewReader(data), private, asset)
				})
				if err != nil || calls != 1 || Verify(private, asset) != nil {
					t.Fatal("hostile cache prevented verified private download", err, calls)
				}
				if contents, err := os.ReadFile(keep); err != nil || !bytes.Equal(contents, data) {
					t.Fatal("cache changed junction target", err)
				}
				if entries, err := os.ReadDir(outside); err != nil || len(entries) != 1 {
					t.Fatal("cache created files through a junction", entries, err)
				}
			})
		}
	}
}

func TestNodePruneWindowsReparseConfinement(t *testing.T) {
	dir, outside := t.TempDir(), t.TempDir()
	old := nodeRetentionBinary(t, dir, "old")
	keep := filepath.Join(outside, "keep")
	if err := os.WriteFile(keep, []byte("untouched"), 0600); err != nil {
		t.Fatal(err)
	}
	testutil.Junction(t, outside, filepath.Join(filepath.Dir(old.Path), "nested-junction"))
	testutil.ReparseFile(t, filepath.Join(filepath.Dir(old.Path), "nested-reparse"))
	top := filepath.Join(dir, "updates", strings.Repeat("a", 64))
	testutil.Junction(t, outside, top)
	opaque := filepath.Join(dir, "updates", strings.Repeat("b", 64))
	testutil.ReparseFile(t, opaque)
	a := &Updater{options: UpdaterOptions{Dir: dir}}
	if err := a.prune(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(old.Path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("ordinary obsolete binary was not pruned", err)
	}
	for _, path := range []string{top, opaque} {
		if _, err := os.Lstat(path); err != nil {
			t.Fatal("top-level reparse entry removed", err)
		}
	}
	if data, err := os.ReadFile(keep); err != nil || string(data) != "untouched" {
		t.Fatal("node pruning escaped update storage", err)
	}
}

func TestGatewayPruneWindowsReparseConfinement(t *testing.T) {
	repo, err := OpenRepository(filepath.Join(t.TempDir(), "updates"))
	if err != nil {
		t.Fatal(err)
	}
	selected := retentionAsset(t, repo, "selected", 2*time.Hour)
	old := retentionAsset(t, repo, "old", 2*time.Hour)
	d, err := repo.Publish(Manifest{Version: "fixture", Assets: []Asset{selected}}, "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.SetPhase(d.ID, "complete", nil); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	keep := filepath.Join(outside, "keep")
	if err := os.WriteFile(keep, []byte("untouched"), 0600); err != nil {
		t.Fatal(err)
	}
	testutil.Junction(t, outside, filepath.Join(repo.dir, "staged", old.SHA256, "nested-junction"))
	testutil.ReparseFile(t, filepath.Join(repo.dir, "staged", old.SHA256, "nested-reparse"))
	top := filepath.Join(repo.dir, "staged", strings.Repeat("a", 64))
	testutil.Junction(t, outside, top)
	opaque := filepath.Join(repo.dir, "blobs", strings.Repeat("b", 64))
	testutil.ReparseFile(t, opaque)
	if _, err := repo.Prune(t.Context(), d.ID, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(repo.dir, "staged", old.SHA256)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("ordinary obsolete staged directory was not pruned", err)
	}
	for _, path := range []string{top, opaque} {
		if _, err := os.Lstat(path); err != nil {
			t.Fatal("top-level reparse entry removed", err)
		}
	}
	if data, err := os.ReadFile(keep); err != nil || string(data) != "untouched" {
		t.Fatal("gateway pruning escaped update storage", err)
	}
}
