package installation

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/gofrs/flock"
	"github.com/koltyakov/control/internal/identity"
	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/node"
	"github.com/koltyakov/control/internal/store"
)

func retirementFixture(t *testing.T) (installedNode, node.Config) {
	t.Helper()
	home := t.TempDir()
	config := filepath.Join(home, "node.json")
	cfg := node.Config{Name: "retired", Gateway: "https://gateway.example", Token: "node-key", DataDir: filepath.Join(home, "state"), WorkDir: filepath.Join(home, "work")}
	if err := store.Write(config, cfg); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(home, "control")
	if err := os.WriteFile(binary, []byte("test executable"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := rememberInstallation(context.Background(), binary, config, "process", cfg); err != nil {
		t.Fatal(err)
	}
	var p installedNode
	if err := store.Read(installationRecord(cfg), &p); err != nil {
		t.Fatal(err)
	}
	if err := store.Write(filepath.Join(cfg.DataDir, "machine-state.json"), model.MachineState{Revision: 2, Unregistered: true}); err != nil {
		t.Fatal(err)
	}
	return p, cfg
}

func TestRetiredUninstallPreservesDataAndIsRepeatable(t *testing.T) {
	p, cfg := retirementFixture(t)
	work := filepath.Join(cfg.WorkDir, "keep.txt")
	if err := store.Bytes(work, []byte("user work"), 0600); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := UninstallRetired(context.Background(), p.Config, cfg.DataDir, p.ID); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(p.Binary); !os.IsNotExist(err) {
		t.Fatal("retired launcher retained", err)
	}
	for _, path := range []string{p.Config, work, filepath.Join(cfg.DataDir, "identity.key"), filepath.Join(cfg.DataDir, "machine-state.json")} {
		if _, err := os.Stat(path); err != nil {
			t.Fatal("retirement removed retained files", path, err)
		}
	}
}

func TestRetiredUninstallRequiresIdentityAndPolicy(t *testing.T) {
	for _, kind := range []string{"wrong identity", "enabled", "disabled", "missing key"} {
		t.Run(kind, func(t *testing.T) {
			p, cfg := retirementFixture(t)
			id := p.ID
			if kind == "wrong identity" {
				id = "other-identity"
			} else if kind == "missing key" {
				if err := os.Remove(filepath.Join(cfg.DataDir, "identity.key")); err != nil {
					t.Fatal(err)
				}
			} else if err := store.Write(filepath.Join(cfg.DataDir, "machine-state.json"), model.MachineState{Revision: 3, Disabled: kind == "disabled"}); err != nil {
				t.Fatal(err)
			}
			if err := UninstallRetired(context.Background(), p.Config, cfg.DataDir, id); err == nil {
				t.Fatal("uninstall accepted a non-retired identity")
			}
			if _, err := os.Stat(p.Binary); err != nil {
				t.Fatal("rejected uninstall removed the launcher", err)
			}
			if kind == "missing key" {
				if _, err := os.Stat(filepath.Join(cfg.DataDir, "identity.key")); !os.IsNotExist(err) {
					t.Fatal("cleanup recreated a missing identity", err)
				}
			}
		})
	}
}

func TestRetiredUninstallWaitsForShutdown(t *testing.T) {
	for _, name := range []string{"runtime.lock", "node.lock"} {
		t.Run(name, func(t *testing.T) {
			p, cfg := retirementFixture(t)
			lock := flock.New(filepath.Join(cfg.DataDir, name))
			defer func() { _ = lock.Close() }()
			if err := lock.Lock(); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
			defer cancel()
			if err := UninstallRetired(ctx, p.Config, cfg.DataDir, p.ID); !errors.Is(err, context.DeadlineExceeded) {
				t.Fatal("uninstall did not wait for shutdown", err)
			}
			if _, err := os.Stat(p.Binary); err != nil {
				t.Fatal("running launcher removed", err)
			}
		})
	}
}

func TestRetiredUninstallDoesNotRemoveReplacementOrSharedBinary(t *testing.T) {
	t.Run("replacement", func(t *testing.T) {
		p, cfg := retirementFixture(t)
		oldDir := cfg.DataDir
		cfg.DataDir = filepath.Join(t.TempDir(), "new-state")
		if _, err := identity.Load(cfg.DataDir); err != nil {
			t.Fatal(err)
		}
		if err := store.Write(p.Config, cfg); err != nil {
			t.Fatal(err)
		}
		if err := UninstallRetired(context.Background(), p.Config, oldDir, p.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(p.Binary); err != nil {
			t.Fatal("replacement launcher removed", err)
		}
	})
	t.Run("shared binary", func(t *testing.T) {
		p, cfg := retirementFixture(t)
		other := p
		other.Config = filepath.Join(t.TempDir(), "node.json")
		if err := store.Write(binaryRegistration(other), other); err != nil {
			t.Fatal(err)
		}
		if err := UninstallRetired(context.Background(), p.Config, cfg.DataDir, p.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(p.Binary); err != nil {
			t.Fatal("shared launcher removed", err)
		}
		if _, err := os.Stat(binaryRegistration(other)); err != nil {
			t.Fatal("other registration removed", err)
		}
	})
}

func TestBinaryRegistryRespectsPlatformPathCase(t *testing.T) {
	p, cfg := retirementFixture(t)
	other := p
	other.Config = filepath.Join(filepath.Dir(p.Config), "Node.json")
	if runtime.GOOS == "windows" {
		if binaryRegistration(other) != binaryRegistration(p) {
			t.Fatal("case-only Windows aliases produced separate registrations")
		}
		return
	}
	if binaryRegistration(other) == binaryRegistration(p) {
		t.Fatal("case-sensitive profiles shared a registration")
	}
	if err := store.Write(binaryRegistration(other), other); err != nil {
		t.Fatal(err)
	}
	if err := UninstallRetired(context.Background(), p.Config, cfg.DataDir, p.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p.Binary); err != nil {
		t.Fatal("shared launcher removed through a case-only profile collision", err)
	}
}
