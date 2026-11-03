package enrollment

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/koltyakov/control/internal/update"
)

func TestHybridInstallers(t *testing.T) {
	ticket := strings.Repeat("a", 43)
	link := "https://gateway.example/install/" + ticket
	if SystemTicket(ticket) == ticket || len(SystemTicket(ticket)) != 43 || SystemTicket(ticket) == SystemTicket(strings.Repeat("b", 43)) {
		t.Fatal("companion tickets are not independently derived")
	}
	for _, platform := range []string{"linux", "darwin", "windows"} {
		for _, role := range []string{"user", "system"} {
			i := Invitation{ID: "primary", HybridID: "primary", HybridRole: role, ServiceMode: "hybrid", Asset: update.Asset{OS: platform, Arch: "amd64", SHA256: strings.Repeat("a", 64)}}
			if role == "system" {
				i.ServiceMode = "system"
			}
			script, err := Script(i, link)
			if err != nil {
				t.Fatal(err)
			}
			if role == "user" && (!strings.Contains(script, SystemTicket(ticket)) || !strings.Contains(script, "pending-enrollment.json")) {
				t.Fatal("Hybrid installer lost companion or recovery flow", script)
			}
			if role == "system" && !strings.Contains(script, "--hybrid-system") {
				t.Fatal("old installer could silently accept privileged enrollment")
			}
			if platform != "windows" && runtime.GOOS != "windows" {
				cmd := exec.Command("bash", "-n")
				cmd.Stdin = strings.NewReader(script)
				if output, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("invalid %s/%s Bash: %s %v", platform, role, output, err)
				}
			}
		}
	}
}

func TestHybridAutomaticNames(t *testing.T) {
	for _, role := range []string{"user", "system"} {
		i := Invitation{AutoName: true, HybridRole: role}
		got, err := i.ResolveName("worker")
		want := "worker"
		if role == "system" {
			want += "-system"
		}
		if err != nil || got != want {
			t.Fatal(got, err)
		}
		if _, err = i.ResolveName(strings.Repeat("a", 57)); err == nil {
			t.Fatal("Hybrid hostname left no space for System suffix")
		}
	}
}

