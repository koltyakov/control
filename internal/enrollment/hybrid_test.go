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

func TestHybridLingerIsOptIn(t *testing.T) {
	for _, linger := range []bool{false, true} {
		i := Invitation{ID: "primary", HybridID: "primary", HybridRole: "user", ServiceMode: "hybrid", Linger: linger, Asset: update.Asset{OS: "linux", Arch: "amd64", SHA256: strings.Repeat("a", 64)}}
		script, err := Script(i, "https://gateway.example/install/ticket")
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(script, "enable-linger") != linger {
			t.Fatal("linger must follow the invitation", linger)
		}
		if linger && strings.Index(script, "--preflight") > strings.Index(script, "enable-linger") {
			t.Fatal("linger ran before the User preflight")
		}
	}
}

func TestHybridPowerShellStagesInProtectedDirectory(t *testing.T) {
	for _, i := range []Invitation{
		{HybridID: "fixture", HybridRole: "user", ServiceMode: "hybrid"},
		{HybridID: "fixture", HybridRole: "system", ServiceMode: "system"},
	} {
		i.Asset = update.Asset{OS: "windows", Arch: "amd64", SHA256: strings.Repeat("a", 64)}
		script, err := Script(i, "https://gateway.example/install/ticket")
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(script, "GetTempPath") || !strings.Contains(script, "$tmp = Join-Path $protectedHome") {
			t.Fatal("elevated Hybrid code must not run from the user's temporary folder", i.HybridRole)
		}
		if strings.Index(script, "Set-Acl") > strings.Index(script, "$tmp = Join-Path $protectedHome") {
			t.Fatal("protect the staging parent before creating the staging folder", i.HybridRole)
		}
	}
	user, err := Script(Invitation{ServiceMode: "user", Asset: update.Asset{OS: "windows", Arch: "amd64", SHA256: strings.Repeat("a", 64)}}, "https://gateway.example/install/ticket")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(user, "protectedHome") {
		t.Fatal("ordinary User installation needs no protected directory")
	}
}

func TestHybridPowerShellSeparatesUserPrivileges(t *testing.T) {
	i := Invitation{HybridID: "fixture", HybridRole: "user", ServiceMode: "hybrid", AutoName: true, Asset: update.Asset{OS: "windows", Arch: "amd64", SHA256: strings.Repeat("a", 64)}}
	script, err := Script(i, "https://gateway.example/install/ticket")
	if err != nil {
		t.Fatal(err)
	}
	preflight := strings.Index(script, "& $exe __hybrid-user --url 'https://gateway.example/install/ticket' --auto-name --preflight")
	check := strings.Index(script, "& $exe __hybrid-system-check $systemExe $systemConfig")
	recovery := strings.Index(script, "& $systemExe --config $systemConfig enroll")
	user := strings.LastIndex(script, "& $exe __hybrid-user --url 'https://gateway.example/install/ticket' --auto-name")
	if preflight < 0 || check <= preflight || recovery <= check || user <= recovery {
		t.Fatal("Hybrid phases are not limited preflight, checked System recovery, limited User enrollment", script)
	}
	if strings.Contains(script, "& $exe enroll") || strings.Count(script, "& $exe __hybrid-user") != 2 {
		t.Fatal("the elevated script must never directly enroll the User profile", script)
	}
	if strings.Contains(script, "Copy-Item -LiteralPath") || !strings.Contains(script, "$reader.CopyTo($writer)") {
		t.Fatal("staging must copy bytes without inheriting an untrusted source DACL")
	}
	for _, want := range []string{
		"FileSystemAccessRule($userSid, 'ReadAndExecute', 'Allow')",
		"FileSystemAccessRule($userSid, 'ReadAndExecute', 'ContainerInherit,ObjectInherit', 'None', 'Allow')",
		"$stageAcl.SetOwner",
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("limited child needs read-only access to protected staging: missing %s", want)
		}
	}
	if strings.Contains(script, "FileSystemAccessRule($userSid, 'FullControl'") || strings.Contains(script, "FileSystemAccessRule($userSid, 'Modify'") {
		t.Fatal("limited user can change privileged staging")
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
		for _, scenario := range []string{"fresh", "installed", "pending", "different", "corrupt-download", "preflight-rejected"} {
			t.Run(platform+"/"+scenario, func(t *testing.T) {
				t.Parallel()
				root := t.TempDir()
				binary := `#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >> "$FIXTURE_CALLS"
if [[ " $* " == *' --preflight '* ]]; then
  if [ "$FIXTURE_SCENARIO" = preflight-rejected ]; then echo 'User startup unavailable' >&2; exit 1; fi
  exit 0
fi
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
				if scenario != "fresh" && scenario != "preflight-rejected" {
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
				switch scenario {
				case "corrupt-download":
					if runErr == nil || !strings.Contains(string(output), "Binary checksum mismatch") {
						t.Fatalf("corrupt fallback must fail verification: %v\n%s", runErr, output)
					}
				case "preflight-rejected":
					if runErr == nil || !strings.Contains(string(output), "User startup unavailable") {
						t.Fatalf("rejected preflight must stop the installer: %v\n%s", runErr, output)
					}
					if _, err := os.Stat(systemHome); !os.IsNotExist(err) {
						t.Fatal("rejected preflight still installed the System companion", err)
					}
				default:
					if runErr != nil {
						t.Fatalf("installer: %v\n%s", runErr, output)
					}
				}
				gotDownloads, err := os.ReadFile(downloads)
				if err != nil && !os.IsNotExist(err) {
					t.Fatal(err)
				}
				wantDownloads := 0
				if scenario == "fresh" || scenario == "different" || scenario == "corrupt-download" || scenario == "preflight-rejected" {
					wantDownloads = 1
				}
				if got := strings.Count(string(gotDownloads), "/binary?"); got != wantDownloads {
					t.Fatalf("got %d downloads, want %d: %s", got, wantDownloads, gotDownloads)
				}
				// A corrupt download must stop the installer before anything runs.
				gotCalls, err := os.ReadFile(calls)
				if scenario == "corrupt-download" {
					if !os.IsNotExist(err) {
						t.Fatalf("ran something before verifying the download: %s %v", gotCalls, err)
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				userCall := "enroll --url " + link + " --service user"
				if strings.Contains(string(gotCalls), userCall) != (scenario != "preflight-rejected") {
					t.Fatalf("unexpected User enrollment: %s", gotCalls)
				}
				preflight := strings.Index(string(gotCalls), "enroll --url "+link+" --preflight --service user")
				if preflight < 0 {
					t.Fatalf("installer skipped the User preflight: %s", gotCalls)
				}
				if system := strings.Index(string(gotCalls), "--hybrid-system"); system >= 0 && system < preflight {
					t.Fatalf("System installed before the User preflight: %s", gotCalls)
				}
				if scenario != "preflight-rejected" {
					gotBinary, err := os.ReadFile(filepath.Join(root, "user-bin", "control"))
					if err != nil || string(gotBinary) != binary {
						t.Fatalf("User binary does not match invitation: %v", err)
					}
				}
			})
		}
	}
}
