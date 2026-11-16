package gateway

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/koltyakov/control/internal/enrollment"
	"github.com/koltyakov/control/internal/identity"
	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/protocol"
	"google.golang.org/protobuf/proto"
)

func proxyWorkerPeer(t *testing.T, serverURL, token, name, userID string, endpoints []model.ProxyEndpoint) (*websocket.Conn, *identity.Identity, bool) {
	t.Helper()
	id, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(ctx, serverURL+"/v1/connect", &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer " + token}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ws.CloseNow() })
	_, challenge, err := ws.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	n := model.Node{ID: id.ID, PublicKey: id.Public, Name: name, UserID: userID, ProxyEndpoints: endpoints}
	hello := Hello{Version: model.Version, NodeBytes: model.JSON(n)}
	hello.Signature = ed25519.Sign(id.Private, hello.Message(challenge))
	if err = ws.Write(ctx, websocket.MessageBinary, model.JSON(hello)); err != nil {
		t.Fatal(err)
	}
	_, ready, err := ws.Read(ctx)
	return ws, id, err == nil && string(ready) == "ready"
}

func sendProxyHealth(t *testing.T, ws *websocket.Conn, readiness []model.ProxyReadiness) {
	t.Helper()
	data, err := proto.Marshal(&protocol.Packet{Kind: "node.health", Data: model.JSON(model.NodeHealth{Proxies: readiness})})
	if err != nil {
		t.Fatal(err)
	}
	if err = ws.Write(context.Background(), websocket.MessageBinary, data); err != nil {
		t.Fatal(err)
	}
}

// proxyRequest keeps the response text, which testAdmin discards.
func proxyRequest(t *testing.T, url, key string, body, out any) error {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(model.JSON(body)))
	if err != nil {
		t.Fatal(err)
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	text, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode >= 300 {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, text)
	}
	if out != nil {
		return json.Unmarshal(text, out)
	}
	return nil
}

func proxyStatus(t *testing.T, admin testAdmin, id string) []model.ProxyEndpointStatus {
	t.Helper()
	var status model.PoolActivitySnapshot
	if err := admin.JSON(context.Background(), "GET", "/v1/status", nil, &status); err != nil {
		t.Fatal(err)
	}
	for _, n := range status.Nodes {
		if n.ID == id {
			return n.ProxyEndpoints
		}
	}
	return nil
}

