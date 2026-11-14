package upload

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/koltyakov/control/internal/testutil"
)

func TestUploadPrivateWindowsJunctionAliases(t *testing.T) {
	m := fixture(t)
	staging := filepath.Join(m.workDir, StagingDir)
	if err := os.MkdirAll(staging, 0700); err != nil {
		t.Fatal(err)
	}
	for _, entry := range []struct{ name, target string }{
		{"staging-alias", staging}, {"metadata-alias", m.dir},
	} {
		t.Run(entry.name, func(t *testing.T) {
			testutil.Junction(t, entry.target, filepath.Join(m.workDir, entry.name))
			for _, suffix := range []string{"", "/missing", "/missing/child"} {
				if err := m.CheckPath(entry.name + suffix); err == nil {
					t.Fatal("junction exposed private upload storage", suffix)
				}
			}
		})
	}
}
