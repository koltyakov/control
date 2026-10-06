package installation

import (
	"fmt"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
)

// WindowsRuntimePath is the stable executable path covered by the node's
// firewall rules. The supervisor publishes verified copies here between runs.
func WindowsRuntimePath(dir string) string {
	return filepath.Join(dir, "runtime", "control.exe")
}

func gatewayPort(gateway string) (string, error) {
	u, err := url.Parse(gateway)
	if err != nil || u.Hostname() == "" {
		return "", fmt.Errorf("invalid firewall gateway URL")
	}
	if u.Scheme != "https" && u.Scheme != "http" && u.Scheme != "wss" && u.Scheme != "ws" {
		return "", fmt.Errorf("unsupported firewall gateway scheme")
	}
	port := u.Port()
	if port == "" {
		switch u.Scheme {
		case "https", "wss":
			port = "443"
		case "http", "ws":
			port = "80"
		default:
			return "", fmt.Errorf("unsupported firewall gateway scheme")
		}
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return "", fmt.Errorf("invalid firewall gateway port")
	}
	return strconv.Itoa(n), nil
}

func windowsFirewallScript(binary, config, runtime, port string) string {
	group := "Control node: " + config
	var rules []string
	for i, program := range []string{binary, runtime} {
		for _, direction := range []string{"Inbound", "Outbound"} {
			rules = append(rules, fmt.Sprintf("@{ Name = %s; Program = %s; Direction = '%s'; Protocol = 'UDP'; RemotePort = 'Any' }",
				powershellLiteral(fmt.Sprintf("ControlFirewall-%s-%d-%s-UDP", profileName(config), i, direction)), powershellLiteral(program), direction))
		}
		rules = append(rules, fmt.Sprintf("@{ Name = %s; Program = %s; Direction = 'Outbound'; Protocol = 'TCP'; RemotePort = %s }",
			powershellLiteral(fmt.Sprintf("ControlFirewall-%s-%d-Outbound-TCP", profileName(config), i)), powershellLiteral(program), powershellLiteral(port)))
	}
	return fmt.Sprintf(`$group = %s
$description = 'Managed by control: WebRTC UDP and outbound gateway TCP only'
$specs = @(
%s
)
$existing = @(Get-NetFirewallRule -PolicyStore PersistentStore -ErrorAction Stop | Where-Object { $_.Name -in $specs.Name })
# Check every name before making changes. Never take over unrelated rules.
foreach ($rule in $existing) {
  if ($rule.Group -ne $group -or $rule.Description -ne $description) { throw 'Control firewall rule belongs to another installation' }
}
foreach ($spec in $specs) {
  $rule = $existing | Where-Object { $_.Name -eq $spec.Name }
  $settings = @{ Program = $spec.Program; Direction = $spec.Direction; Protocol = $spec.Protocol; RemotePort = $spec.RemotePort; LocalPort = 'Any'; LocalAddress = 'Any'; RemoteAddress = 'Any'; Action = 'Allow'; Enabled = 'True'; Profile = 'Any'; EdgeTraversalPolicy = 'Block' }
  if ($rule) {
    Set-NetFirewallRule -InputObject $rule @settings -ErrorAction Stop | Out-Null
  } else {
    New-NetFirewallRule -PolicyStore PersistentStore -Name $spec.Name -DisplayName $spec.Name -Group $group -Description $description @settings -ErrorAction Stop | Out-Null
  }
}
`, powershellLiteral(group), strings.Join(rules, ",\n"))
}
