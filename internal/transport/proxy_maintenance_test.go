package transport

import (
	"context"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/koltyakov/control/internal/gateway"
	"github.com/koltyakov/control/internal/gatewayroute"
	"github.com/koltyakov/control/internal/identity"
	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/protocol"
	"github.com/koltyakov/control/internal/testutil"
	"github.com/koltyakov/control/internal/update"
)

func TestProxyDrainIsScopedAuthenticatedBoundedAndResumable(t *testing.T) {
	self, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	p := New(Config{Identity: self}, nil)
	target, other := strings.Repeat("a", 64), strings.Repeat("b", 64)
	token := identity.NewID()
	request := gateway.RelayDrainRequest{Peer: target, Until: time.Now().Add(time.Minute / 2)}
	packet := &protocol.Packet{Kind: "gateway.drain", From: "forged", Session: token, Data: model.JSON(request)}
	p.receiveGatewayDrain(packet)
	if p.relayDrainingLocked(target) {
		t.Fatal("forged drain changed admission")
	}
	packet.From = update.GatewaySender
	p.receiveGatewayDrain(packet)
	if !p.relayDrainingLocked(target) || p.relayDrainingLocked(other) {
		t.Fatal("scoped drain affected unrelated peers")
	}
	packet.Kind, packet.Session = "gateway.resume", identity.NewID()
	p.receiveGatewayDrain(packet)
	if !p.relayDrainingLocked(target) {
		t.Fatal("wrong token released maintenance")
	}
	packet.Session = token
	p.receiveGatewayDrain(packet)
	if p.relayDrainingLocked(target) {
		t.Fatal("matching resume retained drain")
	}
	request.Peer = self.ID
	packet.Kind, packet.Data = "gateway.drain", model.JSON(request)
	p.receiveGatewayDrain(packet)
	if !p.relayDrainingLocked(other) {
		t.Fatal("child did not drain all of its outgoing relays")
	}
	p.relayPeerDrains[self.ID] = peerDrain{token, time.Now().Add(-time.Second)}
	if p.relayDrainingLocked(other) {
		t.Fatal("lost resume held admission forever")
	}
	for _, bad := range []gateway.RelayDrainRequest{
		{Peer: "not-an-identity", Until: time.Now().Add(time.Second)},
		{Peer: target, Until: time.Now().Add(2 * time.Minute)},
		{Peer: target, Until: time.Now().Add(-time.Second)},
	} {
		packet.Data = model.JSON(bad)
		p.receiveGatewayDrain(packet)
		if p.relayDrainingLocked(target) {
			t.Fatal("invalid drain changed admission", bad)
		}
	}
}

func TestProxyGatewayParkReconnectsWithoutInterruptingPeerStreams(t *testing.T) {
	for _, relay := range []bool{false, true} {
		name := "webrtc"
		if relay {
			name = "relay"
		}
		t.Run(name, func(t *testing.T) { testProxyGatewayPark(t, relay) })
	}
}

func testProxyGatewayPark(t *testing.T, relay bool) {
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	const token = "proxy-park-test-token-123456789"
	g, err := gateway.New(t.TempDir(), token)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = g.Close() }()
	server, pool := testutil.TLSServer(t, g.Handler())
	t.Cleanup(gatewayroute.SetRootCAsForTesting(pool))
	address := server.Listener.Addr().String()
	proxy := testutil.NewConnectProxy(t, map[string]string{address: address})
	route, err := gatewayroute.New(proxy.URL)
	if err != nil {
		t.Fatal(err)
	}
	makePeer := func(name string, routed bool) *Peer {
		id, err := identity.Generate()
		if err != nil {
			t.Fatal(err)
		}
		cfg := Config{Gateway: server.URL, Token: token, Identity: id, RelayOnly: relay, Node: model.Node{ID: id.ID, Name: name, PublicKey: id.Public}}
		if routed {
			cfg.Route = route
		}
		p := New(cfg, func(_ string, c net.Conn) { _, _ = io.Copy(c, c) })
		p.api, p.trickleAPI = loopbackWebRTCAPI(p.cfg.DirectTimeout, false), loopbackWebRTCAPI(p.cfg.DirectTimeout, true)
		p.networkSnapshot = func() (string, error) { return "unchanged", nil }
		if err := p.Start(ctx); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = p.Close() })
		return p
	}
	child, caller := makePeer("child", true), makePeer("caller", false)
	stream, err := caller.Open(ctx, child.IdentityID())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stream.Close() }()
	wantMode := "webrtc"
	if relay {
		wantMode = "relay"
	}
	if mode := stream.(*sessionStream).mode; mode != wantMode {
		t.Fatal("fixture used wrong transport", mode)
	}
	drain := &protocol.Packet{Kind: "gateway.drain", From: update.GatewaySender, Session: identity.NewID(),
		Data: model.JSON(gateway.RelayDrainRequest{Peer: child.IdentityID(), Until: time.Now().Add(time.Minute / 2)})}
	if relay {
		if child.ParkGateway(time.Now().Add(time.Second)) {
			t.Fatal("park severed a live relay")
		}
		child.receive(drain)
		caller.receive(drain)
	}
	// Parking is only a gateway outage. Direct, already-admitted streams
	// continue; the caller's node gate decides whether work may be reserved.
	if !relay {
		if !child.ParkGateway(time.Now().Add(300 * time.Millisecond)) {
			t.Fatal("idle gateway could not park")
		}
		if child.gatewayWS.Load() != nil {
			t.Fatal("park retained gateway connection")
		}
	}
	if _, err := stream.Write([]byte("once")); err != nil {
		t.Fatal("park interrupted accepted peer bytes", err)
	}
	var data [4]byte
	if _, err := io.ReadFull(stream, data[:]); err != nil || string(data[:]) != "once" {
		t.Fatal("park lost direct stream bytes", err)
	}
	if relay {
		_ = stream.Close()
		waitFor(t, ctx, func() bool {
			child.mu.Lock()
			defer child.mu.Unlock()
			for _, l := range child.links {
				if l.mode == "relay" {
					return false
				}
			}
			return true
		})
		waitFor(t, ctx, func() bool { return child.ParkGateway(time.Now().Add(300 * time.Millisecond)) })
	}
	waitFor(t, ctx, func() bool { return child.stats.gatewayReconnects.Load() > 0 && child.gatewayWS.Load() != nil })
	if relay {
		drain.Kind = "gateway.resume"
		caller.receive(drain)
		stream, err = caller.Open(ctx, child.IdentityID())
		if err != nil {
			t.Fatal("parked relay did not recover", err)
		}
		defer func() { _ = stream.Close() }()
	}
	if _, err := stream.Write([]byte("more")); err != nil {
		t.Fatal("reconnect replaced an accepted direct stream", err)
	}
	if _, err := io.ReadFull(stream, data[:]); err != nil || string(data[:]) != "more" {
		t.Fatal("reconnect replayed or lost bytes", err)
	}
}
