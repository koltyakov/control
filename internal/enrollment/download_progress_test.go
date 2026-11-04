package enrollment

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/koltyakov/control/internal/update"
)

// Extract the real scoped requests so native fixtures can run downloads alone,
// without executing installation, elevation, filesystem or service operations.
func powerShellDownloadBlocks(t *testing.T) map[string][]string {
	t.Helper()
	scripts := map[string]string{}
	for _, mode := range []string{"", "user", "system", "hybrid", "hybrid-system"} {
		invitation := Invitation{ServiceMode: mode, Asset: update.Asset{OS: "windows", Arch: "amd64", SHA256: strings.Repeat("a", 64)}}
		if mode == "hybrid-system" {
			invitation.ServiceMode, invitation.HybridRole, invitation.HybridID = "system", "system", "fixture"
		}
		script, err := Script(invitation, "https://gateway.example/install/ticket")
		if err != nil {
			t.Fatal(err)
		}
		scripts["machine/"+mode] = script
	}
	release, err := os.ReadFile("../../scripts/install.ps1")
	if err != nil {
		t.Fatal(err)
	}
	scripts["release"] = string(release)
	request := regexp.MustCompile(`Invoke-(?:WebRequest|RestMethod)\b`)
	scoped := regexp.MustCompile(`(?m)^[\t ]*(?:\$\w+ = )?& \{\r?\n[\t ]*\$ProgressPreference = 'SilentlyContinue'\r?\n[\t ]*Invoke-(?:WebRequest|RestMethod)[^\r\n]*\r?\n[\t ]*\}`)
	blocks := map[string][]string{}
	for name, script := range scripts {
		matches := scoped.FindAllString(script, -1)
		if len(matches) == 0 || len(matches) != len(request.FindAllString(script, -1)) {
			t.Fatalf("%s: every download must suppress progress in its own child scope", name)
		}
		if strings.Count(script, "$ProgressPreference") != len(matches) {
			t.Fatalf("%s: progress preference is modified outside a download scope", name)
		}
		blocks[name] = matches
	}
	return blocks
}

func TestPowerShellDownloadsScopeProgressPreference(t *testing.T) {
	_ = powerShellDownloadBlocks(t)
}
