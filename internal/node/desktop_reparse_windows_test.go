package node

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/koltyakov/control/internal/testutil"
)

func TestDesktopRejectsWindowsReparsePoints(t *testing.T) {
	for _, entry := range []string{"state-junction", "ancestor-junction", "state-reparse-file"} {
		t.Run(entry, func(t *testing.T) {
			n := &Node{Config: Config{DataDir: t.TempDir(), WorkDir: t.TempDir()}}
			home, err := n.desktopStateDir()
			if err != nil {
				t.Fatal("ordinary private state rejected", err)
			}
			if err := os.Remove(home); err != nil {
				t.Fatal(err)
			}
			sentinel := filepath.Join(n.Config.WorkDir, "keep")
			if err := os.WriteFile(sentinel, []byte("untouched"), 0600); err != nil {
				t.Fatal(err)
			}
			switch entry {
			case "state-junction":
				testutil.Junction(t, n.Config.WorkDir, home)
			case "ancestor-junction":
				alias := filepath.Join(t.TempDir(), "data")
				testutil.Junction(t, n.Config.WorkDir, alias)
				n.Config.DataDir = alias
			case "state-reparse-file":
				testutil.ReparseFile(t, home)
			}
			if _, err := n.desktopStateDir(); err == nil {
				t.Fatal("reparse point accepted as private desktop state")
			}
			if data, err := os.ReadFile(sentinel); err != nil || string(data) != "untouched" {
				t.Fatal("workspace sentinel changed", err)
			}
		})
	}
}