func TestProxiedInvitationValidatesFleetReadinessAndPersists(t *testing.T) {
	g, server := installationFixture(t)
	alice, a := createTestUser(t, server, "alice")
	_, b := createTestUser(t, server, "bob")
	ctx := context.Background()
	office, vpn := "http://10.0.0.5:3128", "http://[fd00::5]:3128"
	endpoints := []model.ProxyEndpoint{{URL: office, Interface: "office"}, {URL: vpn, Interface: "vpn"}}
	// Registration accepts only normalized, bounded advertisements.
	if _, _, ok := proxyWorkerPeer(t, server.URL, a.Key, "bad-bridge", alice.ID, []model.ProxyEndpoint{{URL: "http://10.0.0.5:3128/"}}); ok {
		t.Fatal("unnormalized proxy endpoint accepted")
	}
	ws, worker, ok := proxyWorkerPeer(t, server.URL, a.Key, "office-bridge", alice.ID, endpoints)
	if !ok {
		t.Fatal("proxy worker registration rejected")
	}
	request := enrollment.Request{Name: "render-01", OS: "linux", Arch: "amd64", Gateway: "https://gateway.example", ProxyWorker: worker.ID, ProxyURL: office}
	create := func(c testAdmin, q enrollment.Request) (enrollment.Link, error) {
		var link enrollment.Link
		err := proxyRequest(t, server.URL+"/v1/fleet/installations", c.Key, q, &link)
		return link, err
	}
	expect := func(q enrollment.Request, want string) {
		t.Helper()
		if _, err := create(a, q); err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("want %q, got %v", want, err)
		}
	}
	// Advertisement alone is not readiness; the gateway never probes it.
	expect(request, "unchecked")
	if states := proxyStatus(t, a, worker.ID); len(states) != 2 || states[0].State != model.ProxyUnchecked {
		t.Fatalf("unchecked status %+v", states)
	}
	sendProxyHealth(t, ws, []model.ProxyReadiness{
		{URL: office, State: model.ProxyReady, CheckedAt: time.Now().UTC()},
		{URL: vpn, State: model.ProxyUnavailable, CheckedAt: time.Now().UTC(), Error: "proxy refused CONNECT: forbidden"},
		{URL: "http://10.9.9.9:1", State: model.ProxyReady, CheckedAt: time.Now().UTC()},
	})
	deadline := time.Now().Add(3 * time.Second)
	for {
		states := proxyStatus(t, a, worker.ID)
		if len(states) == 2 && states[0].State == model.ProxyReady {
			if states[1].State != model.ProxyUnavailable || states[1].Error == "" {
				t.Fatalf("unavailable status %+v", states)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("readiness not reported: %+v", states)
		}
		time.Sleep(20 * time.Millisecond)
	}
	ambiguous := request
	ambiguous.ProxyURL = ""
	expect(ambiguous, "several endpoints")
	unavailable := request
	unavailable.ProxyURL = vpn
	expect(unavailable, "unavailable")
	foreignURL := request
	foreignURL.ProxyURL = "http://10.9.9.9:1"
	expect(foreignURL, "not advertised")
	arbitrary := request
	arbitrary.ProxyWorker = ""
	expect(arbitrary, "proxyUrl requires proxyWorker")
	plain := request
	plain.Gateway = "http://gateway.example"
	expect(plain, "https")
	// Another fleet cannot select or discover this worker.
	if _, err := create(b, request); err == nil || !strings.Contains(err.Error(), "not a machine in this fleet") {
		t.Fatalf("foreign selection: %v", err)
	}
	var bobInvites []enrollment.Invitation
	if err := b.JSON(ctx, "GET", "/v1/fleet/installations", nil, &bobInvites); err != nil || len(bobInvites) != 0 {
		t.Fatalf("foreign selection left invitation state: %+v %v", bobInvites, err)
	}
	if proxyStatus(t, b, worker.ID) != nil {
		t.Fatal("foreign fleet observed proxy readiness")
	}
	link, err := create(a, request)
	if err != nil {
		t.Fatal(err)
	}
	if link.GatewayProxy == nil || link.GatewayProxy.URL != office || link.GatewayProxy.Worker != worker.ID {
		t.Fatalf("selection not echoed: %+v", link.GatewayProxy)
	}
	if !strings.Contains(link.Command, "--proxy '"+office+"' --noproxy ''") {
		t.Fatalf("command does not pin the proxy: %s", link.Command)
	}
	ticket := link.URL[strings.LastIndex(link.URL, "/")+1:]
	local := server.URL + "/install/" + ticket
	resp, err := http.Get(local)
	if err != nil {
		t.Fatal(err)
	}
	script := make([]byte, 64<<10)
	n, _ := resp.Body.Read(script)
	_ = resp.Body.Close()
	if !strings.Contains(string(script[:n]), "--gateway-proxy '"+office+"'") {
		t.Fatal("generated script does not pass the selected proxy to enrollment")
	}
	// An installer that omits the selection is refused before redemption.
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	check := enrollment.Redemption{PublicKey: pub}
	check.Signature = ed25519.Sign(key, enrollment.CheckMessage(ticket, check))
	if err = proxyRequest(t, local+"/check", "", check, nil); err == nil || !strings.Contains(err.Error(), "selected gateway proxy") {
		t.Fatalf("legacy installer check: %v", err)
	}
	legacy := enrollment.Redemption{PublicKey: pub, CredentialHash: enrollment.Hash("credential")}
	legacy.Signature = ed25519.Sign(key, enrollment.Message(ticket, legacy))
	if err = proxyRequest(t, local+"/redeem", "", legacy, nil); err == nil || !strings.Contains(err.Error(), "selected gateway proxy") {
		t.Fatalf("legacy installer redemption: %v", err)
	}
	check.GatewayProxy = office
	check.Signature = ed25519.Sign(key, enrollment.CheckMessage(ticket, check))
	if err = proxyRequest(t, local+"/check", "", check, nil); err != nil {
		t.Fatal(err)
	}
	// Stale observations are never eligible, regardless of the worker clock.
	sendProxyHealth(t, ws, []model.ProxyReadiness{{URL: office, State: model.ProxyReady, CheckedAt: time.Now().UTC(), AgeSeconds: int(model.ProxyReadinessTTL/time.Second) + 1}})
	deadline = time.Now().Add(3 * time.Second)
	for proxyStatus(t, a, worker.ID)[0].State != model.ProxyStale {
		if time.Now().After(deadline) {
			t.Fatal("old readiness was not stale")
		}
		time.Sleep(20 * time.Millisecond)
	}
	second := request
	second.Name = "render-02"
	expect(second, "stale")
	_ = ws.CloseNow()
	deadline = time.Now().Add(3 * time.Second)
	for proxyStatus(t, a, worker.ID)[0].State != model.ProxyOffline {
		if time.Now().After(deadline) {
			t.Fatal("disconnected worker was not offline")
		}
		time.Sleep(20 * time.Millisecond)
	}
	expect(second, "offline")
	// The selection snapshot survives a gateway restart.
	server.Close()
	if err = g.Close(); err != nil {
		t.Fatal(err)
	}
	g, err = New(filepath.Dir(g.path), commonKey, Options{SuperuserKey: superKey})
	if err != nil {
		t.Fatal(err)
	}
	restarted := httptest.NewServer(g.Handler())
	t.Cleanup(func() { restarted.Close(); _ = g.Close() })
	var invites []enrollment.Invitation
	if err = (testAdmin{URL: restarted.URL, Key: a.Key}).JSON(ctx, "GET", "/v1/fleet/installations", nil, &invites); err != nil {
		t.Fatal(err)
	}
	if len(invites) != 1 || invites[0].GatewayProxy == nil || invites[0].GatewayProxy.URL != office || invites[0].GatewayProxy.Worker != worker.ID {
		t.Fatalf("proxy selection did not persist: %+v", invites)
	}
}
