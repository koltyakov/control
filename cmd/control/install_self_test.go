package main

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/koltyakov/control/internal/installation"
	"github.com/koltyakov/control/skills"
)

func TestInstallSelfInstallsOpenCodeSkill(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("install-self changes the saved Windows user PATH")
	}
	for _, existing := range []string{"", "<!-- Managed by control -->\nold skill", "user-owned skill"} {
		t.Run(existing, func(t *testing.T) {
			root := t.TempDir()
			t.Setenv("CONTROL_INSTALL_DIR", filepath.Join(root, "bin"))
			t.Setenv("CONTROL_HOME", filepath.Join(root, "control"))
			t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
			// Installation must not depend on a valid local node profile.
			t.Setenv("CONTROL_CONFIG", root)
			paths, err := installation.AgentLocations("opencode", root, "")
			if err != nil {
				t.Fatal(err)
			}
			if existing != "" {
				if err := os.MkdirAll(filepath.Dir(paths.Skill), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(paths.Skill, []byte(existing), 0600); err != nil {
					t.Fatal(err)
				}
			}
			err = run(t.Context(), []string{"install-self"})
			want := skills.Control
			if existing == "user-owned skill" {
				if err == nil || !strings.Contains(err.Error(), "not managed by control") {
					t.Fatalf("unmanaged skill conflict not reported: %v", err)
				}
				want = []byte(existing)
			} else if err != nil {
				t.Fatal(err)
			}
			content, err := os.ReadFile(paths.Skill)
			if err != nil || !bytes.Equal(content, want) {
				t.Fatalf("skill content: %q, error: %v", content, err)
			}
			binary, err := installation.BinPath()
			if err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(binary); err != nil {
				t.Fatal("CLI not installed", err)
			}
			if _, err := os.Stat(paths.MCP); !os.IsNotExist(err) {
				t.Fatal("skill-only installation created MCP configuration", err)
			}
		})
	}
}