func TestHybridBashReusesSystemBinary(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Bash fixture runs on Unix")
	}
	for _, platform := range []string{"linux", "darwin"} {
		for _, scenario := range []string{"fresh", "installed", "pending", "different", "corrupt-download"} {
			t.Run(platform+"/"+scenario, func(t *testing.T) {
				t.Parallel()
				root := t.TempDir()
				binary := `#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >> "$FIXTURE_CALLS"
if [[ " $* " == *' enroll '* ]]; then
  mkdir -p "$CONTROL_HOME" "$CONTROL_INSTALL_DIR"
  if [ "$0" != "$CONTROL_INSTALL_DIR/control" ]; then cp "$0" "$CONTROL_INSTALL_DIR/control"; fi
  touch "$CONTROL_HOME/node.json"
fi
`
				asset := filepath.Join(root, "asset")
				if err := os.WriteFile(asset, []byte(binary), 0700); err != nil {
					t.Fatal(err)
				}
				i := Invitation{ID: "fixture", HybridID: "fixture", HybridRole: "user", ServiceMode: "hybrid", Asset: update.Asset{OS: platform, Arch: "arm64", SHA256: Hash(binary)}}
				link := "https://gateway.example/install/ticket"
				userScript, err := Script(i, link)
				if err != nil {
					t.Fatal(err)
				}
				i.HybridRole, i.ServiceMode = "system", "system"
				systemScript, err := Script(i, "https://gateway.example/install/"+SystemTicket("ticket"))
				if err != nil {
					t.Fatal(err)
				}
				// Redirect privileged paths into temporary storage. The sudo fixture
				// only simulates the child script's root check, never OS elevation.
				replace := strings.NewReplacer("/var/lib/control", root+"/profiles", "/usr/local/lib/control", root+"/binaries", "/Library/Application Support/control", root+"/profiles", "/Library/PrivilegedHelperTools/Control", root+"/binaries")
				systemScriptPath := filepath.Join(root, "system.sh")
				if err = os.WriteFile(systemScriptPath, []byte(replace.Replace(systemScript)), 0600); err != nil {
					t.Fatal(err)
				}
				systemHome := filepath.Join(root, "profiles", "hybrid", "fixture")
				systemBin := filepath.Join(root, "binaries", "fixture", "control")
				if scenario != "fresh" {
					for _, dir := range []string{systemHome, filepath.Dir(systemBin)} {
						if err = os.MkdirAll(dir, 0700); err != nil {
							t.Fatal(err)
						}
					}
					installed := binary
					if scenario == "different" || scenario == "corrupt-download" {
						installed += "# older companion\n"
					}
					if err = os.WriteFile(systemBin, []byte(installed), 0700); err != nil {
						t.Fatal(err)
					}
					for _, file := range []string{"node.json", "pending-enrollment.json"} {
						if file == "pending-enrollment.json" && scenario != "pending" {
							continue
						}
						if err = os.WriteFile(filepath.Join(systemHome, file), nil, 0600); err != nil {
							t.Fatal(err)
						}
					}
				}
				harness := fmt.Sprintf(`
uname() { if [ "$1" = -s ]; then echo %s; else echo arm64; fi; }
sysctl() { echo 1; }
id() { echo "${FIXTURE_ROOT:-1000}"; }
sudo() { if [ "$1" = bash ]; then FIXTURE_ROOT=0 bash; else "$@"; fi; }
curl() {
  if [[ "$2" == *'/binary?arch=arm64' ]]; then
    printf '%%s\n' "$2" >> "$FIXTURE_DOWNLOADS"
    if [ "$FIXTURE_SCENARIO" = corrupt-download ]; then echo corrupt > "$4"; else cp "$FIXTURE_ASSET" "$4"; fi
  else
    cat "$FIXTURE_SYSTEM_SCRIPT"
  fi
}
export -f uname sysctl id sudo curl
`, map[string]string{"linux": "Linux", "darwin": "Darwin"}[platform])
				cmd := exec.CommandContext(t.Context(), "bash", "-c", harness+replace.Replace(userScript))
				calls, downloads := filepath.Join(root, "calls"), filepath.Join(root, "downloads")
				cmd.Env = append(os.Environ(), "CONTROL_HOME="+filepath.Join(root, "user"), "CONTROL_INSTALL_DIR="+filepath.Join(root, "user-bin"), "FIXTURE_CALLS="+calls, "FIXTURE_DOWNLOADS="+downloads, "FIXTURE_ASSET="+asset, "FIXTURE_SYSTEM_SCRIPT="+systemScriptPath, "FIXTURE_SCENARIO="+scenario)
				output, runErr := cmd.CombinedOutput()
				if scenario == "corrupt-download" {
					if runErr == nil || !strings.Contains(string(output), "Binary checksum mismatch") {
						t.Fatalf("corrupt fallback must fail verification: %v\n%s", runErr, output)
					}
				} else if runErr != nil {
					t.Fatalf("installer: %v\n%s", runErr, output)
				}
				gotDownloads, err := os.ReadFile(downloads)
				if err != nil && !os.IsNotExist(err) {
					t.Fatal(err)
				}
				wantDownloads := 0
				if scenario == "fresh" || scenario == "different" || scenario == "corrupt-download" {
					wantDownloads = 1
				}
				if got := strings.Count(string(gotDownloads), "/binary?"); got != wantDownloads {
					t.Fatalf("got %d downloads, want %d: %s", got, wantDownloads, gotDownloads)
				}
				gotCalls, err := os.ReadFile(calls)
				if err != nil {
					t.Fatal(err)
				}
				userCall := "enroll --url " + link + " --service user"
				if strings.Contains(string(gotCalls), userCall) != (scenario != "corrupt-download") {
					t.Fatalf("unexpected User enrollment: %s", gotCalls)
				}
				if scenario != "corrupt-download" {
					gotBinary, err := os.ReadFile(filepath.Join(root, "user-bin", "control"))
					if err != nil || string(gotBinary) != binary {
						t.Fatalf("User binary does not match invitation: %v", err)
					}
				}
			})
		}
	}
}
