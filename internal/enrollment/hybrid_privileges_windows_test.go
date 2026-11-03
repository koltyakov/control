package enrollment

import (
	"strings"
	"testing"

	"github.com/koltyakov/control/internal/update"
)

func TestHybridPowerShellPrivilegePhaseFailures(t *testing.T) {
	for _, scenario := range []string{"fresh", "existing", "pending", "preflight-failed", "unsafe-system", "system-failed", "user-failed"} {
		t.Run(scenario, func(t *testing.T) {
			i := Invitation{ServiceMode: "hybrid", HybridRole: "user", HybridID: "fixture", Asset: update.Asset{OS: "windows", Arch: "amd64", SHA256: strings.Repeat("a", 64)}}
			script, err := Script(i, "https://gateway.example/install/ticket")
			if err != nil {
				t.Fatal(err)
			}
			start := strings.Index(script, "  # Run User preflight")
			end := strings.Index(script, "\n} finally {\n  Remove-Item")
			if start < 0 || end <= start {
				t.Fatal("missing Hybrid phase block")
			}
			// Exercise generated dispatch, not enrollment, UAC or services.
			verify := `$ErrorActionPreference = 'Stop'
$scenario = ` + PowerShellQuote(scenario) + `
$protectedHome = ` + PowerShellQuote(t.TempDir()) + `
$systemConfig = Join-Path $protectedHome 'node.json'
$tmp = $protectedHome
$exe = 'Invoke-ProtectedInstaller'
$systemExe = 'Invoke-SystemInstaller'
$systemInstalled = $scenario -ne 'fresh'
$global:phases = @()
$env:CONTROL_HOME = 'original-home'
$env:CONTROL_CONFIG = 'original-config'
$env:CONTROL_INSTALL_DIR = 'original-bin'
if ($scenario -eq 'pending') { Set-Content -LiteralPath (Join-Path $protectedHome 'pending-enrollment.json') -Value '{}' }
function Invoke-ProtectedInstaller {
  $global:LASTEXITCODE = 0
  if ($args[0] -eq '__hybrid-system-check') {
    $global:phases += 'check'
    if ($scenario -eq 'unsafe-system') { $global:LASTEXITCODE = 1 }
  } elseif ($args[0] -eq '__hybrid-user') {
    if ($args -contains '--firewall' -or $args -contains '--limited') { throw 'elevated script bypassed the limited helper' }
    if ($args -contains '--preflight') {
      $global:phases += 'preflight'
      if ($scenario -eq 'preflight-failed') { $global:LASTEXITCODE = 1 }
    } else {
      if ($env:CONTROL_HOME -ne 'original-home' -or $env:CONTROL_CONFIG -ne 'original-config' -or $env:CONTROL_INSTALL_DIR -ne 'original-bin') { throw 'System environment leaked into User enrollment' }
      $global:phases += 'user'
      if ($scenario -eq 'user-failed') { $global:LASTEXITCODE = 1 }
    }
  } else { throw 'elevated script directly executed User enrollment' }
}
function Invoke-SystemInstaller {
  $global:LASTEXITCODE = 0
  if ($args -contains 'enroll') {
    if ($args -notcontains '--hybrid-system') { throw 'System recovery lost its protected-mode requirement' }
    $global:phases += 'resume'
  } else { $global:phases += 'start' }
  if ($scenario -eq 'system-failed') { $global:LASTEXITCODE = 1 }
}
function Invoke-RestMethod {
  return '$global:phases += ''install''; $global:LASTEXITCODE = 0'
}
$failed = $false
try {
` + script[start:end] + `
} catch { $failed = $true }
$expected = switch ($scenario) {
  'fresh' { 'preflight,install,user' }
  'existing' { 'preflight,check,start,user' }
  'pending' { 'preflight,check,resume,user' }
  'preflight-failed' { 'preflight' }
  'unsafe-system' { 'preflight,check' }
  'system-failed' { 'preflight,check,start' }
  'user-failed' { 'preflight,check,start,user' }
}
if (($global:phases -join ',') -ne $expected) { throw ('Unexpected phases: ' + ($global:phases -join ',')) }
if ($failed -ne ($scenario -like '*failed' -or $scenario -eq 'unsafe-system')) { throw 'phase failure was not propagated' }
if ($env:CONTROL_HOME -ne 'original-home' -or $env:CONTROL_CONFIG -ne 'original-config' -or $env:CONTROL_INSTALL_DIR -ne 'original-bin') { throw 'caller environment was not restored' }
`
			if output, err := windowsPowerShellCommand(t, verify).CombinedOutput(); err != nil {
				t.Fatalf("Hybrid phase separation: %v\n%s", err, output)
			}
		})
	}
}

func TestHybridPowerShellCopiesBytesWithoutSourceACL(t *testing.T) {
	i := Invitation{ServiceMode: "hybrid", HybridRole: "user", HybridID: "fixture", Asset: update.Asset{OS: "windows", Arch: "amd64", SHA256: strings.Repeat("a", 64)}}
	script, err := Script(i, "https://gateway.example/install/ticket")
	if err != nil {
		t.Fatal(err)
	}
	start := strings.Index(script, "  function Copy-ControlBinary")
	if start < 0 {
		t.Fatal("missing byte-only staging helper")
	}
	end := strings.Index(script[start:], "\n  }\n")
	if end < 0 {
		t.Fatal("missing staging helper end")
	}
	verify := `$ErrorActionPreference = 'Stop'
$root = ` + PowerShellQuote(t.TempDir()) + `
$source = Join-Path $root 'source.exe'
$directory = Join-Path $root 'private'
$destination = Join-Path $directory 'copy.exe'
[IO.File]::WriteAllText($source, 'pinned bytes')
New-Item -ItemType Directory -Path $directory | Out-Null
$user = [Security.Principal.WindowsIdentity]::GetCurrent().User
$privateACL = New-Object Security.AccessControl.DirectorySecurity
$privateACL.SetAccessRuleProtection($true, $false)
$privateACL.AddAccessRule((New-Object Security.AccessControl.FileSystemAccessRule($user, 'FullControl', 'ContainerInherit,ObjectInherit', 'None', 'Allow')))
Set-Acl -LiteralPath $directory -AclObject $privateACL
$world = New-Object Security.Principal.SecurityIdentifier('S-1-1-0')
$sourceACL = Get-Acl -LiteralPath $source
$sourceACL.AddAccessRule((New-Object Security.AccessControl.FileSystemAccessRule($world, 'Modify', 'Allow')))
Set-Acl -LiteralPath $source -AclObject $sourceACL
` + script[start:start+end+len("\n  }\n")] + `
Copy-ControlBinary $source $destination
if ([IO.File]::ReadAllText($destination) -ne 'pinned bytes') { throw 'copied bytes changed' }
foreach ($rule in (Get-Acl -LiteralPath $destination).Access) {
  if ($rule.IdentityReference.Translate([Security.Principal.SecurityIdentifier]).Value -eq 'S-1-1-0' -and ($rule.FileSystemRights -band [Security.AccessControl.FileSystemRights]::WriteData)) { throw 'source write ACL was copied into staging' }
}
`
	if output, err := windowsPowerShellCommand(t, verify).CombinedOutput(); err != nil {
		t.Fatalf("byte-only staging: %v\n%s", err, output)
	}
}
