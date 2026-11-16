package node

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/koltyakov/control/internal/gateway"
	"github.com/koltyakov/control/internal/gatewayroute"
	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/testutil"
)

// The child's gateway name does not resolve locally and the child has no
// direct route to it. Registration, policy reads, directory lookups, relay
// execution, and the health stream all use the explicit proxy.
func TestProxiedWorkerRegistersReadsPolicyAndExecutesThroughProxy(t *testing.T) {
	g, err := gateway.New(t.TempDir(), testToken, gateway.Options{SuperuserKey: testAccount})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = g.Close() })
	server, pool := testutil.TLSServer(t, g.Handler(), "gateway.invalid")
	t.Cleanup(gatewayroute.SetRootCAsForTesting(pool))
	_, port, _ := net.SplitHostPort(server.Listener.Addr().String())
	childGateway := "https://gateway.invalid:" + port
	proxy := testutil.NewConnectProxy(t, map[string]string{"gateway.invalid:" + port: server.Listener.Addr().String()})
	// Inherited bypass settings must not route gateway traffic directly.
	t.Setenv("NO_PROXY", "gateway.invalid")
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	orchestrator := prepareTestOrchestrator(t, ctx, "https://127.0.0.1:"+port, testAccount, "legacy", true)
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "provider") }))
	t.Cleanup(provider.Close)
	cfg := Config{Name: "child", Gateway: childGateway, Token: testToken, DataDir: t.TempDir(), WorkDir: t.TempDir(), RelayOnly: true, GatewayProxy: &model.GatewayProxy{URL: proxy.URL}}
	cfg.Allow = map[string][]string{orchestrator.owner.ID: {"*"}}
	n, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = n.Close() })
	if err = n.Start(ctx); err != nil {
		t.Fatal(err)
	}
	attachTestOrchestrator(t, n, orchestrator)
	if err = os.WriteFile(filepath.Join(cfg.WorkDir, "file.bin"), []byte("proxied"), 0600); err != nil {
		t.Fatal(err)
	}
	var exported model.Artifact
	call(t, n, "child", "artifacts.export", map[string]any{"path": "file.bin"}, &exported)
	if exported.Size != int64(len("proxied")) {
		t.Fatalf("exported %+v", exported)
	}
	// Provider requests keep their own route; the gateway proxy refuses them.
	var response struct {
		Status int    `json:"status"`
		Body   string `json:"body"`
	}
	call(t, n, "child", "http.request", map[string]any{"url": provider.URL}, &response)
	if response.Status != http.StatusOK {
		t.Fatalf("provider request: %+v", response)
	}
	for _, target := range proxy.Targets() {
		if target != "gateway.invalid:"+port {
			t.Fatalf("proxy saw a non-gateway target %q", target)
		}
	}
	if len(proxy.Targets()) < 2 {
		t.Fatalf("gateway traffic did not use the proxy: %v", proxy.Targets())
	}
	// No direct fallback: losing the proxy disconnects gateway traffic.
	proxy.Close()
	lookup, done := context.WithTimeout(ctx, 5*time.Second)
	defer done()
	if _, err = n.Peer.Nodes(lookup); err == nil {
		t.Fatal("directory read succeeded without the proxy")
	}
}

func TestProxyConfigurationValidation(t *testing.T) {
	base := func() Config {
		return Config{Name: "n", Gateway: "https://gateway.example", Token: "t", DataDir: t.TempDir(), WorkDir: t.TempDir()}
	}
	cfg := base()
	cfg.Gateway = "http://gateway.example"
	cfg.GatewayProxy = &model.GatewayProxy{URL: "http://10.0.0.5:3128"}
	if err := cfg.defaults(); err == nil || !strings.Contains(err.Error(), "https") {
		t.Fatalf("plain HTTP gateway with proxy: %v", err)
	}
	cfg = base()
	cfg.GatewayProxy = &model.GatewayProxy{URL: "http://10.0.0.5:3128"}
	cfg.ProxyEndpoints = []model.ProxyEndpoint{{URL: "http://10.0.0.6:3128"}}
	if err := cfg.defaults(); err == nil || !strings.Contains(err.Error(), "chains") {
		t.Fatalf("proxy chain: %v", err)
	}
	cfg = base()
	cfg.ProxyEndpoints = []model.ProxyEndpoint{{URL: "http://10.0.0.6:3128/", Interface: "office"}, {URL: "http://[FD00::6]:3128"}}
	if err := cfg.defaults(); err != nil {
		t.Fatal(err)
	}
	if cfg.ProxyEndpoints[0].URL != "http://10.0.0.6:3128" || cfg.ProxyEndpoints[1].URL != "http://[fd00::6]:3128" {
		t.Fatalf("endpoints not normalized: %+v", cfg.ProxyEndpoints)
	}
	cfg = base()
	cfg.ProxyEndpoints = []model.ProxyEndpoint{{URL: "http://10.0.0.6:3128"}, {URL: "http://10.0.0.6:3128/"}}
	if err := cfg.defaults(); err == nil {
		t.Fatal("duplicate endpoint accepted")
	}
}

func TestProxyChecksReportAgeAndSanitizedFailures(t *testing.T) {
	checks := newProxyChecks([]model.ProxyEndpoint{{URL: "http://10.0.0.5:3128"}, {URL: "http://10.0.0.6:3128"}})
	checks.probe = func(_ context.Context, proxy, gateway string) error {
		if gateway != "https://gateway.example" {
			t.Fatalf("checked %s", gateway)
		}
		if proxy == "http://10.0.0.6:3128" {
			return errors.New(strings.Repeat("x", 400))
		}
		return nil
	}
	if checks.snapshot(time.Now()) != nil {
		t.Fatal("unchecked endpoints reported as observations")
	}
	checks.checkAll(context.Background(), "https://gateway.example")
	report := checks.snapshot(time.Now().Add(30 * time.Second))
	if len(report) != 2 || report[0].State != model.ProxyReady || report[1].State != model.ProxyUnavailable || len(report[1].Error) != model.MaxProxyErrorBytes {
		t.Fatalf("report %+v", report)
	}
	if report[0].AgeSeconds < 29 || !model.ValidProxyReadiness(report) {
		t.Fatalf("age or validity %+v", report)
	}
	if !slices.ContainsFunc(report, func(r model.ProxyReadiness) bool { return r.URL == "http://10.0.0.5:3128" }) {
		t.Fatal("missing endpoint")
	}
}
