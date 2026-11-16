package gateway

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/koltyakov/control/internal/model"
)

func TestConfiguredProxyRouteIsOwnerOnlyAndSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	g, server := gatewayAt(t, dir)
	t.Cleanup(server.Close)
	t.Cleanup(func() { _ = g.Close() })
	alice, a := createTestUser(t, server, "alice")
	_, bob := createTestUser(t, server, "bob")
	var key struct{ Token string }
	if err := a.JSON(t.Context(), http.MethodPost, "/v1/fleet/keys", map[string]string{"name": "peer"}, &key); err != nil {
		t.Fatal(err)
	}
	proxy := &model.GatewayProxy{URL: "http://10.0.0.5:3128", Worker: strings.Repeat("a", 64)}
	ws, id, ok := proxyNodePeer(t, server.URL, key.Token, model.Node{Name: "child", GatewayProxy: proxy})
	if !ok {
		t.Fatal("configured route rejected")
	}
	check := func(admin testAdmin, want bool) {
		t.Helper()
		var status model.PoolActivitySnapshot
		if err := admin.JSON(t.Context(), http.MethodGet, "/v1/status", nil, &status); err != nil {
			t.Fatal(err)
		}
		found := false
		for _, n := range status.Nodes {
			if n.GatewayProxy != nil {
				found = true
				if *n.GatewayProxy != *proxy || n.ID != id.ID {
					t.Fatalf("incorrect route: %+v", n.GatewayProxy)
				}
			}
		}
		if found != want {
			t.Fatalf("route disclosure: got %t, want %t", found, want)
		}
		var directory []model.Node
		if err := admin.JSON(t.Context(), http.MethodGet, "/v1/nodes", nil, &directory); err != nil {
			t.Fatal(err)
		}
		for _, n := range directory {
			if n.GatewayProxy != nil {
				t.Fatal("owner metadata leaked into peer discovery")
			}
		}
		if admin.Key == a.Key || admin.Key == key.Token {
			var peer model.Node
			if err := admin.JSON(t.Context(), http.MethodGet, "/v1/peers/"+id.ID, nil, &peer); err != nil || peer.GatewayProxy != nil {
				t.Fatal("peer authentication exposed owner proxy configuration", err)
			}
		}
	}
	check(a, true)
	check(testAdmin{URL: server.URL, Key: key.Token}, false)
	check(bob, false)
	check(testAdmin{URL: server.URL, Key: superKey}, false)
	_ = ws.CloseNow()
	waitSessionClosed(t, g, id.ID)
	check(a, true) // Offline is not the same as unconfigured.
	server.Close()
	if err := g.Close(); err != nil {
		t.Fatal(err)
	}
	g, server = gatewayAt(t, dir)
	t.Cleanup(server.Close)
	a.URL = server.URL
	check(a, true)
	entries := g.observations(alice.ID)
	if len(entries) != 1 || entries[0].node.Online {
		t.Fatal("restart did not retain the offline registration")
	}
}

func TestRegistrationRejectsInvalidConfiguredProxyMetadata(t *testing.T) {
	_, server := installationFixture(t)
	for _, node := range []model.Node{
		{Name: "credentials", GatewayProxy: &model.GatewayProxy{URL: "http://user:secret@10.0.0.5:3128"}},
		{Name: "noncanonical", GatewayProxy: &model.GatewayProxy{URL: "http://10.0.0.5:3128/"}},
		{Name: "chain", GatewayProxy: &model.GatewayProxy{URL: "http://10.0.0.5:3128"}, ProxyEndpoints: []model.ProxyEndpoint{{URL: "http://10.0.0.6:3128"}}},
	} {
		t.Run(node.Name, func(t *testing.T) {
			if _, _, ok := proxyNodePeer(t, server.URL, commonKey, node); ok {
				t.Fatal("invalid proxy metadata accepted")
			}
		})
	}
}

// Registering directly clears the old route rather than keeping a stale
// enrollment dependency as a claim about the current connection configuration.
func TestDirectRegistrationClearsConfiguredProxyRoute(t *testing.T) {
	g, server := installationFixture(t)
	ws, id, ok := proxyNodePeer(t, server.URL, commonKey, model.Node{Name: "child", GatewayProxy: &model.GatewayProxy{URL: "http://10.0.0.5:3128"}})
	if !ok {
		t.Fatal("registration failed")
	}
	_ = ws.CloseNow()
	waitSessionClosed(t, g, id.ID)
	direct := rawPeer(t, server.URL, commonKey, "child", legacyUser, id)
	if direct == nil {
		t.Fatal("direct re-registration failed")
	}
	t.Cleanup(func() { _ = direct.CloseNow() })
	var status model.PoolActivitySnapshot
	if err := (testAdmin{URL: server.URL, Key: superKey}).JSON(context.Background(), http.MethodGet, "/v1/status", nil, &status); err != nil {
		t.Fatal(err)
	}
	if len(status.Nodes) != 1 || status.Nodes[0].GatewayProxy != nil {
		t.Fatal("direct registration retained a stale proxy chip")
	}
}
