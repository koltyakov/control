package installation

import (
	"strings"
	"testing"

	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/node"
)

func TestFirewallGatewayPort(t *testing.T) {
	for _, tc := range []struct{ gateway, want string }{
		{"https://gateway.example", "443"},
		{"http://gateway.example", "80"},
		{"wss://gateway.example", "443"},
		{"ws://gateway.example", "80"},
		{"https://gateway.example:7330", "7330"},
		{"https://[::1]:7330", "7330"},
	} {
		if got, err := gatewayPort(tc.gateway); err != nil || got != tc.want {
			t.Fatalf("gateway %q: got %q, %v", tc.gateway, got, err)
		}
	}
	for _, gateway := range []string{"", "gateway.example", "https://", "file:///profile", "ftp://gateway.example:443", "https://gateway.example:0", "https://gateway.example:65536", "https://gateway.example:bad"} {
		if port, err := gatewayPort(gateway); err == nil {
			t.Fatalf("invalid gateway %q accepted as port %q", gateway, port)
		}
	}
}

func TestFirewallUsesConfiguredNextHopIncludingHybrid(t *testing.T) {
	for _, tc := range []struct {
		name, gateway, proxy, want string
		hybrid                     bool
	}{
		{name: "direct", gateway: "https://gateway.example", want: "443"},
		{name: "direct-port", gateway: "https://gateway.example:7443", want: "7443"},
		{name: "proxy", gateway: "https://gateway.example", proxy: "http://10.20.0.5:3128", want: "3128"},
		{name: "hybrid", gateway: "https://gateway.example", proxy: "http://[fd00::5]:8080", hybrid: true, want: "8080"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := node.Config{Gateway: tc.gateway, HybridSystem: tc.hybrid}
			if tc.proxy != "" {
				cfg.GatewayProxy = &model.GatewayProxy{URL: tc.proxy}
			}
			port, err := firewallPort(cfg)
			if err != nil || port != tc.want {
				t.Fatalf("TCP next hop: %q %v", port, err)
			}
			script := windowsFirewallScript(`C:\control\control.exe`, `C:\control\node.json`, `C:\control\runtime\control.exe`, port)
			if strings.Count(script, "Protocol = 'TCP'; RemotePort = '"+tc.want+"'") != 2 {
				t.Fatal("installed CLI and runtime do not both permit the next-hop port")
			}
		})
	}
	for _, proxy := range []string{"http://10.20.0.5:0", "http://user:pass@proxy:3128", ""} {
		if _, err := firewallPort(node.Config{Gateway: "https://gateway.example", GatewayProxy: &model.GatewayProxy{URL: proxy}}); err == nil {
			t.Fatalf("invalid explicit proxy accepted: %q", proxy)
		}
	}
}

func TestWindowsFirewallScope(t *testing.T) {
	binary := `C:\Users\O'Brien\Programs\control\control.exe`
	config := `C:\Users\O'Brien\control profile\node.json`
	runtime := `C:\Users\O'Brien\control profile\state\runtime\control.exe`
	script := windowsFirewallScript(binary, config, runtime, "7330")
	for _, want := range []string{
		powershellLiteral(binary), powershellLiteral(runtime), powershellLiteral("Control node: " + config),
		"Protocol = 'UDP'", "Direction = 'Outbound'; Protocol = 'TCP'; RemotePort = '7330'",
		"-PolicyStore PersistentStore", "EdgeTraversalPolicy = 'Block'", "Profile = 'Any'",
		"Set-NetFirewallRule -InputObject $rule", "New-NetFirewallRule", "$rule.Group -ne $group", "$rule.Description -ne $description",
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("firewall script missing %q: %s", want, script)
		}
	}
	if strings.Count(script, "@{ Name =") != 6 || strings.Count(script, "Direction = 'Inbound'; Protocol = 'UDP'") != 2 || strings.Contains(script, "Direction = 'Inbound'; Protocol = 'TCP'") {
		t.Fatal("firewall must allow only executable-scoped inbound UDP and outbound UDP/gateway TCP")
	}
	if strings.Contains(script, "7331") || strings.Contains(script, "Set-NetFirewallProfile") || strings.Contains(script, "Remove-NetFirewallRule") {
		t.Fatal("firewall must not expose the local API, disable protection, or remove rules")
	}
	if profileName(config) != profileName(strings.ToUpper(config)) {
		t.Fatal("firewall rule names must be case insensitive")
	}
	if profileName(config) == profileName(config+".other") {
		t.Fatal("firewall rules must be profile scoped")
	}
}
