package installation

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/koltyakov/control/internal/identity"
	"github.com/koltyakov/control/internal/node"
	"github.com/koltyakov/control/internal/store"
)

func TestUpdateStartupBindsExistingIdentityWithoutCreatingOne(t *testing.T) {
	dir := t.TempDir()
	config := filepath.Join(dir, "node.json")
	cfg := node.Config{Name: "fixture", DataDir: filepath.Join(dir, "state"), WorkDir: filepath.Join(dir, "work")}
	if err := store.Write(config, cfg); err != nil {
		t.Fatal(err)
	}
	id, err := identity.Load(cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	p := installedNode{Config: config, ID: id.ID, Binary: filepath.Join(dir, "control"), Mode: "user"}
	if err = store.Write(installationRecord(cfg), p); err != nil {
		t.Fatal(err)
	}
	got, err := ReadUpdateStartup(config, cfg.DataDir)
	if err != nil || got.ID != id.ID || got.Binary != p.Binary {
		t.Fatalf("installation binding: %+v, %v", got, err)
	}
	if _, err = ReadUpdateStartup(config, dir); err == nil {
		t.Fatal("foreign state directory accepted")
	}
	if err = os.Remove(filepath.Join(cfg.DataDir, "identity.key")); err != nil {
		t.Fatal(err)
	}
	if _, err = ReadUpdateStartup(config, cfg.DataDir); err == nil {
		t.Fatal("missing identity accepted")
	}
	if _, err = os.Stat(filepath.Join(cfg.DataDir, "identity.key")); !os.IsNotExist(err) {
		t.Fatal("migration preflight created a replacement identity")
	}
}
