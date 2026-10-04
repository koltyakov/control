package enrollment

import (
	"strings"
	"testing"

	"github.com/koltyakov/control/internal/update"
)

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
