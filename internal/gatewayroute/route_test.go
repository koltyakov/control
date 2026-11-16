package gatewayroute

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/koltyakov/control/internal/testutil"
)

func gatewayFixture(t *testing.T) (gateway string, proxy *testutil.ConnectProxy) {
	t.Helper()
	server, pool := testutil.TLSServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			_, _ = io.WriteString(w, "ok\n")
			return
		}
		_, _ = io.WriteString(w, "gateway")
	}), "gateway.invalid")
	t.Cleanup(SetRootCAsForTesting(pool))
	_, port, _ := net.SplitHostPort(server.Listener.Addr().String())
	// The .invalid name never resolves locally; only the proxy maps it.
	gateway = "https://gateway.invalid:" + port
	proxy = testutil.NewConnectProxy(t, map[string]string{"gateway.invalid:" + port: server.Listener.Addr().String()})
	return gateway, proxy
}

func TestProxiedRouteIgnoresBypassVariablesAndNeverFallsBack(t *testing.T) {
	gateway, proxy := gatewayFixture(t)
	t.Setenv("NO_PROXY", "*")
	t.Setenv("no_proxy", "*")
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:9")
	route, err := New(proxy.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := route.Client(5*time.Second, false).Get(gateway + "/v1/anything")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if string(body) != "gateway" || len(proxy.Targets()) == 0 {
		t.Fatalf("request bypassed the explicit proxy: %q %v", body, proxy.Targets())
	}
	// A loopback URL is directly reachable. The proxied route must still use
	// the proxy, which refuses it, rather than connecting directly.
	u, _ := url.Parse(gateway)
	direct := "https://127.0.0.1:" + u.Port()
	if _, err = route.Client(5*time.Second, false).Get(direct + "/healthz"); err == nil {
		t.Fatal("proxied route connected directly to a target the proxy refused")
	}
	proxy.Close()
	if _, err = route.Client(5*time.Second, false).Get(gateway + "/healthz"); err == nil {
		t.Fatal("proxied route succeeded after the proxy stopped")
	}
}

func TestProbeChecksConnectTLSAndHealth(t *testing.T) {
	gateway, proxy := gatewayFixture(t)
	if err := Probe(context.Background(), proxy.URL, gateway); err != nil {
		t.Fatal(err)
	}
	// The proxy's allowlist refuses other destinations.
	if err := Probe(context.Background(), proxy.URL, "https://other.invalid:443"); err == nil || err.Error() != "proxy refused CONNECT: forbidden" {
		t.Fatalf("refused CONNECT: %v", err)
	}
	if err := Probe(context.Background(), proxy.URL, "http://gateway.invalid"); err == nil || !strings.Contains(err.Error(), "HTTPS") {
		t.Fatalf("plain HTTP gateway accepted: %v", err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	unused := "http://" + listener.Addr().String()
	_ = listener.Close()
	if err := Probe(context.Background(), unused, gateway); err == nil || err.Error() != "proxy connection failed" {
		t.Fatalf("unreachable proxy: %v", err)
	}
}

func TestProbeRequiresGatewayCertificateVerification(t *testing.T) {
	gateway, proxy := gatewayFixture(t)
	// Drop the test CA: verification stays mandatory and must fail closed.
	t.Cleanup(SetRootCAsForTesting(x509.NewCertPool()))
	err := Probe(context.Background(), proxy.URL, gateway)
	if err == nil || err.Error() != "gateway TLS verification failed" {
		t.Fatalf("untrusted gateway certificate: %v", err)
	}
	if strings.Contains(err.Error(), "127.0.0.1") || strings.Contains(err.Error(), "invalid") {
		t.Fatal("sanitized error leaked an address")
	}
}

func TestRouteValidationAndMonitoring(t *testing.T) {
	for _, value := range []string{"https://proxy:3128", "http://user:pass@proxy:3128", "http://proxy", "http://proxy:3128/path", "http://[fe80::1%25eth0]:3128", "http://0.0.0.0:3128"} {
		if _, err := New(value); err == nil {
			t.Errorf("accepted %q", value)
		}
	}
	route, err := New("http://[2001:DB8::1]:3128/")
	if err != nil || route.Proxy() != "http://[2001:db8::1]:3128" {
		t.Fatalf("IPv6 normalization: %q %v", route.Proxy(), err)
	}
	if host, port := route.MonitorHost("https://gateway.example"); host != "2001:db8::1" || port != "3128" {
		t.Fatalf("proxied route monitors %s:%s", host, port)
	}
	if host, port := (Route{}).MonitorHost("https://gateway.example"); host != "gateway.example" || port != "443" {
		t.Fatalf("direct route monitors %s:%s", host, port)
	}
	if err = route.CheckGateway("http://gateway.example"); err == nil {
		t.Fatal("proxied route accepted a plain HTTP gateway")
	}
	if (Route{}).Transport() != http.DefaultTransport {
		t.Fatal("direct route changed the process default transport")
	}
}

func TestProxiedRouteRejectsPlainHTTPAndDowngrade(t *testing.T) {
	server, pool := testutil.TLSServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://"+r.Host+"/cleartext", http.StatusFound)
	}))
	t.Cleanup(SetRootCAsForTesting(pool))
	address := server.Listener.Addr().String()
	proxy := testutil.NewConnectProxy(t, map[string]string{address: address})
	route, err := New(proxy.URL)
	if err != nil {
		t.Fatal(err)
	}
	for _, method := range []string{"client", "do", "initial-http"} {
		t.Run(method, func(t *testing.T) {
			target := server.URL + "/v1/auth"
			if method == "initial-http" {
				target = strings.Replace(target, "https://", "http://", 1)
			}
			req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, target, nil)
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Authorization", "Bearer private-gateway-key")
			var resp *http.Response
			if method == "do" {
				resp, err = route.Do(req)
			} else {
				resp, err = route.Client(5*time.Second, true).Do(req)
			}
			if resp != nil {
				_ = resp.Body.Close()
			}
			if err == nil || !strings.Contains(err.Error(), "TLS verification is mandatory") {
				t.Fatalf("plaintext gateway request was not rejected: %v", err)
			}
			if proxy.PlaintextRequests() != 0 {
				t.Fatal("proxy received a plaintext gateway request")
			}
		})
	}
}

