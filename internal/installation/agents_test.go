package installation

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/koltyakov/control/skills"
	"github.com/pelletier/go-toml/v2"
	"github.com/tailscale/hujson"
)

func TestOpenCodeSkillInstall(t *testing.T) {
	for _, xdg := range []bool{false, true} {
		t.Run(map[bool]string{false: "default", true: "xdg"}[xdg], func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("USERPROFILE", home)
			t.Setenv("XDG_CONFIG_HOME", "")
			base := filepath.Join(home, ".config")
			if xdg {
				base = filepath.Join(home, "custom-config")
				t.Setenv("XDG_CONFIG_HOME", base)
			}
			path := filepath.Join(base, "opencode", "skills", "control", "SKILL.md")
			for range 2 {
				if err := InstallAgent(t.Context(), "opencode", "", "control", "node.json", false, true); err != nil {
					t.Fatal(err)
				}
				content, err := os.ReadFile(path)
				if err != nil || !bytes.Equal(content, skills.Control) {
					t.Fatalf("skill content: %q, error: %v", content, err)
				}
				assertSkillBundle(t, path)
			}
			if _, err := os.Stat(filepath.Join(base, "opencode", "opencode.json")); !os.IsNotExist(err) {
				t.Fatal("skill-only installation created MCP configuration", err)
			}
		})
	}
}

func assertSkillBundle(t *testing.T, path string) {
	t.Helper()
	err := fs.WalkDir(skills.ControlFiles, "control", func(name string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		want, err := skills.ControlFiles.ReadFile(name)
		if err != nil {
			return err
		}
		dest := filepath.Join(filepath.Dir(path), filepath.FromSlash(strings.TrimPrefix(name, "control/")))
		got, err := os.ReadFile(dest)
		if err != nil || !bytes.Equal(got, want) {
			t.Errorf("installed skill file %s differs: %v", dest, err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestSkillBundleInstallAllClients(t *testing.T) {
	for _, agent := range []string{"opencode", "claude", "cursor", "copilot", "codex", "windsurf", "antigravity", "agents"} {
		t.Run(agent, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("USERPROFILE", home)
			t.Setenv("XDG_CONFIG_HOME", "")
			for _, project := range []string{"", t.TempDir()} {
				paths, err := AgentLocations(agent, home, project)
				if err != nil {
					t.Fatal(err)
				}
				for range 2 {
					if err = InstallAgent(t.Context(), agent, project, "control", "node.json", false, true); err != nil {
						t.Fatal(err)
					}
					assertSkillBundle(t, paths.Skill)
				}
				if _, err = os.Stat(paths.MCP); !os.IsNotExist(err) {
					t.Fatal("skill-only installation created MCP configuration", err)
				}
			}
		})
	}
}

func TestSkillBundleManagedFiles(t *testing.T) {
	for _, conflict := range []string{"SKILL.md", "references/desktop.md"} {
		t.Run(conflict, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "SKILL.md")
			oldCore := []byte("<!-- Managed by control -->\nold self-contained skill\n")
			if err := os.WriteFile(path, oldCore, 0600); err != nil {
				t.Fatal(err)
			}
			userPath := filepath.Join(root, filepath.FromSlash(conflict))
			if err := os.MkdirAll(filepath.Dir(userPath), 0700); err != nil {
				t.Fatal(err)
			}
			userContent := []byte("user-authored instructions\n")
			if err := os.WriteFile(userPath, userContent, 0600); err != nil {
				t.Fatal(err)
			}
			err := installSkill(t.Context(), path)
			if err == nil || !strings.Contains(err.Error(), "not managed by control") {
				t.Fatalf("unmanaged conflict not reported: %v", err)
			}
			got, err := os.ReadFile(userPath)
			if err != nil || !bytes.Equal(got, userContent) {
				t.Fatal("unmanaged instructions changed", err)
			}
			if conflict != "SKILL.md" {
				got, err = os.ReadFile(path)
				if err != nil || !bytes.Equal(got, oldCore) {
					t.Fatal("core changed before reference conflict was checked", err)
				}
			}
			if _, err = os.Stat(filepath.Join(root, "references", "administration.md")); !os.IsNotExist(err) {
				t.Fatal("bundle was partially written despite unmanaged conflict", err)
			}
		})
	}

	t.Run("refresh", func(t *testing.T) {
		root := t.TempDir()
		path := filepath.Join(root, "SKILL.md")
		if err := installSkill(t.Context(), path); err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"SKILL.md", "references/desktop.md"} {
			if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(name)), []byte("<!-- Managed by control -->\nstale\n"), 0600); err != nil {
				t.Fatal(err)
			}
		}
		custom := filepath.Join(root, "references", "custom.md")
		if err := os.WriteFile(custom, []byte("user notes\n"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(filepath.Join(root, "references", "browser.md")); err != nil {
			t.Fatal(err)
		}
		if err := installSkill(t.Context(), path); err != nil {
			t.Fatal(err)
		}
		assertSkillBundle(t, path)
		if got, err := os.ReadFile(custom); err != nil || string(got) != "user notes\n" {
			t.Fatal("unrelated reference changed", err)
		}
	})
}

func TestSkillBundleCancelledInstall(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "control", "SKILL.md")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := installSkill(ctx, path); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled installation returned %v", err)
	}
	if _, err := os.Stat(filepath.Dir(path)); !os.IsNotExist(err) {
		t.Fatal("cancelled installation wrote skill files", err)
	}
}

func TestMCPInstallPreservesUnrelatedConfiguration(t *testing.T) {
	for _, agent := range []string{"opencode", "claude", "cursor", "copilot", "codex", "windsurf", "antigravity", "agents"} {
		t.Run(agent, func(t *testing.T) {
			root := t.TempDir()
			paths, err := AgentLocations(agent, root, root)
			if err != nil {
				t.Fatal(err)
			}
			if err = os.MkdirAll(filepath.Dir(paths.MCP), 0700); err != nil {
				t.Fatal(err)
			}
			input := `{"custom":true, /* retain my comment */ "unrelated":{"nested":42}}`
			if paths.TOML {
				input = "custom = true\n[unrelated]\nnested = 42\n"
			}
			if err = os.WriteFile(paths.MCP, []byte(input), 0600); err != nil {
				t.Fatal(err)
			}
			entry := map[string]any{"command": "/path with spaces/control", "args": []string{"--config", "/private/profile.json", "mcp"}}
			for range 2 {
				if err = mergeMCP(paths, entry); err != nil {
					t.Fatal(err)
				}
			}
			b, err := os.ReadFile(paths.MCP)
			if err != nil {
				t.Fatal(err)
			}
			var output map[string]any
			if paths.TOML {
				err = toml.Unmarshal(b, &output)
			} else {
				if !strings.Contains(string(b), "retain my comment") {
					t.Fatal("JSONC comment removed")
				}
				var clean []byte
				clean, err = hujson.Standardize(b)
				if err == nil {
					err = json.Unmarshal(clean, &output)
				}
			}
			if err != nil || output["custom"] != true || output["unrelated"] == nil {
				t.Fatalf("unrelated config changed: %s %v", b, err)
			}
			backup, err := os.ReadFile(paths.MCP + ".control-backup")
			if err != nil || string(backup) != input {
				t.Fatal("original backup missing")
			}
		})
	}
}
