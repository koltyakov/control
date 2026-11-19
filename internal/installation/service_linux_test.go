package installation

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLinuxUserServiceProfileOwnership(t *testing.T) {
	for _, tt := range []struct {
		name      string
		operation string
		unit      string
		conflict  bool
	}{
		{name: "start refuses another profile", operation: "start", unit: "other", conflict: true},
		{name: "uninstall refuses another profile", operation: "uninstall", unit: "other", conflict: true},
		{name: "uninstall preserves unmanaged unit", operation: "uninstall", unit: "unmanaged", conflict: true},
		{name: "uninstall ignores profile in comment", operation: "uninstall", unit: "comment", conflict: true},
		{name: "uninstall rejects extra arguments", operation: "uninstall", unit: "extra", conflict: true},
		{name: "start refreshes launcher for same profile", operation: "start", unit: "owned"},
		{name: "uninstall removes owned unit", operation: "uninstall", unit: "owned"},
		{name: "start creates missing unit", operation: "start"},
		{name: "uninstall accepts missing unit", operation: "uninstall"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "xdg"))
			t.Setenv("PATH", root)
			calls := filepath.Join(root, "calls")
			t.Setenv("FIXTURE_CALLS", calls)
			const systemctl = "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$FIXTURE_CALLS\"\n"
			if err := os.WriteFile(filepath.Join(root, "systemctl"), []byte(systemctl), 0700); err != nil {
				t.Fatal(err)
			}
			path, err := servicePath()
			if err != nil {
				t.Fatal(err)
			}
			profile := filepath.Join(root, `user %$ "profile"`, "node.json")
			other := filepath.Join(root, "another-profile", "node.json")
			oldBinary := filepath.Join(root, `old %$ "control"`)
			newBinary := filepath.Join(root, "new control")
			var before string
			switch tt.unit {
			case "owned":
				before = userSystemdUnit(oldBinary, profile)
			case "other":
				before = userSystemdUnit(oldBinary, other)
			case "comment":
				before = userSystemdUnit(oldBinary, other) + "# node --config " + systemdQuote(profile) + "\n"
			case "extra":
				before = strings.Replace(userSystemdUnit(oldBinary, profile), " node --config ", " node --config "+systemdQuote(other)+" node --config ", 1)
			case "unmanaged":
				before = strings.TrimPrefix(userSystemdUnit(oldBinary, profile), "# Managed by control\n")
			}
			if before != "" {
				if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
					t.Fatal(err)
				}
				if err = os.WriteFile(path, []byte(before), 0600); err != nil {
					t.Fatal(err)
				}
			}
			var wantUnit, wantCalls string
			if tt.operation == "start" {
				err = installService(t.Context(), newBinary, profile)
				wantUnit = userSystemdUnit(newBinary, profile)
				wantCalls = "--user show-environment\n--user daemon-reload\n--user enable --now control-node.service\n"
			} else {
				err = uninstallService(t.Context(), profile)
				if before != "" {
					wantCalls = "--user disable control-node.service\n--user daemon-reload\n"
				}
			}
			if tt.conflict {
				if err == nil {
					t.Fatal("wanted an ownership error")
				}
				wantUnit, wantCalls = before, ""
			} else if err != nil {
				t.Fatal(err)
			}
			checkServiceFixtureFile(t, path, wantUnit)
			checkServiceFixtureFile(t, calls, wantCalls)
		})
	}
}

func checkServiceFixtureFile(t *testing.T, path, want string) {
	t.Helper()
	got, err := os.ReadFile(path)
	if want == "" && errors.Is(err, os.ErrNotExist) {
		return
	}
	if err != nil || string(got) != want {
		t.Fatalf("%s = %q, want %q: %v", filepath.Base(path), got, want, err)
	}
}
