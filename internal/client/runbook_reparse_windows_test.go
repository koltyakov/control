package client

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/koltyakov/control/internal/testutil"
)

func TestScenarioRenameRejectsWindowsReparsePoints(t *testing.T) {
	for _, kind := range []string{"junction", "reparse-file"} {
		for _, entry := range []string{"root", "source", "nested", "destination", "lock"} {
			t.Run(kind+"/"+entry, func(t *testing.T) {
				root := filepath.Join(t.TempDir(), "runbooks")
				original := filepath.Join(root, "old", "run.md")
				const content = "Machine: `old`\n"
				writeScenarioFixture(t, original, content)
				outside := t.TempDir()
				keep := filepath.Join(outside, "keep.md")
				writeScenarioFixture(t, keep, content)
				link := map[string]string{
					"root": root, "source": filepath.Join(root, "old"),
					"nested":      filepath.Join(root, "old", "link"),
					"destination": filepath.Join(root, "trader"), "lock": filepath.Join(root, ".rename.lock"),
				}[entry]
				if entry == "root" || entry == "source" {
					if err := os.Rename(link, link+"-original"); err != nil {
						t.Fatal(err)
					}
					if entry == "root" {
						original = filepath.Join(link+"-original", "old", "run.md")
					} else {
						original = filepath.Join(link+"-original", "run.md")
					}
				}
				if kind == "junction" {
					testutil.Junction(t, outside, link)
				} else {
					testutil.ReparseFile(t, link)
				}
				c, calls := scenarioRenameServer(t, "old", http.StatusNoContent, nil)
				if err := c.renameMachine(t.Context(), "stable-id", "trader", root); err == nil || calls.Load() != 0 {
					t.Fatal("reparse entry did not block remote rename", err, calls.Load())
				}
				for _, path := range []string{original, keep} {
					if data, err := os.ReadFile(path); err != nil || string(data) != content {
						t.Fatal("rejected rename changed protected contents", path, err)
					}
				}
				if _, err := os.Lstat(link); err != nil {
					t.Fatal("rejected rename removed the reparse entry", err)
				}
			})
		}
	}
}
