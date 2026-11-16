package enrollment

import (
	"encoding/pem"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/testutil"
	"github.com/koltyakov/control/internal/update"
)

// Every stage passes the selected proxy explicitly: environment does not
// survive sudo, UAC, or limited-token helpers, and NO_PROXY must not bypass it.
func TestProxiedInstallerPinsProxyForEveryStage(t *testing.T) {
	const proxy = "http://[fd00::5]:3128"
	const link = "https://gateway.example/install/token1"
	for _, platform := range []string{"linux", "darwin", "windows"} {
		for _, mode := range []string{"", "user", "system", "hybrid"} {
			for _, role := range []string{"", "system"} {
				if role == "system" && mode != "system" {
					continue
				}
				i := Invitation{ServiceMode: mode, HybridRole: role, HybridID: strings.Repeat("b", 64), GatewayProxy: &model.GatewayProxy{URL: proxy, Worker: strings.Repeat("c", 64)}, Asset: update.Asset{OS: platform, Arch: "amd64", SHA256: strings.Repeat("a", 64)}}
				if mode == "hybrid" {
					i.HybridRole = "user"
				}
				script, err := Script(i, link)
				if err != nil {
					t.Fatal(err)
				}
				name := platform + "/" + mode + "/" + role
				command := InstallCommand(link, platform == "windows", mode, proxy)
				features := strings.Index(script, "__installer-features")
				if features < 0 {
					t.Fatalf("%s: no installer capability check", name)
				}
				for line := range strings.SplitSeq(script, "\n") {
					trimmed := strings.TrimSpace(line)
					if strings.HasPrefix(trimmed, "#") {
						continue
					}
					if strings.Contains(line, "curl ") && !strings.Contains(line, "--proxy '"+proxy+"' --noproxy '' --proto '=https' --proto-redir '=https'") {
						t.Fatalf("%s: unpinned download: %s", name, line)
					}
					if (strings.Contains(line, "Invoke-WebRequest") || strings.Contains(line, "Invoke-RestMethod")) && !strings.Contains(line, "-Proxy '"+proxy+"'") {
						t.Fatalf("%s: unpinned PowerShell download: %s", name, line)
					}
					if (strings.Contains(line, "Invoke-WebRequest") || strings.Contains(line, "Invoke-RestMethod")) && !strings.Contains(line, "-MaximumRedirection 0") {
						t.Fatalf("%s: PowerShell download permits redirects: %s", name, line)
					}
					if (strings.Contains(line, " enroll --url") || strings.Contains(line, "__hybrid-user --url")) && !strings.Contains(line, "--gateway-proxy '"+proxy+"'") {
						t.Fatalf("%s: enrollment without route: %s", name, line)
					}
					if (strings.Contains(line, " enroll --url") || strings.Contains(line, "__hybrid-user --url")) && strings.Index(script, line) < features {
						t.Fatalf("%s: enrollment precedes the capability check", name)
					}
				}
				if platform == "windows" {
					if command != "irm -Proxy '"+proxy+"' -MaximumRedirection 0 -ErrorAction Stop '"+link+"'|iex" {
						t.Fatalf("%s: command %s", name, command)
					}
				} else if !strings.HasPrefix(command, "curl -fsSL --proxy '"+proxy+"' --noproxy '' --proto '=https' --proto-redir '=https' '"+link+"' | ") {
					t.Fatalf("%s: command %s", name, command)
				}
				if platform != "windows" {
					if bash, err := exec.LookPath("bash"); err == nil {
						path := filepath.Join(t.TempDir(), "install.sh")
						if err = os.WriteFile(path, []byte(script), 0600); err != nil {
							t.Fatal(err)
						}
						if out, err := exec.Command(bash, "-n", path).CombinedOutput(); err != nil {
							t.Fatalf("%s: bash syntax: %v %s", name, err, out)
						}
					}
				}
			}
		}
	}
}

func TestProxiedInstallerRejectsHTTPSDowngrade(t *testing.T) {
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("curl is required to exercise the Unix installer")
	}
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash is required to exercise the Unix installer")
	}
	server, _ := testutil.TLSServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://"+r.Host+"/untrusted-script", http.StatusFound)
	}))
	ca := filepath.Join(t.TempDir(), "gateway.pem")
	if err = os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.TLS.Certificates[0].Certificate[1]}), 0600); err != nil {
		t.Fatal(err)
	}
	address := server.Listener.Addr().String()
	proxy := testutil.NewConnectProxy(t, map[string]string{address: address})
	cmd := exec.CommandContext(t.Context(), bash, "-o", "pipefail", "-c", InstallCommand(server.URL+"/install/ticket", false, "", proxy.URL))
	cmd.Env = append(os.Environ(), "CURL_CA_BUNDLE="+ca)
	output, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(strings.ToLower(string(output)), "disabled") {
		t.Fatalf("curl did not reject the protocol downgrade: %v\n%s", err, output)
	}
	if proxy.PlaintextRequests() != 0 {
		t.Fatal("installer sent its ticket to the proxy in plaintext")
	}
}

func TestDirectInstallerIsUnchanged(t *testing.T) {
	for _, platform := range []string{"linux", "windows"} {
		i := Invitation{Asset: update.Asset{OS: platform, Arch: "amd64", SHA256: strings.Repeat("a", 64)}}
		script, err := Script(i, "https://gateway.example/install/token1")
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(script, "proxy") || strings.Contains(script, "__installer-features") {
			t.Fatalf("%s direct installer references a proxy", platform)
		}
	}
}

func TestRedemptionSignatureKeepsLegacyBytes(t *testing.T) {
	r := Redemption{PublicKey: []byte{1}, CredentialHash: "h"}
	if strings.Contains(string(Message("t", r)), "gatewayProxy") || strings.Contains(string(CheckMessage("t", r)), "gatewayProxy") {
		t.Fatal("direct redemption changed legacy signature bytes")
	}
	r.GatewayProxy = "http://10.0.0.5:3128"
	if !strings.Contains(string(Message("t", r)), "gatewayProxy") || !strings.Contains(string(CheckMessage("t", r)), "gatewayProxy") {
		t.Fatal("proxied redemption must sign its route")
	}
}
