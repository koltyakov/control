package gateway

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/koltyakov/control/internal/enrollment"
	"github.com/koltyakov/control/internal/identity"
	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/protocol"
	"github.com/koltyakov/control/internal/update"
	"google.golang.org/protobuf/proto"
)

func gatewayAt(t *testing.T, dir string) (*Gateway, *httptest.Server) {
	t.Helper()
	g, err := New(dir, commonKey, Options{SuperuserKey: superKey})
	if err != nil {
		t.Fatal(err)
	}
	data := []byte("installer fixture")
	a := update.Asset{OS: "linux", Arch: "amd64", File: "control_linux_amd64", Size: int64(len(data)), SHA256: enrollment.Hash(string(data))}
	if err = g.updates.Upload(bytes.NewReader(data), a); err != nil {
		t.Fatal(err)
	}
	if _, err = g.updates.Publish(update.Manifest{Version: "test", Assets: []update.Asset{a}}, "test"); err != nil {
		t.Fatal(err)
	}
	return g, httptest.NewServer(g.Handler())
}

func durableState(t *testing.T, g *Gateway) string {
	t.Helper()
	g.mu.Lock()
	defer g.mu.Unlock()
	return string(model.JSON(map[string]any{
		"users": g.users, "keys": g.keys, "installations": g.installations, "nodes": g.nodes,
		"machineStates": g.machineStates, "owners": g.owners, "clientOwners": g.clientOwners, "clientTransports": g.clientTransports,
	}))
}

// Every request path commits only the records it changes. A restart must load
// exactly the state the running gateway held, including deletions.
func TestIncrementalCommitsSurviveRestart(t *testing.T) {
	dir := t.TempDir()
	g, s := gatewayAt(t, dir)
	ctx := context.Background()
	alice, a := createTestUser(t, s, "alice")
	_, b := createTestUser(t, s, "bob")
	var key struct {
		Key   APIKey
		Token string
	}
	if err := a.JSON(ctx, "POST", "/v1/fleet/keys", map[string]string{"name": "spare"}, &key); err != nil {
		t.Fatal(err)
	}
	if err := a.JSON(ctx, "DELETE", "/v1/fleet/keys/"+key.Key.ID, nil, nil); err != nil {
		t.Fatal(err)
	}
	if (testAdmin{URL: s.URL, Key: key.Token}).JSON(ctx, "GET", "/v1/auth", nil, nil) == nil {
		t.Fatal("revoked key still authenticates")
	}
	link := invite(t, s, "redeemed")
	q, credential := redeemRequest(t, link)
	body, _ := json.Marshal(q)
	resp, err := http.Post(link.URL+"/redeem", "application/json", bytes.NewReader(body))
	if err != nil || resp.StatusCode != http.StatusNoContent {
		t.Fatal("redeem", err)
	}
	_ = resp.Body.Close()
	if err := (testAdmin{URL: s.URL, Key: credential}).JSON(ctx, "GET", "/v1/auth", nil, nil); err != nil {
		t.Fatal("redeemed credential not indexed", err)
	}
	_ = invite(t, s, "pending")
	worker, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	gone, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	if rawPeer(t, s.URL, a.Key, "worker", alice.ID, worker) == nil {
		t.Fatal("machine registration failed")
	}
	removed := rawPeer(t, s.URL, a.Key, "gone", alice.ID, gone)
	if removed == nil {
		t.Fatal("machine registration failed")
	}
	owner, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	transport, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	if rawOwnedClientSession(t, s.URL, a.Key, transport, model.Node{Name: "cli-" + owner.ID[:16]}, true, owner, nil) == nil {
		t.Fatal("client session failed")
	}
	name := "renamed"
	if err := a.JSON(ctx, "PATCH", "/v1/fleet/nodes/"+worker.ID, map[string]any{"name": name}, nil); err != nil {
		t.Fatal(err)
	}
	_ = removed.CloseNow()
	if err := a.JSON(ctx, "DELETE", "/v1/fleet/nodes/"+gone.ID+"?stop=true", nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := (testAdmin{URL: s.URL, Key: superKey}).JSON(ctx, "DELETE", "/v1/admin/users/"+userID(t, s, "bob"), nil, nil); err != nil {
		t.Fatal(err)
	}
	if b.JSON(ctx, "GET", "/v1/auth", nil, nil) == nil {
		t.Fatal("disabled user's key still authenticates")
	}
	s.Close()
	if err := g.Close(); err != nil {
		t.Fatal(err)
	}
	before := durableState(t, g)
	if strings.Contains(before, `"online":true`) {
		t.Fatal("closed gateway retained online machines")
	}
	reopened, server := gatewayAt(t, dir)
	defer func() { server.Close(); _ = reopened.Close() }()
	if after := durableState(t, reopened); after != before {
		t.Fatalf("restart changed durable state\nbefore: %s\nafter:  %s", before, after)
	}
	if err := (testAdmin{URL: server.URL, Key: credential}).JSON(ctx, "GET", "/v1/auth", nil, nil); err != nil {
		t.Fatal("credential index not rebuilt on startup", err)
	}
}

func userID(t *testing.T, s *httptest.Server, name string) string {
	t.Helper()
	var users []User
	if err := (testAdmin{URL: s.URL, Key: superKey}).JSON(context.Background(), "GET", "/v1/admin/users", nil, &users); err != nil {
		t.Fatal(err)
	}
	for _, u := range users {
		if u.Name == name {
			return u.ID
		}
	}
	t.Fatal("unknown user", name)
	return ""
}

// A newer node signs its exact registration bytes, so fields this gateway does
// not know are ignored rather than breaking verification.
func TestSignedRegistrationBytesTolerateUnknownFields(t *testing.T) {
	g, s := installationFixture(t)
	alice, a := createTestUser(t, s, "alice")
	id, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	register := func(mutate func(*Hello, []byte)) string {
		ws, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(s.URL, "http")+"/v1/connect", &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer " + a.Key}}})
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = ws.CloseNow() }()
		_, challenge, err := ws.Read(ctx)
		if err != nil {
			t.Fatal(err)
		}
		node := model.JSON(model.Node{ID: id.ID, PublicKey: id.Public, Name: "future", UserID: alice.ID, LargePackets: true})
		node = append(node[:len(node)-1], []byte(`,"futureCapability":{"enabled":true}}`)...)
		hello := Hello{Version: model.Version, NodeBytes: node}
		mutate(&hello, challenge)
		if err := ws.Write(ctx, websocket.MessageBinary, model.JSON(hello)); err != nil {
			t.Fatal(err)
		}
		_, ready, err := ws.Read(ctx)
		if err != nil {
			return ""
		}
		g.mu.Lock()
		registered := g.nodes[id.ID]
		g.mu.Unlock()
		if !registered.LargePackets || strings.Contains(string(model.JSON(registered)), "futureCapability") {
			t.Fatal("registration fields not decoded leniently", registered)
		}
		return string(ready)
	}
	if got := register(func(h *Hello, challenge []byte) { h.Signature = ed25519.Sign(id.Private, h.Message(challenge)) }); got != "ready" {
		t.Fatal("signed registration bytes rejected")
	}
	waitSessionClosed(t, g, id.ID)
	// The signature covers the exact bytes; a modified registration is refused.
	if got := register(func(h *Hello, challenge []byte) {
		h.Signature = ed25519.Sign(id.Private, h.Message(challenge))
		h.NodeBytes = bytes.Replace(h.NodeBytes, []byte(`"future"`), []byte(`"forged"`), 1)
	}); got == "ready" {
		t.Fatal("modified registration bytes accepted")
	}
}

