package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/koltyakov/control/internal/client"
	"github.com/koltyakov/control/internal/gateway"
	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/node"
	"github.com/koltyakov/control/internal/transport"
	"github.com/koltyakov/control/internal/transport/transporttest"
	"github.com/koltyakov/control/internal/vaultwire"
)

// Exercise the actual authorization, dispatch and forward data paths together.
// The second HTTP connection is deliberately made *after* management revocation.
func TestVaultGrantSeparatesFleetCommandsFromForwardTraffic(t *testing.T) {
	for _, relay := range []bool{false, true} {
		t.Run(fmt.Sprintf("relay=%v", relay), func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
			defer cancel()
			const super = "vault-integration-operator-test-12345678"
			gw, err := gateway.New(t.TempDir(), "", gateway.Options{SuperuserKey: super})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = gw.Close() }()
			gatewayHTTP := httptest.NewServer(gw.Handler())
			defer gatewayHTTP.Close()
			var account struct{ Token string }
			admin := client.Admin{URL: gatewayHTTP.URL, Key: super}
			if err := admin.JSON(ctx, http.MethodPost, "/v1/admin/users", map[string]string{"name": "vault-test"}, &account); err != nil || account.Token == "" {
				t.Fatal("create fixture account", err)
			}
			options := transporttest.Loopback()
			for _, name := range []string{"first", "second"} {
				w, err := node.New(node.Config{Name: name, Gateway: gatewayHTTP.URL, Token: account.Token, WorkDir: t.TempDir(), DataDir: t.TempDir(), RelayOnly: relay}, options)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = w.Close() }()
				if err := w.Start(ctx); err != nil {
					t.Fatal(err)
				}
			}
			private := t.TempDir()
			cfg := client.StandaloneConfig{Gateway: client.Admin{URL: gatewayHTTP.URL, Key: account.Token}, StateDir: filepath.Join(private, "clients"), AccountRouting: true, RelayOnly: relay, PeerOptions: []transport.Options{options}}
			tlsFixture := httptest.NewTLSServer(http.NotFoundHandler())
			cert := tlsFixture.TLS.Certificates[0]
			tlsFixture.Close()
			pub, key, err := ed25519.GenerateKey(rand.Reader)
			if err != nil {
				t.Fatal(err)
			}
			l, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			brokerCtx, stop := context.WithCancel(ctx)
			done := make(chan error, 1)
			tunnels := filepath.Join(private, "tunnels")
			go func() { done <- serveVault(brokerCtx, l, cert, pub, cfg, tunnels) }()
			defer func() {
				stop()
				if err := <-done; err != nil && brokerCtx.Err() != err {
					t.Errorf("stop broker: %v", err)
				}
			}()
			address, pin := l.Addr().String(), vaultwire.Pin(cert.Certificate[0])
			grantee, err := vaultwire.Dial(ctx, address, pin, "grantee")
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = grantee.Close() }()
			other, err := vaultwire.Dial(ctx, address, pin, "other")
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = other.Close() }()
			_, err = grantee.Do(ctx, "tunnel.list", json.RawMessage(`{}`))
			entry, pending := vaultwire.ApprovalRequired(err)
			if !pending || entry.ID != grantee.ID() {
				t.Fatalf("first management call did not request approval for its connection: %v", err)
			}
			grant, err := other.GrantEntry(ctx, key, entry, time.Hour)
			if err != nil {
				t.Fatal(err)
			}
			_, err = other.Do(ctx, "tunnel.list", json.RawMessage(`{}`))
			if pending, ok := vaultwire.ApprovalRequired(err); !ok || pending.ID != other.ID() {
				t.Fatalf("approving connection must get its own approval request: %v", err)
			}
			for _, name := range []string{"first", "second"} {
				raw, err := grantee.Do(ctx, "call", model.JSON(map[string]any{"target": name, "method": "node.describe", "params": map[string]any{}}))
				var info struct{ Name string }
				if err != nil || json.Unmarshal(raw, &info) != nil || info.Name != name {
					t.Fatalf("granted call to %s: %s, %v", name, raw, err)
				}
			}
			app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "through the forward") }))
			defer app.Close()
			raw, err := grantee.Do(ctx, "forward.start", model.JSON(client.ForwardSpec{Node: "first", Address: strings.TrimPrefix(app.URL, "http://")}))
			var f client.ForwardInfo
			if err != nil || json.Unmarshal(raw, &f) != nil || f.Listen == "" {
				t.Fatalf("start forward: %s, %v", raw, err)
			}
			get := func() {
				t.Helper()
				h := &http.Client{Timeout: 4 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
				r, err := h.Get("http://" + f.Listen)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = r.Body.Close() }()
				b, err := io.ReadAll(r.Body)
				if err != nil || string(b) != "through the forward" {
					t.Fatalf("traffic: %q, %v", b, err)
				}
			}
			get()
			if err := other.Revoke(ctx, key, grant.ID); err != nil {
				t.Fatal(err)
			}
			_, err = grantee.Do(ctx, "forward.stop", model.JSON(map[string]string{"id": f.ID}))
			var denied *vaultwire.Error
			if !errors.As(err, &denied) || denied.Code != vaultwire.CodeGrantRevoked {
				t.Fatalf("expected broker refusal after revocation, got %v", err)
			}
			get()
		})
	}
}
