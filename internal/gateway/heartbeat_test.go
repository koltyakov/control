package gateway

import (
	"context"
	"crypto/ed25519"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/koltyakov/control/internal/identity"
	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/protocol"
	"github.com/koltyakov/control/internal/update"
	"google.golang.org/protobuf/proto"
)

func TestHeartbeatNegotiationIsSignedAndLegacyProofIsUnchanged(t *testing.T) {
	challenge := []byte("challenge")
	h := Hello{Node: model.Node{Name: "worker"}}
	legacy := append(append([]byte(nil), challenge...), model.JSON(h.Node)...)
	if string(h.Message(challenge)) != string(legacy) {
		t.Fatal("legacy handshake proof changed")
	}
	id, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	h.GatewayHeartbeat = true
	proof := ed25519.Sign(id.Private, h.Message(challenge))
	h.GatewayHeartbeat = false
	if ed25519.Verify(id.Public, h.Message(challenge), proof) {
		t.Fatal("heartbeat negotiation can be removed without invalidating the proof")
	}
}

func TestApplicationHeartbeatRetiresEndpointDespiteWebSocketPongs(t *testing.T) {
	g, server := installationFixture(t)
	user, admin := createTestUser(t, server, "heartbeat-user")
	id, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http")+"/v1/connect", &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer " + admin.Key}}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ws.CloseNow() }()
	_, challenge, err := ws.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	h := Hello{Version: model.Version, Node: model.Node{ID: id.ID, Name: "worker", PublicKey: id.Public, UserID: user.ID}, GatewayHeartbeat: true}
	h.Signature = ed25519.Sign(id.Private, h.Message(challenge))
	if err := ws.Write(ctx, websocket.MessageBinary, model.JSON(h)); err != nil {
		t.Fatal(err)
	}
	if _, ready, err := ws.Read(ctx); err != nil || string(ready) != "ready" {
		t.Fatal("registration failed", string(ready), err)
	}
	read := make(chan error, 1)
	challenged := make(chan struct{}, 1)
	relayed := make(chan struct{}, 1)
	go func() {
		for {
			_, data, err := ws.Read(ctx) // Automatically answers WebSocket ping frames.
			if err != nil {
				read <- err
				return
			}
			var packet protocol.Packet
			if err := proto.Unmarshal(data, &packet); err != nil {
				read <- err
				return
			}
			if packet.Kind == "data" && string(packet.Data) == "relay still flows" {
				relayed <- struct{}{}
				continue
			}
			if packet.Kind != "gateway.ping" || packet.From != update.GatewaySender {
				read <- errors.New("unexpected heartbeat packet")
				return
			}
			// A wrong application nonce must not refresh liveness.
			wrong, _ := proto.Marshal(&protocol.Packet{Kind: "gateway.pong", Session: strings.Repeat("0", 32)})
			if err := ws.Write(ctx, websocket.MessageBinary, wrong); err != nil {
				read <- err
				return
			}
			challenged <- struct{}{}
		}
	}()
	g.mu.Lock()
	c := g.peers[id.ID]
	g.mu.Unlock()
	ping, done := context.WithTimeout(ctx, time.Second)
	err = c.ws.Ping(ping)
	done()
	if err != nil {
		t.Fatal("transport-level ping should succeed", err)
	}
	select {
	case <-challenged:
	case err := <-read:
		t.Fatal("endpoint closed before application challenge", err)
	case <-ctx.Done():
		t.Fatal("gateway did not send application challenge")
	}
	if !c.enqueue(&protocol.Packet{Kind: "data", From: "peer", Session: strings.Repeat("1", 32), Data: []byte("relay still flows")}) {
		t.Fatal("relay packet was not queued")
	}
	select {
	case <-relayed:
	case <-time.After(time.Second):
		t.Fatal("waiting for an application pong blocked relay traffic")
	}
	select {
	case <-read:
	case <-ctx.Done():
		t.Fatal("unresponsive application retained its gateway connection")
	}
	waitSessionClosed(t, g, id.ID)
	if replacement := rawPeer(t, server.URL, admin.Key, "worker", user.ID, id); replacement == nil {
		t.Fatal("stale identity blocked reconnection after heartbeat expiry")
	}
}
