package enrollment

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
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

func TestHybridPowerShellReusesSystemBinary(t *testing.T) {
	for _, scenario := range []string{"matching", "different", "corrupt-download"} {
		t.Run(scenario, func(t *testing.T) {
			root := t.TempDir()
			binary := "pinned binary"
			installed := binary
			if scenario != "matching" {
				installed = "older companion"
			}
			systemExe := filepath.Join(root, "system.exe")
			if err := os.WriteFile(systemExe, []byte(installed), 0600); err != nil {
				t.Fatal(err)
			}
			i := Invitation{ServiceMode: "hybrid", HybridRole: "user", HybridID: "fixture", Asset: update.Asset{OS: "windows", Arch: "amd64", SHA256: Hash(binary)}}
			script, err := Script(i, "https://gateway.example/install/ticket")
			if err != nil {
				t.Fatal(err)
			}
			// Exercise the generated copy/download/verification block with real
			// files, without enrollment, elevation, ACL or service changes.
			start, end := strings.Index(script, "  $exe = Join-Path $tmp"), strings.Index(script, "  & $exe enroll")
			if start < 0 || end <= start {
				t.Fatal("missing binary staging block")
			}
			verify := `$ErrorActionPreference = 'Stop'
$tmp = ` + PowerShellQuote(root) + `
$systemExe = ` + PowerShellQuote(systemExe) + `
$arch = 'amd64'
$checksum = ` + PowerShellQuote(Hash(binary)) + `
$scenario = ` + PowerShellQuote(scenario) + `
$script:downloads = 0
function Invoke-WebRequest {
  param([switch]$UseBasicParsing, [string]$Uri, [string]$OutFile)
  if ($Uri -cne 'https://gateway.example/install/ticket/binary?arch=amd64') { throw 'Incorrect binary request' }
  $script:downloads++
  $content = if ($scenario -eq 'corrupt-download') { 'corrupt' } else { 'pinned binary' }
  [IO.File]::WriteAllText($OutFile, $content)
}
$failure = $null
try {
` + script[start:end] + `
} catch { $failure = $_.Exception.Message }
if ($scenario -eq 'corrupt-download') {
  if ($failure -ne 'Binary checksum mismatch') { throw 'Corrupt fallback was not rejected' }
} elseif ($failure) { throw $failure }
$expectedDownloads = if ($scenario -eq 'matching') { 0 } else { 1 }
if ($script:downloads -ne $expectedDownloads) { throw 'Unexpected binary download count' }
if ([IO.File]::ReadAllText($systemExe) -cne ` + PowerShellQuote(installed) + `) { throw 'System binary was modified' }
`
			cmd := exec.CommandContext(t.Context(), "powershell.exe", "-NoProfile", "-NonInteractive", "-Command", verify)
			if output, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("PowerShell staging: %v\n%s", err, output)
			}
		})
	}
}
