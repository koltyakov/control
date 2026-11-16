package main

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/koltyakov/control/internal/gatewayroute"
	"github.com/koltyakov/control/internal/installation"
	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/node"
	"github.com/koltyakov/control/internal/store"
	"github.com/koltyakov/control/internal/testutil"
)

func TestLoginUsesSavedProxyAndPreservesOperatorAuthorization(t *testing.T) {
	t.Setenv("CONTROL_HOME", t.TempDir())
	for _, name := range []string{"CONTROL_GATEWAY", "CONTROL_USER_KEY", "CONTROL_SUPERUSER_KEY", "CONTROL_TOKEN", "CONTROL_CONFIG"} {
		t.Setenv(name, "")
	}
	t.Setenv("NO_PROXY", "*")
	var operatorReads, userReads atomic.Int32
	server, pool := testutil.TLSServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Header.Get("Authorization") {
		case "Bearer operator-key":
			operatorReads.Add(1)
			_, _ = w.Write([]byte(`{"role":"superuser"}`))
		case "Bearer account-key":
			userReads.Add(1)
			_, _ = w.Write([]byte(`{"role":"user"}`))
		default:
			http.Error(w, "invalid credential", http.StatusUnauthorized)
		}
	}), "gateway.invalid")
	t.Cleanup(gatewayroute.SetRootCAsForTesting(pool))
	address := server.Listener.Addr().String()
	_, port, _ := net.SplitHostPort(address)
	gatewayURL := "https://gateway.invalid:" + port
	proxy := testutil.NewConnectProxy(t, map[string]string{"gateway.invalid:" + port: address})
	config := filepath.Join(t.TempDir(), "node.json")
	if err := store.Write(config, node.Config{Gateway: gatewayURL, GatewayProxy: &model.GatewayProxy{URL: proxy.URL}}); err != nil {
		t.Fatal(err)
	}
	if err := installation.SaveAdmin(installation.AdminProfile{Gateway: gatewayURL, Key: "operator-key"}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if err := run(ctx, []string{"--config", config, "login", "--api-key", "account-key"}); err != nil {
		t.Fatal(err)
	}
	if userReads.Load() != 1 || operatorReads.Load() != 1 || len(proxy.Targets()) == 0 {
		t.Fatal("login or previous operator validation bypassed the saved proxy")
	}
	saved, err := installation.ReadAdmin()
	if err != nil || saved.Key != "account-key" || saved.Gateway != gatewayURL {
		t.Fatalf("account login: %+v %v", saved, err)
	}
	operator, err := installation.ReadUpdateAdmin()
	if err != nil || operator.Key != "operator-key" || operator.Gateway != gatewayURL {
		t.Fatalf("operator authorization: %+v %v", operator, err)
	}
	// With no saved login, the node's own gateway is also a usable default.
	if err = installation.SaveAdmin(installation.AdminProfile{}); err != nil {
		t.Fatal(err)
	}
	if err = run(ctx, []string{"--config", config, "login", "--api-key", "account-key"}); err != nil {
		t.Fatal(err)
	}
	proxy.Close()
	if err = run(ctx, []string{"--config", config, "login"}); err == nil {
		t.Fatal("login fell back to a direct connection after proxy loss")
	}
	after, err := installation.ReadAdmin()
	if err != nil || after != saved {
		t.Fatal("failed proxied login replaced saved credentials", err)
	}
}

func TestLoginDoesNotReuseProxyForDifferentGateway(t *testing.T) {
	t.Setenv("CONTROL_HOME", t.TempDir())
	for _, name := range []string{"CONTROL_GATEWAY", "CONTROL_USER_KEY", "CONTROL_SUPERUSER_KEY", "CONTROL_TOKEN", "CONTROL_CONFIG"} {
		t.Setenv(name, "")
	}
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer other-key" {
			t.Error("login reused a credential from another gateway")
		}
		calls.Add(1)
		_, _ = w.Write([]byte(`{"role":"user"}`))
	}))
	defer server.Close()
	config := filepath.Join(t.TempDir(), "node.json")
	if err := store.Write(config, node.Config{Gateway: "https://original.invalid", GatewayProxy: &model.GatewayProxy{URL: "http://127.0.0.1:1"}}); err != nil {
		t.Fatal(err)
	}
	if err := run(t.Context(), []string{"--config", config, "login", "--gateway", server.URL, "--api-key", "other-key"}); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatal("login did not reach the explicitly selected gateway")
	}
}
