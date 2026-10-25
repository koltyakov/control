package gateway

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/koltyakov/control/internal/identity"
	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/protocol"
	"google.golang.org/protobuf/proto"
)

func TestConsoleStartupRelayAndFallback(t *testing.T) {
	g, server := installationFixture(t)
	id, err := identity.Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ws := rawPeer(t, server.URL, commonKey, "startup-worker", legacyUser, id)
	if ws == nil {
		t.Fatal("registration failed")
	}
	admin := testAdmin{URL: server.URL, Key: superKey}
	provisionBrowser(t, admin, "")
	browser := browserLogin(t, server.URL, "@operator", browserPassword)
	route := "/console/api/fleet/nodes/" + id.ID + "/startup"
	g.mu.Lock()
	n := g.nodes[id.ID]
	n.RunAs = "user"
	g.nodes[id.ID] = n
	g.mu.Unlock()
	data, _ := browser.request(t, "POST", route, map[string]string{"mode": "system"}, 200)
	var reply model.StartupReply
	if json.Unmarshal(data, &reply) != nil || reply.Status != "manual" {
		t.Fatal("old nodes need a local fallback", string(data))
	}
	for _, body := range []map[string]string{{"mode": "other"}, {"mode": "system", "command": "whoami"}, {"mode": "system", "config": "/other-profile"}} {
		browser.request(t, "POST", route, body, 400)
	}
	g.mu.Lock()
	n.GatewayStartup = true
	g.nodes[id.ID] = n
	g.mu.Unlock()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			_, data, err := ws.Read(ctx)
			if err != nil {
				return
			}
			var packet protocol.Packet
			if proto.Unmarshal(data, &packet) != nil || packet.Kind != "startup.request" {
				continue
			}
			var request model.StartupRequest
			if json.Unmarshal(packet.Data, &request) != nil || request.Mode != "system" {
				return
			}
			result, _ := proto.Marshal(&protocol.Packet{Kind: "startup.result", Data: model.JSON(model.StartupReply{ID: request.ID, Status: "accepted"})})
			_ = ws.Write(ctx, websocket.MessageBinary, result)
			return
		}
	}()
	data, _ = browser.request(t, "POST", route, map[string]string{"mode": "system"}, 200)
	if json.Unmarshal(data, &reply) != nil || reply.Status != "accepted" {
		t.Fatal("admission not reported", string(data))
	}
	<-done
	_, other := createTestUser(t, server, "other-startup-owner")
	var ignored model.StartupReply
	if err := other.JSON(t.Context(), "POST", "/v1/fleet/nodes/"+id.ID+"/startup", map[string]string{"mode": "system"}, &ignored); err == nil {
		t.Fatal("another fleet requested a startup change")
	}
	g.mu.Lock()
	g.startupChanges["pending"] = pendingStartup{node: id.ID, reply: make(chan model.StartupReply, 1)}
	g.mu.Unlock()
	if g.receiveStartup(model.Node{ID: "forged"}, model.JSON(model.StartupReply{ID: "pending", Status: "accepted"})) {
		t.Fatal("accepted another node's startup reply")
	}
	browser.request(t, "POST", route, map[string]string{"mode": "system"}, 409)
}
