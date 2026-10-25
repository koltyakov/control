package gateway

import (
	"context"
	"crypto/ed25519"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/koltyakov/control/internal/identity"
	"github.com/koltyakov/control/internal/model"
)

func TestWakeMetadataPersistenceAndIsolation(t *testing.T) {
	dir := t.TempDir()
	g, s := gatewayAt(t, dir)
	t.Cleanup(func() { s.Close(); _ = g.Close() })
	alice, a := createTestUser(t, s, "alice")
	_, b := createTestUser(t, s, "bob")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	id, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	interfaces := []model.WakeInterface{{Address: "192.168.1.2/24", MAC: "02:00:00:00:00:02"}}
	n := model.Node{ID: id.ID, Name: "sleeper", PublicKey: id.Public, WakeInterfaces: interfaces, LANDiscovery: true, LANPeers: []model.LANPeer{{ID: "forged", Local: interfaces[0], VerifiedAt: time.Now()}}}
	register := func(n model.Node) bool {
		ws, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(s.URL, "http")+"/v1/connect", &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer " + a.Key}}})
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = ws.CloseNow() }()
		_, challenge, err := ws.Read(ctx)
		if err != nil {
			t.Fatal(err)
		}
		h := Hello{Version: model.Version, NodeBytes: model.JSON(n)}
		h.Signature = ed25519.Sign(id.Private, h.Message(challenge))
		if err := ws.Write(ctx, websocket.MessageBinary, model.JSON(h)); err != nil {
			t.Fatal(err)
		}
		_, ready, err := ws.Read(ctx)
		return err == nil && string(ready) == "ready"
	}
	if !register(n) {
		t.Fatal("valid wake metadata rejected")
	}
	g.mu.Lock()
	forged := len(g.nodes[id.ID].LANPeers) != 0
	g.mu.Unlock()
	if forged {
		t.Fatal("registration supplied LAN relationship was trusted")
	}
	waitSessionClosed(t, g, id.ID)
	var nodes []model.Node
	if err := a.JSON(ctx, "GET", "/v1/nodes", nil, &nodes); err != nil || len(nodes) != 1 || nodes[0].Online || nodes[0].UserID != alice.ID || !reflect.DeepEqual(nodes[0].WakeInterfaces, interfaces) {
		t.Fatal("offline wake metadata lost", nodes, err)
	}
	if err := b.JSON(ctx, "GET", "/v1/nodes", nil, &nodes); err != nil || len(nodes) != 0 {
		t.Fatal("wake metadata crossed fleets", nodes, err)
	}
	bad := n
	bad.WakeInterfaces = []model.WakeInterface{{Address: "8.8.8.8/24", MAC: interfaces[0].MAC}}
	if register(bad) {
		t.Fatal("invalid wake interface accepted")
	}
	clientID, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	if rawClientSession(t, s.URL, a.Key, clientID, model.Node{Name: "cli-" + clientID.ID[:16], WakeInterfaces: interfaces}, true) != nil {
		t.Fatal("client advertised wake metadata")
	}
	s.Close()
	if err := g.Close(); err != nil {
		t.Fatal(err)
	}
	g, s = gatewayAt(t, dir)
	a.URL = s.URL
	if err := a.JSON(ctx, "GET", "/v1/nodes", nil, &nodes); err != nil || len(nodes) != 1 || !reflect.DeepEqual(nodes[0].WakeInterfaces, interfaces) {
		t.Fatal("wake metadata did not survive restart", nodes, err)
	}
}
