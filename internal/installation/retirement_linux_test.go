package installation

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRetiredSystemdHelperSELinux(t *testing.T) {
	for _, scenario := range []string{"copied", "reused", "permissive", "disabled", "label failure", "missing tool", "invalid mount", "cancelled", "user", "auto"} {
		t.Run(scenario, func(t *testing.T) {
			root := t.TempDir()
			calls := filepath.Join(root, "calls")
			label := filepath.Join(root, "labeled")
			helperRan := filepath.Join(root, "helper-ran")
			t.Setenv("PATH", root)
			t.Setenv("FIXTURE_CALLS", calls)
			t.Setenv("FIXTURE_LABEL", label)
			t.Setenv("FIXTURE_HELPER_RAN", helperRan)
			// These stand-ins only record arguments. No systemd unit, SELinux
			// label, or retirement cleanup is created or executed by the test.
			chcon := "#!/bin/sh\n{ printf 'chcon\\n'; printf '%s\\n' \"$@\"; } >> \"$FIXTURE_CALLS\"\n"
			if scenario == "label failure" {
				chcon += "exit 1\n"
			} else {
				chcon += ": > \"$FIXTURE_LABEL\"\n"
			}
			if scenario != "missing tool" {
				if err := os.WriteFile(filepath.Join(root, "chcon"), []byte(chcon), 0700); err != nil {
					t.Fatal(err)
				}
			}
			systemd := "#!/bin/sh\n{ printf 'systemd-run\\n'; printf '%s\\n' \"$@\"; } >> \"$FIXTURE_CALLS\"\n"
			needsLabel := scenario == "copied" || scenario == "reused" || scenario == "permissive"
			if needsLabel {
				systemd += "test -f \"$FIXTURE_LABEL\" || exit 2\n"
			}
			if err := os.WriteFile(filepath.Join(root, "systemd-run"), []byte(systemd), 0700); err != nil {
				t.Fatal(err)
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
			body := []byte("#!/bin/sh\n: > \"$FIXTURE_HELPER_RAN\"\nexit 99\n")
			source := filepath.Join(root, "source")
			if err := os.WriteFile(source, body, 0700); err != nil {
				t.Fatal(err)
			}
			sum := sha256.Sum256(body)
			digest := hex.EncodeToString(sum[:])
			data := filepath.Join(root, "state with spaces")
			helper := filepath.Join(data, "uninstall-"+digest, "control")
			if err := copyUninstallHelper(source, helper, digest); err != nil {
				t.Fatal(err)
			}
			if scenario == "reused" {
				before, err := os.Stat(helper)
				if err != nil {
					t.Fatal(err)
				}
				if err := copyUninstallHelper(source, helper, digest); err != nil {
					t.Fatal(err)
				}
				after, err := os.Stat(helper)
				if err != nil || !os.SameFile(before, after) || !before.ModTime().Equal(after.ModTime()) {
					t.Fatal("verified helper was replaced rather than reused", err)
				}
			}
			p := installedNode{Config: filepath.Join(root, "node.json"), ID: "retired-id", Mode: "system"}
			flags := []string{}
			if scenario == "user" || scenario == "auto" {
				p.Mode = scenario
				flags = []string{"--user"}
			}
			args := []string{"__uninstall", p.Config, data, p.ID}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if scenario == "cancelled" {
				cancel()
			}
			err := launchRetiredSystemdUninstall(ctx, helper, args, p, flags, selinux)
			wantError := scenario == "label failure" || scenario == "missing tool" || scenario == "invalid mount" || scenario == "cancelled"
			if (err != nil) != wantError {
				t.Fatalf("launch error = %v, want error %t", err, wantError)
			}
			if wantError && !strings.Contains(err.Error(), "prepare retirement cleanup helper") {
				t.Fatal("label failure lost retirement context", err)
			}
			if scenario == "cancelled" && !errors.Is(err, context.Canceled) {
				t.Fatal("cancellation cause was lost", err)
			}
			want := ""
			if needsLabel || scenario == "label failure" {
				want = "chcon\n-t\nbin_t\n--\n" + helper + "\n"
			}
			if !wantError {
				launch := append(flags, "--collect", "--unit=control-"+p.ID+"-uninstall-"+digest, "--property=Type=exec", "--", helper)
				launch = append(launch, args...)
				want += "systemd-run\n" + strings.Join(launch, "\n") + "\n"
			}
			got, readErr := os.ReadFile(calls)
			if want == "" {
				if !os.IsNotExist(readErr) {
					t.Fatal("unexpected command after rejected launch", string(got), readErr)
				}
			} else if readErr != nil || string(got) != want {
				t.Fatalf("commands = %q, want %q: %v", got, want, readErr)
			}
			if _, err := os.Stat(helperRan); !os.IsNotExist(err) {
				t.Fatal("test executed a cleanup helper", err)
			}
			gotBody, err := os.ReadFile(helper)
			if err != nil || string(gotBody) != string(body) {
				t.Fatal("helper contents changed", err)
			}
		})
	}
}
