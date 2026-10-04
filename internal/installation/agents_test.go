package installation

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pelletier/go-toml/v2"
	"github.com/tailscale/hujson"
)

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
