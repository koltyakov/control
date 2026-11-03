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

func TestBashInstallersReuseLocalBinary(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Bash fixture runs on Unix")
	}
	for _, platform := range []string{"linux", "darwin"} {
		for _, installer := range []string{"user", "system", "hybrid-system"} {
			for _, location := range []string{"cwd", "asset", "bin", "bin-asset", "install", "default-install", "path", "missing", "stale", "corrupt-download"} {
				if location == "default-install" && (installer == "system" || installer == "hybrid-system") {
					continue // System defaults use protected paths, not the user's bin.
				}
				t.Run(platform+"/"+installer+"/"+location, func(t *testing.T) {
					t.Parallel()
					root := t.TempDir()
					cwd, home := filepath.Join(root, "working folder"), filepath.Join(root, "home")
					installDir, pathDir := filepath.Join(root, "installed bin"), filepath.Join(root, "path bin")
					for _, dir := range []string{cwd, home, installDir, pathDir} {
						if err := os.MkdirAll(dir, 0700); err != nil {
							t.Fatal(err)
						}
					}
					binary := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$FIXTURE_CALLS\"\n"
					asset := "control_" + platform + "_arm64"
					locations := map[string]string{
						"cwd": filepath.Join(cwd, "control"), "asset": filepath.Join(cwd, asset),
						"bin": filepath.Join(cwd, "bin", "control"), "bin-asset": filepath.Join(cwd, "bin", asset),
						"install": filepath.Join(installDir, "control"), "default-install": filepath.Join(home, ".local", "bin", "control"),
						"path": filepath.Join(pathDir, "control"), "stale": filepath.Join(cwd, "control"),
					}
					if installer == "hybrid-system" {
						locations["install"] = filepath.Join(installDir, "fixture", "control")
					}
					if candidate := locations[location]; candidate != "" {
						if err := os.MkdirAll(filepath.Dir(candidate), 0700); err != nil {
							t.Fatal(err)
						}
						content := binary
						if location == "stale" {
							content = "#!/bin/sh\necho 'Unverified binary executed' >&2\nexit 99\n"
						}
						mode := os.FileMode(0700)
						if location == "asset" || location == "bin-asset" {
							mode = 0600 // A downloaded file need not already be executable.
						}
						if err := os.WriteFile(candidate, []byte(content), mode); err != nil {
							t.Fatal(err)
						}
						// A stale earlier candidate must not hide a valid later one.
						if location == "bin" {
							if err := os.WriteFile(filepath.Join(cwd, "control"), []byte("stale"), 0700); err != nil {
								t.Fatal(err)
							}
						}
					}
					i := Invitation{Version: "v1", ServiceMode: installer, Asset: update.Asset{OS: platform, Arch: "arm64", SHA256: Hash(binary)}}
					if installer == "hybrid-system" {
						i.ServiceMode, i.HybridRole, i.HybridID = "system", "system", "fixture"
					}
					script, err := Script(i, "https://gateway.example/install/ticket")
					if err != nil {
						t.Fatal(err)
					}
					// Never write privileged paths or request real elevation.
					script = strings.NewReplacer("/var/lib/control", home, "/usr/local/lib/control", installDir, "/Library/Application Support/control", home, "/Library/PrivilegedHelperTools/Control", installDir).Replace(script)
					calls, downloads := filepath.Join(root, "calls"), filepath.Join(root, "downloads")
					harness := fmt.Sprintf(`
uname() { if [ "$1" = -s ]; then echo %s; else echo arm64; fi; }
sysctl() { echo 1; }
id() { echo 0; }
curl() {
  printf '%%s\n' "$2" >> "$FIXTURE_DOWNLOADS"
  if [ "$FIXTURE_SCENARIO" = corrupt-download ]; then echo corrupt > "$4"; else printf '%%s' %s > "$4"; fi
}
`, map[string]string{"linux": "Linux", "darwin": "Darwin"}[platform], ShellQuote(binary))
					cmd := exec.CommandContext(t.Context(), "bash", "-c", harness+script)
					cmd.Dir = cwd
					if location == "default-install" {
						installDir = ""
					}
					cmd.Env = append(os.Environ(), "HOME="+home, "CONTROL_INSTALL_DIR="+installDir, "CONTROL_HOME="+home, "CONTROL_VERSION=v1", "CONTROL_SERVICE_MODE=", "PATH="+pathDir+string(os.PathListSeparator)+os.Getenv("PATH"), "FIXTURE_CALLS="+calls, "FIXTURE_DOWNLOADS="+downloads, "FIXTURE_SCENARIO="+location)
					output, runErr := cmd.CombinedOutput()
					if location == "corrupt-download" {
						if runErr == nil || !strings.Contains(string(output), "Binary checksum mismatch") {
							t.Fatalf("corrupt binary accepted: %v\n%s", runErr, output)
						}
					} else if runErr != nil {
						t.Fatalf("installer: %v\n%s", runErr, output)
					}
					gotDownloads, err := os.ReadFile(downloads)
					if err != nil && !os.IsNotExist(err) {
						t.Fatal(err)
					}
					wantDownloads := 0
					if location == "missing" || location == "stale" || location == "corrupt-download" {
						wantDownloads = 1
					}
					if got := strings.Count(string(gotDownloads), "\n"); got != wantDownloads {
						t.Fatalf("got %d binary downloads, want %d: %s", got, wantDownloads, gotDownloads)
					}
					gotCalls, err := os.ReadFile(calls)
					if location == "corrupt-download" {
						if err == nil {
							t.Fatalf("ran corrupt binary: %s", gotCalls)
						}
						return
					}
					want := "enroll --url https://gateway.example/install/ticket"
					if err != nil || !strings.HasPrefix(string(gotCalls), want) {
						t.Fatalf("missing installation: %v, %s", err, gotCalls)
					}
				})
			}
		}
	}
}
