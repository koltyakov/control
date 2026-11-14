package secrets

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/koltyakov/control/internal/testutil"
)

func TestWorkspaceRejectsWindowsJunctions(t *testing.T) {
	for _, entry := range []string{"store", "ancestor", "workspace-alias"} {
		t.Run(entry, func(t *testing.T) {
			work, base := t.TempDir(), t.TempDir()
			physical := filepath.Join(work, "state", "secrets")
			if err := os.MkdirAll(physical, 0700); err != nil {
				t.Fatal(err)
			}
			s := Store{WorkDir: work}
			link := filepath.Join(base, "alias")
			switch entry {
			case "store":
				testutil.Junction(t, physical, link)
				s.Dir = link
			case "ancestor":
				testutil.Junction(t, work, link)
				s.Dir = filepath.Join(link, "state", "secrets")
			case "workspace-alias":
				testutil.Junction(t, work, link)
				s.Dir, s.WorkDir = physical, link
			}
			if err := s.Set(t.Context(), "fixture", "not-a-real-secret"); err == nil {
				t.Fatal("credentials stored in workspace through a junction")
			}
			for _, name := range []string{"values.json", "secrets.lock"} {
				if _, err := os.Stat(filepath.Join(physical, name)); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("rejected store created private files in workspace", name, err)
				}
			}
		})
	}
}
