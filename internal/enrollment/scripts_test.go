package enrollment

import (
	"encoding/base64"
	"encoding/binary"
	"strings"
	"testing"
	"unicode/utf16"

	"github.com/koltyakov/control/internal/update"
)

func TestInstallCommand(t *testing.T) {
	link := "https://gateway.example/a'b/install/token"
	for _, mode := range []string{"", "user", "system"} {
		want := `powershell.exe -NoProfile -c "irm 'https://gateway.example/a''b/install/token'|iex"`
		if got := InstallCommand(link, true, mode); got != want {
			t.Fatalf("Windows command for %q: got %q, want %q", mode, got, want)
		}
		want = "curl -fsSL " + ShellQuote(link) + " | bash"
		if mode == "system" {
			want = "curl -fsSL " + ShellQuote(link) + " | sudo bash"
		}
		if got := InstallCommand(link, false, mode); got != want {
			t.Fatalf("Unix command for %q: got %q, want %q", mode, got, want)
		}
	}
}

func TestWindowsInstallCommandPreservesShellMetacharacters(t *testing.T) {
	for _, char := range []string{`"`, "$", "`", `\`, "%", "!", "\r", "\n"} {
		link := "https://gateway.example/日本" + char + "'path/install/token"
		command := InstallCommand(link, true, "user")
		encoded, ok := strings.CutPrefix(command, "powershell.exe -NoProfile -EncodedCommand ")
		if !ok {
			t.Fatalf("unsafe URL must not appear in an outer-shell command: %q", command)
		}
		data, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil || len(data)%2 != 0 {
			t.Fatalf("invalid encoded PowerShell command: %q, %v", command, err)
		}
		units := make([]uint16, len(data)/2)
		for i := range units {
			units[i] = binary.LittleEndian.Uint16(data[i*2:])
		}
		if got, want := string(utf16.Decode(units)), "irm "+PowerShellQuote(link)+"|iex"; got != want {
			t.Fatalf("decoded command: got %q, want %q", got, want)
		}
	}
}

func TestInstallerQuotingAndPlatformSelection(t *testing.T) {
	for _, platform := range []string{"linux", "darwin", "windows"} {
		i := Invitation{Asset: update.Asset{OS: platform, Arch: "arm64", SHA256: strings.Repeat("a", 64)}}
		script, err := Script(i, "https://gateway.example/a'b/install/token")
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(script, "enroll --url") || !strings.Contains(script, i.Asset.SHA256) {
			t.Fatal("installer omitted checksum or enrollment")
		}
		if platform == "windows" && !strings.Contains(script, "a''b") {
			t.Fatal("PowerShell URL not escaped")
		}
		if platform != "windows" && !strings.Contains(script, `a'"'"'b`) {
			t.Fatal("shell URL not escaped")
		}
	}
}

func TestWindowsInstallerDefaultsToUserMode(t *testing.T) {
	i := Invitation{Asset: update.Asset{OS: "windows", Arch: "amd64", SHA256: strings.Repeat("a", 64)}}
	script, err := Script(i, "https://gateway.example/install/token")
	if err != nil {
		t.Fatal(err)
	}
	selection := "$serviceMode = if ($env:CONTROL_SERVICE_MODE) { $env:CONTROL_SERVICE_MODE } else { 'user' }"
	guard := "if ($serviceMode -ne 'process' -and $serviceMode -ne 'user')"
	if !strings.Contains(script, selection) || strings.Index(script, selection) > strings.Index(script, guard) {
		t.Fatal("installer must default to user startup while preserving explicit environment overrides before checking elevation")
	}
	if !strings.Contains(script, guard) || strings.Index(script, guard) > strings.Index(script, "Invoke-WebRequest") {
		t.Fatal("installer must check system-mode elevation before download, exempting user and process modes")
	}
	if !strings.Contains(script, "enroll --url 'https://gateway.example/install/token' --service $serviceMode") {
		t.Fatal("installer must pass the selected mode explicitly instead of reusing saved system startup")
	}
	if !strings.Contains(script, "--service $serviceMode --firewall") {
		t.Fatal("Windows worker installation must request scoped firewall setup")
	}
	if strings.Contains(script, "$env:CONTROL_SERVICE_MODE =") {
		t.Fatal("installer must not change the caller's service-mode environment")
	}
}

func TestInstallerExplicitStartupContext(t *testing.T) {
	for _, platform := range []string{"linux", "darwin", "windows"} {
		for _, mode := range []string{"user", "system"} {
			i := Invitation{ServiceMode: mode, Asset: update.Asset{OS: platform, Arch: "amd64", SHA256: strings.Repeat("a", 64)}}
			script, err := Script(i, "https://gateway.example/install/token")
			if err != nil {
				t.Fatal(err)
			}
			command := InstallCommand("https://gateway.example/install/token", platform == "windows", mode)
			if strings.Contains(command, "sudo bash") != (platform != "windows" && mode == "system") {
				t.Fatalf("wrong elevation: %s", command)
			}
			if platform == "windows" {
				if !strings.Contains(script, "$serviceMode = '"+mode+"'") || strings.Contains(script, "$env:CONTROL_SERVICE_MODE) {") || !strings.Contains(script, "if ($serviceMode -eq 'system') { $serviceMode = 'auto' }") {
					t.Fatal("explicit Windows context must override environment and map system to SCM")
				}
			} else {
				if !strings.Contains(script, "service_mode='"+mode+"'") || strings.Contains(script, "${CONTROL_SERVICE_MODE:-}") {
					t.Fatal("explicit Unix context must override environment")
				}
				if strings.Index(script, "System startup requires root") > strings.Index(script, "mktemp -d") {
					t.Fatal("system elevation must be checked before downloads and enrollment")
				}
				if !strings.Contains(script, "/var/lib/control") || !strings.Contains(script, "/usr/local/bin") || !strings.Contains(script, `set -- --service "$service_mode"`) {
					t.Fatal("Unix script omitted system paths or enrollment context")
				}
			}
		}
	}
}

func TestAutoNameInstallerRejectsOlderBinariesBeforeEnrollment(t *testing.T) {
	for _, platform := range []string{"linux", "darwin", "windows"} {
		for _, auto := range []bool{false, true} {
			i := Invitation{AutoName: auto, Asset: update.Asset{OS: platform, Arch: "amd64", SHA256: strings.Repeat("a", 64)}}
			script, err := Script(i, "https://gateway.example/install/token")
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(script, " --auto-name") != auto {
				t.Fatal("automatic installer must require the new enrollment flag; fixed-name scripts must remain compatible")
			}
			if platform == "windows" && !strings.Contains(script, " --service $serviceMode") || platform != "windows" && !strings.Contains(script, `"$@"`) {
				t.Fatal("scripts must pass the selected startup mode")
			}
		}
	}
}
