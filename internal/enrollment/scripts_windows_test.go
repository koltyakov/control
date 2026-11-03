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

func windowsPowerShellCommand(t *testing.T, script string) *exec.Cmd {
	t.Helper()
	// CI starts tests in PowerShell 7. -NoProfile does not clear its inherited
	// PSModulePath, whose modules can fail to load in Windows PowerShell 5.1.
	// Change only the child process, never the test runner's environment.
	script = "$env:PSModulePath = $PSHOME + '\\Modules'\n" + script
	return exec.CommandContext(t.Context(), "powershell.exe", "-NoProfile", "-NonInteractive", "-Command", script)
}

// windowsPowerShellFileCommand keeps installer one-liners off the command line,
// where Microsoft Defender can deny process creation based on their text.
func windowsPowerShellFileCommand(t *testing.T, script string) *exec.Cmd {
	t.Helper()
	file := filepath.Join(t.TempDir(), "verify.ps1")
	// Windows PowerShell 5.1 reads a script without a BOM as ANSI.
	content := "\ufeff$env:PSModulePath = $PSHOME + '\\Modules'\n" + script
	if err := os.WriteFile(file, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	return exec.CommandContext(t.Context(), "powershell.exe", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", file)
}

func TestWindowsPowerShellIgnoresInheritedModulePath(t *testing.T) {
	root := t.TempDir()
	module := filepath.Join(root, "contaminated", "Microsoft.PowerShell.Utility")
	if err := os.MkdirAll(module, 0700); err != nil {
		t.Fatal(err)
	}
	manifest := "@{ ModuleVersion = '99.0'; GUID = 'eed74dfb-5093-46dd-9b75-c317d96d68cd'; RootModule = 'utility.psm1'; FunctionsToExport = @('Get-FileHash') }"
	if err := os.WriteFile(filepath.Join(module, "Microsoft.PowerShell.Utility.psd1"), []byte(manifest), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(module, "utility.psm1"), []byte("throw 'inherited utility module was loaded'"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PSModulePath", filepath.Dir(module))
	file := filepath.Join(root, "binary.exe")
	if err := os.WriteFile(file, []byte("pinned binary"), 0600); err != nil {
		t.Fatal(err)
	}
	verify := `$ErrorActionPreference = 'Stop'
if ($env:PSModulePath -cne ($PSHOME + '\Modules')) { throw 'Inherited module path leaked' }
if ((Get-FileHash -Algorithm SHA256 -LiteralPath ` + PowerShellQuote(file) + `).Hash.ToLowerInvariant() -ne ` + PowerShellQuote(Hash("pinned binary")) + `) { throw 'Incorrect file checksum' }
`
	if output, err := windowsPowerShellCommand(t, verify).CombinedOutput(); err != nil {
		t.Fatalf("native checksum module unavailable: %v\n%s", err, output)
	}
	if got := os.Getenv("PSModulePath"); got != filepath.Dir(module) {
		t.Fatalf("parent module path changed: %q", got)
	}
}

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
				cmd := windowsPowerShellFileCommand(t, verify)
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
			cmd := windowsPowerShellFileCommand(t, command)
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
	for _, scenario := range []string{"matching", "different", "corrupt-download", "missing-companion"} {
		t.Run(scenario, func(t *testing.T) {
			root := t.TempDir()
			binary := "pinned binary"
			installed := binary
			if scenario != "matching" {
				installed = "older companion"
			}
			systemExe := filepath.Join(root, "system.exe")
			if scenario != "missing-companion" {
				if err := os.WriteFile(systemExe, []byte(installed), 0600); err != nil {
					t.Fatal(err)
				}
			}
			i := Invitation{ServiceMode: "hybrid", HybridRole: "user", HybridID: "fixture", Asset: update.Asset{OS: "windows", Arch: "amd64", SHA256: Hash(binary)}}
			script, err := Script(i, "https://gateway.example/install/ticket")
			if err != nil {
				t.Fatal(err)
			}
			// Exercise the generated copy/download/verification block with real
			// files, without enrollment, elevation, ACL or service changes.
			start, end := strings.Index(script, "  $exe = Join-Path $tmp"), strings.Index(script, "  & $exe __hybrid-user")
			if start < 0 || end <= start {
				t.Fatal("missing binary staging block")
			}
			verify := `$ErrorActionPreference = 'Stop'
$tmp = ` + PowerShellQuote(root) + `
$systemExe = ` + PowerShellQuote(systemExe) + `
$systemInstalled = Test-Path -LiteralPath $systemExe
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
  if ($failure -ne 'Binary checksum mismatch') { throw "Corrupt fallback was not rejected: $failure" }
} elseif ($failure) { throw $failure }
$expectedDownloads = if ($scenario -eq 'matching') { 0 } else { 1 }
if ($script:downloads -ne $expectedDownloads) { throw 'Unexpected binary download count' }
if ($scenario -ne 'missing-companion' -and [IO.File]::ReadAllText($systemExe) -cne ` + PowerShellQuote(installed) + `) { throw 'System binary was modified' }
`
			cmd := windowsPowerShellCommand(t, verify)
			if output, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("PowerShell staging: %v\n%s", err, output)
			}
		})
	}
}

func TestPowerShellInstallersReuseLocalBinary(t *testing.T) {
	for _, installer := range []string{"user", "system", "hybrid-system"} {
		for _, location := range []string{"cwd", "asset", "bin", "bin-asset", "install", "default-install", "path", "missing", "stale", "corrupt-download"} {
			t.Run(installer+"/"+location, func(t *testing.T) {
				t.Parallel()
				root := t.TempDir()
				cwd, installDir, localAppData, pathDir := filepath.Join(root, "working folder"), filepath.Join(root, "install"), filepath.Join(root, "local"), filepath.Join(root, "path")
				for _, dir := range []string{cwd, installDir, localAppData, pathDir} {
					if err := os.MkdirAll(dir, 0700); err != nil {
						t.Fatal(err)
					}
				}
				binary, asset := "pinned binary", "control_windows_amd64.exe"
				locations := map[string]string{
					"cwd": filepath.Join(cwd, "control.exe"), "asset": filepath.Join(cwd, asset),
					"bin": filepath.Join(cwd, "bin", "control.exe"), "bin-asset": filepath.Join(cwd, "bin", asset),
					"install": filepath.Join(installDir, "control.exe"), "default-install": filepath.Join(localAppData, "Programs", "control", "control.exe"),
					"path": filepath.Join(pathDir, "control.exe"), "stale": filepath.Join(cwd, "control.exe"),
				}
				candidate := locations[location]
				if candidate != "" {
					if err := os.MkdirAll(filepath.Dir(candidate), 0700); err != nil {
						t.Fatal(err)
					}
					content := binary
					if location == "stale" {
						content = "older binary"
					}
					if err := os.WriteFile(candidate, []byte(content), 0600); err != nil {
						t.Fatal(err)
					}
					if location == "bin" {
						if err := os.WriteFile(filepath.Join(cwd, "control.exe"), []byte("stale"), 0600); err != nil {
							t.Fatal(err)
						}
					}
				}
				i := Invitation{ServiceMode: installer, Asset: update.Asset{OS: "windows", Arch: "amd64", SHA256: Hash(binary)}}
				if installer == "hybrid-system" {
					i.ServiceMode, i.HybridRole, i.HybridID = "system", "system", "fixture"
				}
				script, err := Script(i, "https://gateway.example/install/ticket")
				if err != nil {
					t.Fatal(err)
				}
				start, end := strings.Index(script, "  $exe = Join-Path $tmp"), strings.Index(script, "  & $exe ")
				if start < 0 || end <= start {
					t.Fatal("missing binary staging block")
				}
				// Run only staging, never enrollment, ACLs or services.
				verify := `$ErrorActionPreference = 'Stop'
$tmp = ` + PowerShellQuote(root) + `
$env:CONTROL_INSTALL_DIR = ` + PowerShellQuote(installDir) + `
$env:LOCALAPPDATA = ` + PowerShellQuote(localAppData) + `
$env:Path = ` + PowerShellQuote(pathDir) + ` + ';' + $env:Path
$arch = 'amd64'
$asset = ` + PowerShellQuote(asset) + `
$checksum = ` + PowerShellQuote(Hash(binary)) + `
$scenario = ` + PowerShellQuote(location) + `
$script:downloads = 0
function Invoke-WebRequest {
  param([switch]$UseBasicParsing, [string]$Uri, [string]$OutFile)
  $script:downloads++
  $content = if ($scenario -eq 'corrupt-download') { 'corrupt' } else { 'pinned binary' }
  [IO.File]::WriteAllText($OutFile, $content)
}
$failure = $null
try {
` + script[start:end] + `
} catch { $failure = $_.Exception.Message }
if ($scenario -eq 'corrupt-download') {
  if ($failure -ne 'Binary checksum mismatch') { throw "Corrupt fallback was not rejected: $failure" }
} elseif ($failure) { throw $failure }
$expectedDownloads = if ($scenario -in @('missing', 'stale', 'corrupt-download')) { 1 } else { 0 }
if ($script:downloads -ne $expectedDownloads) { throw "Unexpected binary download count: $script:downloads" }
`
				cmd := windowsPowerShellCommand(t, verify)
				cmd.Dir = cwd
				if output, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("PowerShell staging: %v\n%s", err, output)
				}
			})
		}
	}
}
