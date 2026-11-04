package installation

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSystemdSystemStartup(t *testing.T) {
	unit := systemdSystemUnit("/usr/local/bin/control", "/var/lib/control profile/node.json")
	for _, want := range []string{`ExecStart="/usr/local/bin/control" node --config "/var/lib/control profile/node.json"`, "WantedBy=multi-user.target", "Restart=on-failure", "UMask=0077", "Environment=CONTROL_TOKEN="} {
		if !strings.Contains(unit, want) {
			t.Fatalf("system unit missing %q: %s", want, unit)
		}
	}
	if strings.Contains(unit, "default.target") || strings.Contains(unit, "--user") {
		t.Fatal("system unit depends on a login session")
	}
}

func TestSystemServiceSELinuxLabel(t *testing.T) {
	for _, scenario := range []string{"disabled", "enforcing", "permissive", "failure", "missing tool", "invalid mount"} {
		t.Run(scenario, func(t *testing.T) {
			root := t.TempDir()
			bin := filepath.Join(root, "control launcher")
			calls := filepath.Join(root, "calls")
			t.Setenv("PATH", root)
			t.Setenv("FIXTURE_CALLS", calls)
			if scenario != "missing tool" {
				body := "#!/bin/sh\nprintf '%s\\n' \"$@\" >> \"$FIXTURE_CALLS\"\n"
				if scenario == "failure" {
					body += "exit 1\n"
				}
				if err := os.WriteFile(filepath.Join(root, "chcon"), []byte(body), 0700); err != nil {
					t.Fatal(err)
				}
			}
			selinux := filepath.Join(root, "selinux")
			if scenario == "invalid mount" {
				if err := os.WriteFile(selinux, nil, 0600); err != nil {
					t.Fatal(err)
				}
			} else if scenario != "disabled" {
				if err := os.Mkdir(selinux, 0700); err != nil {
					t.Fatal(err)
				}
				mode := "1"
				if scenario == "permissive" {
					mode = "0"
				}
				if err := os.WriteFile(filepath.Join(selinux, "enforce"), []byte(mode), 0600); err != nil {
					t.Fatal(err)
				}
			}
			err := labelSystemServiceBinary(context.Background(), bin, selinux)
			wantError := scenario == "failure" || scenario == "missing tool" || scenario == "invalid mount"
			if (err != nil) != wantError {
				t.Fatalf("label error = %v, want error %t", err, wantError)
			}
			got, readErr := os.ReadFile(calls)
			if scenario == "disabled" || scenario == "missing tool" || scenario == "invalid mount" {
				if !os.IsNotExist(readErr) {
					t.Fatal("unexpected label command", string(got), readErr)
				}
			} else if readErr != nil || string(got) != "-t\nbin_t\n--\n"+bin+"\n" {
				t.Fatal("label did not target only the launcher", string(got), readErr)
			}
		})
	}
}
