package main

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/koltyakov/control/internal/buildinfo"
	"github.com/koltyakov/control/internal/enrollment"
	"github.com/koltyakov/control/internal/gatewayroute"
	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/node"
	"github.com/koltyakov/control/internal/store"
	"github.com/koltyakov/control/internal/testutil"
	"github.com/koltyakov/control/internal/update"
)

// The target cannot resolve the gateway name: every enrollment request must
// use the selected proxy, and the route is saved with pending state and profile.
func TestProxiedEnrollmentPersistsAndRetainsRoute(t *testing.T) {
	software := buildinfo.Current()
	worker := strings.Repeat("w", 64)
	var proxyURL, gatewayURL string
	var checked bool
	server, pool := testutil.TLSServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/install/ticket01/info":
			_ = json.NewEncoder(w).Encode(enrollment.Invitation{UserID: "fleet", Name: "child", Gateway: gatewayURL, GatewayProxy: &model.GatewayProxy{URL: proxyURL, Worker: worker}, Asset: update.Asset{OS: software.OS, Arch: software.Arch, SHA256: software.SHA256}})
		case "/install/ticket01/check":
			var q enrollment.Redemption
			if json.NewDecoder(r.Body).Decode(&q) != nil || q.GatewayProxy != proxyURL {
				http.Error(w, "missing route", http.StatusConflict)
				return
			}
			checked = true
			w.WriteHeader(http.StatusNoContent)
		case "/v1/node/state":
			_ = json.NewEncoder(w).Encode(model.MachineState{})
		default:
			http.NotFound(w, r)
		}
	}), "gateway.invalid")
	t.Cleanup(gatewayroute.SetRootCAsForTesting(pool))
	_, port, _ := net.SplitHostPort(server.Listener.Addr().String())
	gatewayURL = "https://gateway.invalid:" + port
	proxy := testutil.NewConnectProxy(t, map[string]string{"gateway.invalid:" + port: server.Listener.Addr().String()})
	proxyURL = proxy.URL
	link := gatewayURL + "/install/ticket01"
	ctx := context.Background()

	t.Run("new profile", func(t *testing.T) {
		config := filepath.Join(t.TempDir(), "node.json")
		if _, err := prepareRoutedEnrollment(ctx, config, link, false, nil); err == nil {
			t.Fatal("enrollment reached an unresolvable gateway without a proxy")
		}
		requested := proxyURL
		pending, err := prepareRoutedEnrollment(ctx, config, link, false, &requested)
		if err != nil {
			t.Fatal(err)
		}
		if pending.GatewayProxy == nil || pending.GatewayProxy.URL != proxyURL || pending.GatewayProxy.Worker != worker {
			t.Fatalf("pending route %+v", pending.GatewayProxy)
		}
		var saved pendingEnrollment
		if err = store.Read(filepath.Join(filepath.Dir(config), "pending-enrollment.json"), &saved); err != nil || saved.GatewayProxy == nil || saved.GatewayProxy.URL != proxyURL {
			t.Fatalf("route not saved with pending enrollment: %+v %v", saved.GatewayProxy, err)
		}
		// A retry reuses the saved route without the flag and refuses a different one.
		retry, err := prepareRoutedEnrollment(ctx, config, link, false, nil)
		if err != nil || retry.GatewayProxy == nil || retry.GatewayProxy.URL != proxyURL || retry.Credential != pending.Credential {
			t.Fatalf("retry changed route or credential: %+v %v", retry.GatewayProxy, err)
		}
		direct := "direct"
		if _, err = prepareRoutedEnrollment(ctx, config, link, false, &direct); err == nil || !strings.Contains(err.Error(), "same route") {
			t.Fatalf("retry switched routes: %v", err)
		}
		if err = checkRedemption(ctx, link, pending, nil); err != nil || !checked {
			t.Fatalf("redemption check did not carry the route: %v", err)
		}
		if err = writeNodeConfig(config, pending.Invitation.Name, pending.Invitation.Gateway, pending.Credential, "", false, pending.GatewayProxy); err != nil {
			t.Fatal(err)
		}
		cfg, err := node.LoadConfig(config)
		if err != nil || cfg.GatewayProxy == nil || cfg.GatewayProxy.URL != proxyURL {
			t.Fatalf("profile route %+v %v", cfg.GatewayProxy, err)
		}
	})

	t.Run("re-enrollment preserves route and profile fields", func(t *testing.T) {
		config := filepath.Join(t.TempDir(), "node.json")
		if err := writeNodeConfig(config, "child", gatewayURL, "old-token", "127.0.0.1:7339", false, &model.GatewayProxy{URL: proxyURL, Worker: worker}); err != nil {
			t.Fatal(err)
		}
		var fields map[string]json.RawMessage
		data, _ := os.ReadFile(config)
		_ = json.Unmarshal(data, &fields)
		fields["labels"] = model.JSON(map[string]string{"site": "render"})
		if err := store.Write(config, fields); err != nil {
			t.Fatal(err)
		}
		pending, err := prepareRoutedEnrollment(ctx, config, link, false, nil)
		if err != nil {
			t.Fatal(err)
		}
		var next node.Config
		if err = json.Unmarshal(pending.Config, &next); err != nil {
			t.Fatal(err)
		}
		if next.GatewayProxy == nil || next.GatewayProxy.URL != proxyURL || next.Labels["site"] != "render" || next.Token != pending.Credential {
			t.Fatalf("re-enrollment profile %+v", next)
		}
	})

	t.Run("invitation selection is exact", func(t *testing.T) {
		invitation := enrollment.Invitation{GatewayProxy: &model.GatewayProxy{URL: "http://10.0.0.5:3128", Worker: worker}}
		if _, err := invitationRoute(invitation, nil); err == nil || !strings.Contains(err.Error(), "--gateway-proxy http://10.0.0.5:3128") {
			t.Fatalf("missing route: %v", err)
		}
		if _, err := invitationRoute(invitation, &model.GatewayProxy{URL: "http://10.0.0.6:3128"}); err == nil {
			t.Fatal("another proxy accepted")
		}
		selected, err := invitationRoute(invitation, &model.GatewayProxy{URL: "http://10.0.0.5:3128"})
		if err != nil || selected.Worker != worker {
			t.Fatalf("matching route: %+v %v", selected, err)
		}
		if selected, err = invitationRoute(enrollment.Invitation{}, nil); err != nil || selected != nil {
			t.Fatal("direct invitation changed route")
		}
		manual := &model.GatewayProxy{URL: "http://10.0.0.7:3128"}
		if selected, err = invitationRoute(enrollment.Invitation{}, manual); err != nil || selected != manual {
			t.Fatal("manual route on a direct invitation was not kept")
		}
	})
}
