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

	"github.com/koltyakov/control/internal/buildinfo"
	"github.com/koltyakov/control/internal/store"
)

type nodeRuntime struct {
	Path    string `json:"path"`
	Version string `json:"version"`
	Asset   Asset  `json:"asset"`
}

func nodeRetentionBinary(t *testing.T, dir, version string) nodeRuntime {
	t.Helper()
	data := []byte(version)
	hash := sha256.Sum256(data)
	asset := Asset{OS: "linux", Arch: "amd64", File: AssetName("linux", "amd64"), Size: int64(len(data)), SHA256: hex.EncodeToString(hash[:])}
	path := filepath.Join(dir, "updates", asset.SHA256, asset.File)
	if err := SaveBinary(strings.NewReader(version), path, asset); err != nil {
		t.Fatal(err)
	}
	return nodeRuntime{Path: path, Version: version, Asset: asset}
}

func TestNodePruneKeepsCurrentAndPendingBinaries(t *testing.T) {
	dir := t.TempDir()
	current := nodeRetentionBinary(t, dir, "current")
	previous := nodeRetentionBinary(t, dir, "previous")
	obsolete := nodeRetentionBinary(t, dir, "obsolete")
	staged := nodeRetentionBinary(t, dir, "staged")
	requested := nodeRetentionBinary(t, dir, "requested")
	for name, binary := range map[string]nodeRuntime{"runtime.json": current, "runtime-previous.json": previous, "runtime-request.json": requested} {
		if err := store.Write(filepath.Join(dir, name), binary); err != nil {
			t.Fatal(err)
		}
	}
	a := &Updater{options: UpdaterOptions{Dir: dir, Software: buildinfo.Info{Version: current.Version, OS: current.Asset.OS, Arch: current.Asset.Arch, SHA256: current.Asset.SHA256}}, staged: staged.Path}
	if err := a.prune(t.Context()); err != nil {
		t.Fatal(err)
	}
	for _, binary := range []nodeRuntime{current, staged, requested} {
		if err := Verify(binary.Path, binary.Asset); err != nil {
			t.Fatal("removed a referenced binary", err)
		}
	}
	for _, path := range []string{filepath.Dir(previous.Path), filepath.Dir(obsolete.Path), filepath.Join(dir, "runtime-previous.json")} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("obsolete binary or recovery record retained: %s, %v", path, err)
		}
	}
	// Superseding a download releases it without waiting for another restart.
	a.staged = ""
	if err := a.prune(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(staged.Path); !os.IsNotExist(err) {
		t.Fatal("superseded download retained", err)
	}
	// A stable Windows launch copy is not under updates. Its versioned source
	// must remain available for supervisor validation and subsequent launches.
	stable := filepath.Join(dir, "runtime", "control.exe")
	if err := SaveBinary(strings.NewReader(current.Version), stable, current.Asset); err != nil {
		t.Fatal(err)
	}
	if err := a.prune(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := Verify(current.Path, current.Asset); err != nil {
		t.Fatal("removed stable runtime source", err)
	}
}

func TestNodePrunePreservesFailedStartupRecovery(t *testing.T) {
	dir := t.TempDir()
	previous := nodeRetentionBinary(t, dir, "previous")
	candidate := nodeRetentionBinary(t, dir, "candidate")
	for name, binary := range map[string]nodeRuntime{"runtime.json": candidate, "runtime-previous.json": previous} {
		if err := store.Write(filepath.Join(dir, name), binary); err != nil {
			t.Fatal(err)
		}
	}
	// Even an equal version is insufficient when the selected checksum differs.
	a := &Updater{options: UpdaterOptions{Dir: dir, Software: buildinfo.Info{Version: candidate.Version, OS: previous.Asset.OS, Arch: previous.Asset.Arch, SHA256: previous.Asset.SHA256}}}
	if err := a.prune(t.Context()); err != nil {
		t.Fatal(err)
	}
	for _, binary := range []nodeRuntime{previous, candidate} {
		if err := Verify(binary.Path, binary.Asset); err != nil {
			t.Fatal("removed startup recovery binary", err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "runtime-previous.json")); err != nil {
		t.Fatal("removed startup recovery selection", err)
	}
}

func TestNodeUpdaterCleansAtStartupAndAfterOffers(t *testing.T) {
	dir := t.TempDir()
	current := nodeRetentionBinary(t, dir, "current")
	old := nodeRetentionBinary(t, dir, "old")
	if err := store.Write(filepath.Join(dir, "runtime.json"), current); err != nil {
		t.Fatal(err)
	}
	reports := make(chan Status, 8)
	a, err := NewUpdater(UpdaterOptions{
		Dir: dir, Software: buildinfo.Info{Version: current.Version, OS: current.Asset.OS, Arch: current.Asset.Arch, SHA256: current.Asset.SHA256},
		Busy: func() bool { return false }, Pause: func() bool { return true }, Resume: func() {},
		Report: func(_ context.Context, status Status) error { reports <- status; return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); a.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("updater did not stop")
		}
	})
	select {
	case <-reports:
	case <-time.After(5 * time.Second):
		t.Fatal("updater did not report startup")
	}
	if _, err := os.Stat(old.Path); !os.IsNotExist(err) {
		t.Fatal("startup retained obsolete binary", err)
	}
	obsolete := nodeRetentionBinary(t, dir, "obsolete")
	command := Command{ID: strings.Repeat("c", 64), Version: current.Version, Asset: current.Asset}
	data, err := json.Marshal(command)
	if err != nil {
		t.Fatal(err)
	}
	a.Receive("update.offer", data)
	wait := time.NewTimer(5 * time.Second)
	defer wait.Stop()
	for {
		select {
		case report := <-reports:
			if report.ID != command.ID {
				continue
			}
			if _, err := os.Stat(obsolete.Path); !os.IsNotExist(err) {
				t.Fatal("update offer retained obsolete binary", err)
			}
			return
		case <-wait.C:
			t.Fatal("updater did not process offer")
		}
	}
}

func TestNodePruneFailsClosed(t *testing.T) {
	for _, problem := range []string{"runtime.json", "runtime-request.json", "runtime-previous.json", "missing-current", "cancelled"} {
		t.Run(problem, func(t *testing.T) {
			dir := t.TempDir()
			current := nodeRetentionBinary(t, dir, "current")
			old := nodeRetentionBinary(t, dir, "old")
			if err := store.Write(filepath.Join(dir, "runtime.json"), current); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			switch problem {
			case "cancelled":
				cancel()
			case "missing-current":
				if err := os.Remove(current.Path); err != nil {
					t.Fatal(err)
				}
			default:
				if err := os.WriteFile(filepath.Join(dir, problem), []byte("{broken"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			a := &Updater{options: UpdaterOptions{Dir: dir, Software: buildinfo.Info{Version: current.Version, OS: current.Asset.OS, Arch: current.Asset.Arch, SHA256: current.Asset.SHA256}}}
			if err := a.prune(ctx); err == nil {
				t.Fatal("unsafe cleanup succeeded")
			}
			if err := Verify(old.Path, old.Asset); err != nil {
				t.Fatal("unsafe cleanup removed binary", err)
			}
		})
	}
}

func TestNodePruneCannotFollowLinksOutsideUpdateStorage(t *testing.T) {
	dir := t.TempDir()
	old := nodeRetentionBinary(t, dir, "old")
	external := t.TempDir()
	path := filepath.Join(external, "keep")
	if err := os.WriteFile(path, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(external, filepath.Join(filepath.Dir(old.Path), "external")); err != nil {
		t.Skip("symlinks unavailable", err)
	}
	linked := filepath.Join(dir, "updates", strings.Repeat("a", 64))
	if err := os.Symlink(external, linked); err != nil {
		t.Fatal(err)
	}
	a := &Updater{options: UpdaterOptions{Dir: dir}}
	if err := a.prune(t.Context()); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != "keep" {
		t.Fatalf("cleanup escaped update storage: %q, %v", data, err)
	}
	if _, err := os.Lstat(linked); err != nil {
		t.Fatal("removed symlink entry", err)
	}
}
