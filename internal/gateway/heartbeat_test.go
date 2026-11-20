package gateway

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"errors"
	"net/http"
	"net/http/httptest"
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

func TestWriterReleasesOversizedMarshalStorageAfterWrite(t *testing.T) {
	received := make(chan []byte, 4)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = ws.CloseNow() }()
		ws.SetReadLimit(1 << 20)
		for {
			_, data, err := ws.Read(r.Context())
			if err != nil {
				return
			}
			received <- data
		}
	}))
	defer server.Close()
	ws, _, err := websocket.Dial(t.Context(), "ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ws.CloseNow() }()
	c := &connection{ws: ws}
	write := func(size int) {
		t.Helper()
		packet := &protocol.Packet{Kind: "data", From: "a", Session: "s", Data: bytes.Repeat([]byte{byte(size)}, size)}
		if err := c.writePacket(t.Context(), packet); err != nil {
			t.Fatal(err)
		}
		var got protocol.Packet
		if err := proto.Unmarshal(<-received, &got); err != nil || !bytes.Equal(got.Data, packet.Data) {
			t.Fatal("written packet corrupted", err)
		}
	}
	write(64 << 10)
	ordinary := c.writeBuffer
	if cap(ordinary) == 0 || cap(ordinary) > maxRetainedWriteBytes {
		t.Fatal("ordinary marshal storage not retained", cap(ordinary))
	}
	write(16)
	if cap(c.writeBuffer) == 0 || &c.writeBuffer[:1][0] != &ordinary[:1][0] {
		t.Fatal("ordinary marshal storage not reused")
	}
	write(512 << 10)
	if cap(c.writeBuffer) > maxRetainedWriteBytes {
		t.Fatal("oversized marshal storage retained after write completed", cap(c.writeBuffer))
	}
	write(16)
	// A failed write must release oversized storage too. Closing the socket
	// makes the outcome deterministic rather than racing cancellation.
	if err := ws.CloseNow(); err != nil {
		t.Fatal(err)
	}
	if err := c.writePacket(t.Context(), &protocol.Packet{Kind: "data", Data: make([]byte, 512<<10)}); err == nil {
		t.Fatal("closed socket accepted a write")
	}
	if c.writeBuffer != nil {
		t.Fatal("failed write retained oversized marshal storage")
	}
}

func TestHeartbeatEchoConfirmsTelemetryOnItsConnection(t *testing.T) {
	g, server := installationFixture(t)
	user, admin := createTestUser(t, server, "telemetry-user")
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
	send := func(kind, session string, data []byte) {
		t.Helper()
		packet, _ := proto.Marshal(&protocol.Packet{Kind: kind, Session: session, Data: data})
		if err := ws.Write(ctx, websocket.MessageBinary, packet); err != nil {
			t.Fatal(err)
		}
	}
	send("node.health", "", model.JSON(model.NodeHealth{ActiveCount: 1}))
	send("update.status", "", model.JSON(update.Status{State: "idle"}))
	g.mu.Lock()
	c := g.peers[id.ID]
	g.mu.Unlock()
	waitFor := func(what string, ok func() bool) {
		t.Helper()
		for !ok() {
			select {
			case <-ctx.Done():
				t.Fatal(what)
			case <-time.After(5 * time.Millisecond):
			}
		}
	}
	waitFor("reports not received", func() bool {
		g.mu.Lock()
		defer g.mu.Unlock()
		return c.health != nil && !c.statusAt.IsZero()
	})
	// Age both reports without making them stale, then answer a challenge.
	age := func(reported time.Duration) (time.Time, time.Time) {
		g.mu.Lock()
		defer g.mu.Unlock()
		old := time.Now().UTC().Add(-3 * time.Second)
		c.healthAt, c.healthReceived, c.statusAt = old, time.Now().UTC().Add(-reported), time.Now().UTC().Add(-reported)
		status := g.updateStatus[id.ID]
		status.SeenAt = old
		g.updateStatus[id.ID] = status
		return c.healthAt, status.SeenAt
	}
	ping := func() string {
		t.Helper()
		c.requestProbe()
		for {
			_, data, err := ws.Read(ctx)
			if err != nil {
				t.Fatal(err)
			}
			var packet protocol.Packet
			if proto.Unmarshal(data, &packet) == nil && packet.Kind == "gateway.ping" {
				return packet.Session
			}
		}
	}
	read := func() (time.Time, time.Time) {
		g.mu.Lock()
		defer g.mu.Unlock()
		return c.healthAt, g.updateStatus[id.ID].SeenAt
	}

	healthBefore, statusBefore := age(time.Second)
	nonce := ping()
	send("gateway.pong", strings.Repeat("0", 32), nil)
	time.Sleep(50 * time.Millisecond)
	if health, status := read(); !health.Equal(healthBefore) || !status.Equal(statusBefore) {
		t.Fatal("a wrong challenge echo confirmed telemetry")
	}
	send("gateway.pong", nonce, nil)
	waitFor("matched echo did not confirm reports", func() bool {
		health, status := read()
		return health.After(healthBefore) && status.After(statusBefore)
	})

	// Reports older than the confirmation limit must be sent again.
	healthBefore, statusBefore = age(telemetryReportLimit + time.Second)
	send("gateway.pong", ping(), nil)
	time.Sleep(50 * time.Millisecond)
	if health, status := read(); !health.Equal(healthBefore) || !status.Equal(statusBefore) {
		t.Fatal("heartbeat revived reports beyond the confirmation limit")
	}

	// A report received on a previous connection is never confirmed.
	g.mu.Lock()
	other := &connection{statusAt: time.Now(), health: c.health, healthReceived: time.Now()}
	g.mu.Unlock()
	healthBefore, statusBefore = age(time.Second)
	g.refreshTelemetry(id.ID, other)
	if health, status := read(); !health.Equal(healthBefore) || !status.Equal(statusBefore) {
		t.Fatal("superseded connection confirmed telemetry")
	}
}
