package enrollment

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"

	"github.com/koltyakov/control/internal/update"
)

func TestWindowsInstallerParsesAndLoadsWholeResponse(t *testing.T) {
	for _, mode := range []string{"", "user", "system", "hybrid", "hybrid-system"} {
		for _, link := range []string{"https://gateway.example/install/token", "https://gateway.example/日本'$path/install/token"} {
			t.Run(mode+"/"+link, func(t *testing.T) {
				invitation := Invitation{ServiceMode: mode, AutoName: true, Asset: update.Asset{OS: "windows", Arch: "amd64", SHA256: strings.Repeat("a", 64)}}
				if mode == "hybrid-system" {
					invitation.ServiceMode, invitation.HybridRole, invitation.HybridID = "system", "system", "fixture"
				}
				script, err := Script(invitation, link)
				if err != nil {
					t.Fatal(err)
				}
				command := InstallCommand(link, true, mode)
				// Parse the real installer without running it. Execute only a harmless
				// multi-line response through a mocked web request, with no network,
				// enrollment, filesystem changes, or privilege changes.
				verify := `$ErrorActionPreference = 'Stop'
$tokens = $null
$parseErrors = $null
[void][System.Management.Automation.Language.Parser]::ParseInput(` + PowerShellQuote(script) + `, [ref]$tokens, [ref]$parseErrors)
if ($parseErrors.Count -ne 0) { throw ($parseErrors | Out-String) }
function Invoke-RestMethod {
  param([string]$Uri)
  if ($Uri -cne ` + PowerShellQuote(link) + `) { throw 'Incorrect web request' }
  @'
if ($true) {
  if ($true) {
    $global:installerResult = 'executed'
  }
}
'@
}
` + command + `
if ($global:installerResult -ne 'executed') { throw 'Multi-line response was not executed as a whole' }
`
				cmd := exec.CommandContext(t.Context(), "powershell.exe", "-NoProfile", "-NonInteractive", "-Command", verify)
				if output, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("Windows PowerShell validation: %v\n%s", err, output)
				}
			})
		}
	}
}

func TestWindowsInstallCommandRunsInPowerShell(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("if ($true) {\n  if ($true) {\n    Write-Output 'installer executed'\n  }\n}\n"))
	}))
	t.Cleanup(server.Close)
	for _, path := range []string{"/install/token", "/a'b/install/token", "/$path/install/token"} {
		command := InstallCommand(server.URL+path, true, "user")
		t.Run(path, func(t *testing.T) {
			cmd := exec.CommandContext(t.Context(), "powershell.exe", "-NoProfile", "-NonInteractive", "-Command", command)
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			output, err := cmd.Output()
			if err != nil || strings.TrimSpace(string(output)) != "installer executed" {
				t.Fatalf("launcher: %s\nresult: %v\nstdout:\n%s\nstderr:\n%s", command, err, output, stderr.String())
			}
		})
	}
}
