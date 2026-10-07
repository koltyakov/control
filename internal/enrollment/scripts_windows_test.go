package enrollment

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"unicode/utf16"

	"github.com/koltyakov/control/internal/update"
)

func TestWindowsInstallerParsesAndLoadsWholeResponse(t *testing.T) {
	for _, mode := range []string{"", "user", "system"} {
		for _, link := range []string{"https://gateway.example/install/token", "https://gateway.example/日本'$path/install/token"} {
			t.Run(mode+"/"+link, func(t *testing.T) {
				invitation := Invitation{ServiceMode: mode, AutoName: true, Asset: update.Asset{OS: "windows", Arch: "amd64", SHA256: strings.Repeat("a", 64)}}
				script, err := Script(invitation, link)
				if err != nil {
					t.Fatal(err)
				}
				command := InstallCommand(link, true, mode)
				var loader string
				if encoded, ok := strings.CutPrefix(command, "powershell.exe -NoProfile -EncodedCommand "); ok {
					data, err := base64.StdEncoding.DecodeString(encoded)
					if err != nil || len(data)%2 != 0 {
						t.Fatalf("invalid encoded command: %v", err)
					}
					units := make([]uint16, len(data)/2)
					for i := range units {
						units[i] = binary.LittleEndian.Uint16(data[i*2:])
					}
					loader = string(utf16.Decode(units))
				} else {
					var ok bool
					loader, ok = strings.CutPrefix(command, `powershell.exe -NoProfile -c "`)
					if !ok || !strings.HasSuffix(loader, `"`) {
						t.Fatalf("unexpected launcher: %s", command)
					}
					loader = strings.TrimSuffix(loader, `"`)
				}
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
` + loader + `
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

func TestWindowsInstallCommandRunsFromOuterShell(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("if ($true) {\n  if ($true) {\n    Write-Output 'installer executed'\n  }\n}\n"))
	}))
	t.Cleanup(server.Close)
	for _, path := range []string{"/install/token", "/a'b/install/token", "/$path/install/token"} {
		command := InstallCommand(server.URL+path, true, "user")
		for _, shell := range []string{"powershell.exe", "cmd.exe"} {
			t.Run(shell+path, func(t *testing.T) {
				cmd := exec.CommandContext(t.Context(), shell, "-NoProfile", "-NonInteractive", "-Command", command)
				if shell == "cmd.exe" {
					// CMD does not use the CommandLineToArgvW quoting applied by os/exec.
					cmd = exec.CommandContext(t.Context(), shell)
					cmd.SysProcAttr = &syscall.SysProcAttr{CmdLine: `cmd.exe /d /s /c "` + command + `"`}
				}
				// Encoded commands can emit CLIXML progress on stderr even on success.
				var stderr bytes.Buffer
				cmd.Stderr = &stderr
				output, err := cmd.Output()
				if err != nil || strings.TrimSpace(string(output)) != "installer executed" {
					t.Fatalf("launcher: %s\nresult: %v\nstdout:\n%s\nstderr:\n%s", command, err, output, stderr.String())
				}
			})
		}
	}
}
