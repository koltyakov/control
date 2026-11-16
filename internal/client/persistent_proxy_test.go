package client

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/koltyakov/control/internal/gateway"
	"github.com/koltyakov/control/internal/gatewayroute"
	"github.com/koltyakov/control/internal/node"
	"github.com/koltyakov/control/internal/store"
	"github.com/koltyakov/control/internal/testutil"
)

func TestPersistentTunnelServiceRetainsProxyAfterRestart(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 40*time.Second)
	defer cancel()
	const key = "persistent-proxy-superuser-key-1234567890"
	g, err := gateway.New(t.TempDir(), "", gateway.Options{SuperuserKey: key})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = g.Close() }()
	server, pool := testutil.TLSServer(t, g.Handler(), "gateway.invalid")
	t.Cleanup(gatewayroute.SetRootCAsForTesting(pool))
	address := server.Listener.Addr().String()
	_, port, _ := net.SplitHostPort(address)
	gatewayURL := "https://gateway.invalid:" + port
	proxy := testutil.NewConnectProxy(t, map[string]string{"gateway.invalid:" + port: address})
	route, err := gatewayroute.New(proxy.URL)
	if err != nil {
		t.Fatal(err)
	}
	worker, err := node.New(node.Config{
		Name: "worker", Gateway: server.URL, Token: key,
		DataDir: t.TempDir(), WorkDir: t.TempDir(), RelayOnly: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = worker.Close() }()
	if err = worker.Start(ctx); err != nil {
		t.Fatal(err)
	}
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "retained proxy")
	}))
	defer destination.Close()
	cfg := StandaloneConfig{
		Gateway:  Admin{URL: gatewayURL, Key: key, Route: route},
		StateDir: t.TempDir(), RelayOnly: true, AccountRouting: true,
	}
	var stop context.CancelFunc
	var done chan error
	launch := func(_ context.Context, dir string) error {
		var serviceCtx context.Context
		serviceCtx, stop = context.WithCancel(ctx)
		done = make(chan error, 1)
		go func() { done <- RunTunnelService(serviceCtx, dir) }()
		return nil
	}
	defer func() {
		if stop != nil {
			stop()
			if err := <-done; err != nil {
				t.Error(err)
			}
		}
	}()
	c := (Client{}).WithStandalone(ctx, cfg).WithPersistentTunnels(t.TempDir(), launch)
	defer func() { _ = c.Close() }()
	spec := PersistentTunnelSpec{ID: "proxy", ForwardSpec: ForwardSpec{
		Node: "worker", Address: destination.Listener.Addr().String(), Listen: fixedTunnelAddress(t),
	}}
	if _, err = c.StartPersistentTunnel(ctx, spec); err != nil {
		t.Fatal(err)
	}
	dir, err := c.tunnelDirectory()
	if err != nil {
		t.Fatal(err)
	}
	var saved StandaloneConfig
	if err = store.Read(filepath.Join(dir, "service.json"), &saved); err != nil || saved.Gateway.Route.Proxy() != proxy.URL {
		t.Fatalf("saved tunnel route: %q %v", saved.Gateway.Route.Proxy(), err)
	}
	httpClient := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{Proxy: nil}}
	defer httpClient.CloseIdleConnections()
	for round := range 2 {
		waitPersistent(t, ctx, c, 1)
		resp, err := httpClient.Get("http://" + spec.Listen)
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil || string(body) != "retained proxy" {
			t.Fatalf("round %d: %q %v", round, body, err)
		}
		if round == 0 {
			stop()
			if err = <-done; err != nil {
				t.Fatal(err)
			}
			stop = nil
		}
	}
	if len(proxy.Targets()) < 2 || proxy.PlaintextRequests() != 0 {
		t.Fatal("tunnel service did not retain its HTTPS proxy route")
	}
}