func TestProxiedRoutePreservesHTTPSRedirectsAndLimit(t *testing.T) {
	server, pool := testutil.TLSServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/start":
			http.Redirect(w, r, "/done", http.StatusFound)
		case "/loop":
			http.Redirect(w, r, "/loop", http.StatusFound)
		default:
			_, _ = io.WriteString(w, "secure")
		}
	}))
	t.Cleanup(SetRootCAsForTesting(pool))
	address := server.Listener.Addr().String()
	proxy := testutil.NewConnectProxy(t, map[string]string{address: address})
	route, err := New(proxy.URL)
	if err != nil {
		t.Fatal(err)
	}
	client := route.Client(5*time.Second, true)
	resp, err := client.Get(server.URL + "/start")
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil || string(body) != "secure" {
		t.Fatalf("HTTPS redirect: %q %v", body, err)
	}
	resp, err = client.Get(server.URL + "/loop")
	if resp != nil {
		_ = resp.Body.Close()
	}
	if err == nil || !strings.Contains(err.Error(), "stopped after 10 redirects") {
		t.Fatalf("default redirect limit changed: %v", err)
	}
}

func TestRouteJSONPreservesProxyAndValidatesRestoration(t *testing.T) {
	route, err := New("http://10.20.0.5:3128")
	if err != nil {
		t.Fatal(err)
	}
	if !route.ParkIfIdle(time.Now().Add(time.Minute)) {
		t.Fatal("could not park route before serialization")
	}
	data, err := json.Marshal(route)
	if err != nil {
		t.Fatal(err)
	}
	var restored Route
	if err = json.Unmarshal(data, &restored); err != nil || restored.Proxy() != route.Proxy() {
		t.Fatalf("restored route: %q %v", restored.Proxy(), err)
	}
	if restored.maintenance == route.maintenance || restored.maintenance == nil || !restored.maintenance.until.IsZero() {
		t.Fatal("restoration retained process-local maintenance state")
	}
	for _, legacy := range []string{`{}`, `null`} {
		if err = json.Unmarshal([]byte(legacy), &restored); err != nil || restored.Proxied() {
			t.Fatalf("legacy direct route %s: %+v %v", legacy, restored, err)
		}
	}
	for _, invalid := range []string{`{"proxy":"https://proxy:3128"}`, `{"proxy":"http://user:pass@proxy:3128"}`, `{"proxy":false}`} {
		restored = route
		if err = json.Unmarshal([]byte(invalid), &restored); err == nil || restored != route {
			t.Fatalf("invalid route changed the configured next hop: %s %v", invalid, err)
		}
	}
}