// A destination that cannot keep up loses only the overflowing relay session,
// not its gateway connection, signaling, or other sessions.
func TestRelayOverflowClosesOnlyTheSession(t *testing.T) {
	g, s := installationFixture(t)
	alice, a := createTestUser(t, s, "alice")
	sender, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	receiver, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	ws := rawPeer(t, s.URL, a.Key, "sender", alice.ID, sender)
	slow := rawPeer(t, s.URL, a.Key, "receiver", alice.ID, receiver)
	if ws == nil || slow == nil {
		t.Fatal("registration failed")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	closed := make(chan *protocol.Packet, 1)
	go func() {
		for {
			_, b, err := ws.Read(ctx)
			if err != nil {
				return
			}
			var p protocol.Packet
			if proto.Unmarshal(b, &p) == nil && p.Kind == "close" && p.Session == "flood" {
				closed <- &p
				return
			}
		}
	}()
	chunk := bytes.Repeat([]byte("x"), 64<<10)
	packet, err := proto.Marshal(&protocol.Packet{Kind: "data", To: receiver.ID, Session: "flood", Data: chunk})
	if err != nil {
		t.Fatal(err)
	}
	// The receiver never reads, so its socket buffers and then its queue fill.
	var signal *protocol.Packet
	for sent := 0; signal == nil && sent < 4096; sent++ {
		if err := ws.Write(ctx, websocket.MessageBinary, packet); err != nil {
			t.Fatal(err)
		}
		select {
		case signal = <-closed:
		default:
		}
	}
	if signal == nil {
		select {
		case signal = <-closed:
		case <-ctx.Done():
			t.Fatal("sender was not told to close the overflowing session")
		}
	}
	if signal.From != receiver.ID || string(signal.Data) != "relay queue full" {
		t.Fatal("unexpected close", signal)
	}
	if g.route(receiver.ID, alice.ID) == nil {
		t.Fatal("slow receiver's gateway connection was closed")
	}
	var metrics GatewayMetrics
	if err := (testAdmin{URL: s.URL, Key: a.Key}).JSON(ctx, "GET", "/v1/admin/metrics", nil, &metrics); err == nil {
		t.Fatal("fleet account read gateway-wide metrics")
	}
	if err := (testAdmin{URL: s.URL, Key: superKey}).JSON(ctx, "GET", "/v1/admin/metrics", nil, &metrics); err != nil {
		t.Fatal(err)
	}
	if metrics.RelayOverflows == 0 || metrics.ConnectedMachines != 2 || metrics.RelayPackets == 0 {
		t.Fatal("metrics did not record the overflow", metrics)
	}
}
