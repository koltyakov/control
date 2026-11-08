package node

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConfigRejectsLegacyRPA(t *testing.T) {
	for _, input := range []string{
		`{"rpa":{"command":"python3","args":["provider.py"]}}`,
		`{"rpa":{}}`,
		`{"rpa":null}`,
		`{"RPA":false}`,
		`{"RpA":"private-legacy-setting"}`,
		`{"rpa":{},"rpa":null,"desktop":{"library":"/native/library"}}`,
	} {
		t.Run(input, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "node.json")
			if err := os.WriteFile(path, []byte(input), 0600); err != nil {
				t.Fatal(err)
			}
			var cfg Config
			for _, err := range []error{json.Unmarshal([]byte(input), &cfg), func() error {
				_, err := LoadConfig(path)
				return err
			}()} {
				if err == nil {
					t.Fatal("legacy rpa configuration was silently accepted")
				}
				for _, text := range []string{"remove the rpa field", "desktop.library", "desktop.run/control_desktop", "no action batches, screenshot artifacts, or physical input", "docs/desktop.md#migrate-from-rpa"} {
					if !strings.Contains(err.Error(), text) {
						t.Errorf("migration error omits %q: %v", text, err)
					}
				}
				if strings.Contains(err.Error(), "private-legacy-setting") {
					t.Fatal("migration error exposed configuration contents")
				}
			}
		})
	}
}

func TestConfigDesktopRoundTrip(t *testing.T) {
	library := filepath.Join(t.TempDir(), "native-library")
	input, err := json.Marshal(Config{Name: "worker", Desktop: &DesktopConfig{Library: library}})
	if err != nil {
		t.Fatal(err)
	}
	var cfg Config
	if err := json.Unmarshal(input, &cfg); err != nil || cfg.Desktop == nil || cfg.Desktop.Library != library || cfg.Name != "worker" {
		t.Fatal("native desktop configuration did not round-trip", err)
	}
	if err := json.Unmarshal([]byte(`{"futureSetting":true}`), &cfg); err != nil {
		t.Fatal("unrelated unknown settings must remain tolerated", err)
	}
}

func TestRemovedRPANotRegistered(t *testing.T) {
	n := desktopNode(t, "echo")
	if _, exists := n.providers["rpa.run"]; exists {
		t.Fatal("removed RPA capability is still registered")
	}
	if err := n.Call(t.Context(), "", "rpa.run", map[string]any{}, nil); err == nil {
		t.Fatal("removed RPA capability was callable")
	}
}
