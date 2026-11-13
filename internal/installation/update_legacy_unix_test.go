//go:build !windows

package installation

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/koltyakov/control/internal/update"
)

func unixRefreshFiles(t *testing.T) (string, update.RuntimeBinary) {
	t.Helper()
	dir := t.TempDir()
	binary, source := filepath.Join(dir, "control"), filepath.Join(dir, "selected")
	if err := os.WriteFile(binary, []byte("old launchd supervisor"), 0750); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(binary, 0750); err != nil {
		t.Fatal(err)
	}
	next := []byte("verified direct runtime")
	if err := os.WriteFile(source, next, 0700); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(next)
	return binary, update.RuntimeBinary{Path: source, Version: "new", Asset: update.Asset{Size: int64(len(next)), SHA256: hex.EncodeToString(sum[:])}}
}

func TestRefreshLegacyUnixBinaryPreservesOriginalAndMode(t *testing.T) {
	binary, selected := unixRefreshFiles(t)
	original, err := os.ReadFile(binary)
	if err != nil {
		t.Fatal(err)
	}
	checks := 0
	if err := refreshLegacyUnixBinary(context.Background(), binary, selected, func() error { checks++; return nil }); err != nil {
		t.Fatal(err)
	}
	if checks != 1 {
		t.Fatalf("expected final identity/parent recheck, got %d", checks)
	}
	if err := update.Verify(binary, selected.Asset); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(binary)
	if err != nil || info.Mode().Perm() != 0750 {
		t.Fatal("startup executable permissions changed", err)
	}
	backups, err := filepath.Glob(binary + ".before-single-process-*")
	if err != nil || len(backups) != 1 {
		t.Fatal("original startup executable not retained", err)
	}
	backup, err := os.ReadFile(backups[0])
	if err != nil || !bytes.Equal(original, backup) {
		t.Fatal("original executable backup changed", err)
	}
	if err := update.Verify(selected.Path, selected.Asset); err != nil {
		t.Fatal("migration changed persisted runtime source", err)
	}
	staging, _ := filepath.Glob(filepath.Join(filepath.Dir(binary), ".control-startup-*"))
	if len(staging) != 0 {
		t.Fatal("temporary startup staging retained")
	}
}

func TestRefreshLegacyUnixBinaryRefusesUnsafeChanges(t *testing.T) {
	for _, scenario := range []string{"cancel", "identity", "source", "symlink", "shared-write", "replaced", "permissions"} {
		t.Run(scenario, func(t *testing.T) {
			binary, selected := unixRefreshFiles(t)
			before, _ := os.ReadFile(binary)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			recheck := func() error { return nil }
			switch scenario {
			case "cancel":
				cancel()
			case "identity":
				recheck = func() error { return errors.New("identity changed") }
			case "source":
				selected.Asset.SHA256 = strings.Repeat("0", 64)
			case "symlink":
				if err := os.Rename(binary, binary+".real"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(binary+".real", binary); err != nil {
					t.Fatal(err)
				}
			case "shared-write":
				if err := os.Chmod(binary, 0770); err != nil {
					t.Fatal(err)
				}
			case "replaced":
				recheck = func() error {
					if err := os.Rename(binary, binary+".other"); err != nil {
						return err
					}
					return os.WriteFile(binary, before, 0750)
				}
			case "permissions":
				recheck = func() error { return os.Chmod(binary, 0755) }
			}
			if err := refreshLegacyUnixBinary(ctx, binary, selected, recheck); err == nil {
				t.Fatal("unsafe startup replacement succeeded")
			}
			after, err := os.ReadFile(binary)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("failed migration replaced the startup executable", err)
			}
			if scenario == "permissions" {
				info, err := os.Stat(binary)
				if err != nil || info.Mode().Perm() != 0755 {
					t.Fatal("migration discarded a concurrent permission change", err)
				}
			}
		})
	}
}
