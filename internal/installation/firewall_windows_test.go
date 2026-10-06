package installation

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestWindowsFirewallRegistration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// Parse and execute the generated script without touching the real firewall
	// or requesting elevation. Keep mock rules to test repeated registration.
	mocks := `$script:rules = New-Object 'System.Collections.Generic.List[object]'
$script:created = 0
$script:updated = 0
function Get-NetFirewallRule { param($PolicyStore, $ErrorAction) $script:rules.ToArray() }
function New-NetFirewallRule {
  param($PolicyStore, $Name, $DisplayName, $Group, $Description, $Program, $Direction, $Protocol, $RemotePort, $LocalPort, $LocalAddress, $RemoteAddress, $Action, $Enabled, $Profile, $EdgeTraversalPolicy, $ErrorAction)
  $script:rules.Add([pscustomobject]@{Name=$Name;Group=$Group;Description=$Description;Program=$Program;Direction=$Direction;Protocol=$Protocol;RemotePort=$RemotePort;LocalPort=$LocalPort;Action=$Action;Profile=$Profile;EdgeTraversalPolicy=$EdgeTraversalPolicy})
  $script:created++
}
function Set-NetFirewallRule {
  param($InputObject, $Program, $Direction, $Protocol, $RemotePort, $LocalPort, $LocalAddress, $RemoteAddress, $Action, $Enabled, $Profile, $EdgeTraversalPolicy, $ErrorAction)
  if ($InputObject.Program -ne $Program -or $InputObject.Direction -ne $Direction -or $InputObject.Protocol -ne $Protocol) { throw 'Wrong rule selected' }
  $script:updated++
}
`
	binary := `C:\Users\O'Brien\control.exe`
	runtime := `C:\Users\O'Brien\state\runtime\control.exe`
	script := windowsFirewallScript(binary, `C:\Users\O'Brien\node.json`, runtime, "7330")
	output, err := userPowerShellOutput(ctx, mocks+script+script+`[pscustomobject]@{Created=$script:created;Updated=$script:updated;Rules=$script:rules.ToArray()} | ConvertTo-Json -Depth 3 -Compress`)
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		Created, Updated int
		Rules            []struct {
			Program, Direction, Protocol, RemotePort, LocalPort, Action, Profile, EdgeTraversalPolicy string
		}
	}
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatalf("parse rules: %v: %s", err, output)
	}
	if result.Created != 6 || result.Updated != 6 || len(result.Rules) != 6 {
		t.Fatalf("registration is not idempotent: %s", output)
	}
	for _, rule := range result.Rules {
		if (rule.Program != binary && rule.Program != runtime) || rule.Action != "Allow" || rule.Profile != "Any" || rule.EdgeTraversalPolicy != "Block" || rule.LocalPort != "Any" {
			t.Fatalf("unexpected rule: %+v", rule)
		}
		if rule.Protocol == "TCP" && (rule.Direction != "Outbound" || rule.RemotePort != "7330") {
			t.Fatalf("TCP rule exposes more than gateway egress: %+v", rule)
		}
	}
}

func TestWindowsFirewallRejectsForeignRules(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	config := `C:\control\node.json`
	mocks := `function Get-NetFirewallRule {
  param($PolicyStore, $ErrorAction)
  [pscustomobject]@{ Name = ` + powershellLiteral("ControlFirewall-"+profileName(config)+"-0-Inbound-UDP") + `; Group = 'Other'; Description = 'Other' }
}
function New-NetFirewallRule { throw 'unexpected mutation' }
function Set-NetFirewallRule { throw 'unexpected mutation' }
`
	_, err := userPowerShellOutput(ctx, mocks+windowsFirewallScript(`C:\control\control.exe`, config, `C:\control\runtime\control.exe`, "443"))
	if err == nil || !strings.Contains(err.Error(), "belongs to another installation") || strings.Contains(err.Error(), "unexpected mutation") {
		t.Fatalf("foreign firewall rules must not be changed: %v", err)
	}
}
