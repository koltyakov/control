package runbooks

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/koltyakov/control/internal/testutil"
)

func TestRunbooksRejectWindowsReparsePoints(t *testing.T) {
	for _, entry := range []string{"parent-junction", "root-junction", "directory-junction", "reparse-file"} {
		t.Run(entry, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "control", "runbooks")
			writeBook(t, root, "shared/run.md", "safe")
			outside := t.TempDir()
			link, targetFile := root, "shared/run.md"
			switch entry {
			case "parent-junction":
				link, targetFile = filepath.Dir(root), "runbooks/shared/run.md"
			case "directory-junction":
				link, targetFile = filepath.Join(root, "shared"), "run.md"
			case "reparse-file":
				link = filepath.Join(root, "shared", "run.md")
			}
			writeBook(t, outside, targetFile, "outside content must not be returned")
			if err := os.Rename(link, link+"-original"); err != nil {
				t.Fatal(err)
			}
			if entry == "reparse-file" {
				testutil.ReparseFile(t, link)
			} else {
				testutil.Junction(t, outside, link)
			}
			for _, query := range []Query{{}, {Source: "project", File: "shared/run.md"}} {
				catalog, err := Discover(t.Context(), []Root{{Source: "project", Path: root}}, Selector{}, query)
				rejected := err != nil
				if query.File == "" {
					rejected = err == nil && len(catalog.Sources) == 1 && catalog.Sources[0].Error != ""
				}
				if !rejected || len(catalog.Runbooks) != 0 {
					t.Fatalf("reparse point read by query %+v: %+v, %v", query, catalog, err)
				}
			}
			if data, err := os.ReadFile(filepath.Join(outside, filepath.FromSlash(targetFile))); err != nil || string(data) != "outside content must not be returned" {
				t.Fatal("discovery changed outside contents", err)
			}
		})
	}
}
