package transport

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hashicorp/yamux"
	"github.com/koltyakov/control/internal/gateway"
	"github.com/koltyakov/control/internal/identity"
	"github.com/koltyakov/control/internal/model"
	"github.com/pion/webrtc/v4"
)

func TestNetworkMonitorChangesAndSamplingFailures(t *testing.T) {
	p := New(Config{}, nil)
	p.ctx, p.cancel = context.WithCancel(context.Background())
	defer func() { _ = p.Close() }()
	ticks := make(chan time.Time)
	var calls atomic.Int32
	samples := []struct {
		value string
		err   error
	}{{"wifi", nil}, {"", errors.New("sample failed")}, {"wifi", nil}, {"vpn", nil}, {"vpn", nil}, {"wifi", nil}}
	done := make(chan struct{})
	go func() {
		defer close(done)
		p.monitorNetwork(func() (string, error) {
			s := samples[int(calls.Add(1))-1]
			return s.value, s.err
		}, ticks)
	}()
	for i, want := range []uint64{0, 0, 0, 1, 1, 2} {
		if i > 0 {
			ticks <- time.Now()
		}
		deadline := time.Now().Add(time.Second)
		for {
			p.mu.Lock()
			generation := p.networkGeneration
			p.mu.Unlock()
			if calls.Load() >= int32(i+1) && generation == want {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("sample %d: network generation %d, want %d", i, generation, want)
			}
			time.Sleep(time.Millisecond)
		}
	}
	p.cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("network monitor did not stop")
	}
}

func TestNetworkSwitchReconnectsAndDoesNotReplayStreams(t *testing.T) {
	for _, tc := range []struct{ relay, receiver bool }{{false, false}, {true, false}, {false, true}, {true, true}} {
		t.Run(fmt.Sprintf("relay=%v/receiver=%v", tc.relay, tc.receiver), func(t *testing.T) {
			relay := tc.relay
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			const token = "network-switch-test-token-123456"
			g, err := gateway.New(t.TempDir(), token)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = g.Close() }()
			server := httptest.NewServer(g.Handler())
			defer server.Close()
			var received atomic.Int32
			makePeer := func(name string) *Peer {
				id, err := identity.Generate()
				if err != nil {
					t.Fatal(err)
				}
				p := New(Config{Gateway: server.URL, Token: token, Identity: id, Node: model.Node{ID: id.ID, Name: name, PublicKey: id.Public}, RelayOnly: relay}, func(_ string, conn net.Conn) {
					buffer := make([]byte, 4)
					for {
						if _, err := io.ReadFull(conn, buffer); err != nil {
							return
						}
						received.Add(1)
						if _, err := conn.Write(buffer); err != nil {
							return
						}
					}
				})
				p.networkSnapshot = func() (string, error) { return "unchanged", nil }
				if err := p.Start(ctx); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = p.Close() })
				return p
			}
			a, b := makePeer("caller"), makePeer("worker")
			var streams []net.Conn
			for _, lane := range []Lane{ControlLane, BulkLane, InteractiveLane} {
				stream, err := a.OpenLane(ctx, b.IdentityID(), lane)
				if err != nil {
					t.Fatal(err)
				}
				streams = append(streams, stream)
				want := "webrtc"
				if relay {
					want = "relay"
				}
				if stream.(*sessionStream).mode != want {
					t.Fatalf("%s lane used %s, want %s", lane, stream.(*sessionStream).mode, want)
				}
				_ = stream.SetDeadline(time.Now().Add(5 * time.Second))
				if _, err := stream.Write([]byte("once")); err != nil {
					t.Fatal(err)
				}
				var echo [4]byte
				if _, err := io.ReadFull(stream, echo[:]); err != nil || string(echo[:]) != "once" {
					t.Fatal(echo, err)
				}
			}
			switched := a
			if tc.receiver {
				switched = b
			}
			switched.resetNetwork()
			if len(switched.Connections()) != 0 || len(switched.SessionStats()) != 0 {
				t.Fatal("network switch retained stale connection counts")
			}
			for _, stream := range streams {
				if _, err := stream.Write([]byte("lost")); err == nil && !tc.receiver {
					t.Fatal("old stream survived network switch")
				}
				var buffer [1]byte
				if _, err := stream.Read(buffer[:]); err == nil {
					t.Fatal("old stream remained readable after network switch")
				}
				_ = stream.Close()
			}
			for switched.Stats().GatewayReconnects == 0 {
				select {
				case <-ctx.Done():
					t.Fatal("gateway did not reconnect", ctx.Err())
				case <-time.After(10 * time.Millisecond):
				}
			}
			if received.Load() != 3 {
				t.Fatal("network recovery replayed old stream data", received.Load())
			}
			stream, err := a.Open(ctx, b.IdentityID())
			if err != nil {
				t.Fatal("new call could not establish a fresh session", err)
			}
			defer func() { _ = stream.Close() }()
			_ = stream.SetDeadline(time.Now().Add(5 * time.Second))
			if _, err := stream.Write([]byte("next")); err != nil {
				t.Fatal(err)
			}
			var echo [4]byte
			if _, err := io.ReadFull(stream, echo[:]); err != nil || string(echo[:]) != "next" || received.Load() != 4 {
				t.Fatal("fresh stream failed or replayed data", echo, err, received.Load())
			}
		})
	}
}

func TestDisconnectedCarrierIsHiddenAndRecoveryCancelsRetirement(t *testing.T) {
	p := New(Config{}, nil)
	p.ctx, p.cancel = context.WithCancel(context.Background())
	defer func() { _ = p.Close() }()
	left, right := net.Pipe()
	mux, err := yamux.Client(left, muxConfig(ControlLane))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = right.Close() }()
	c := &rtcCarrier{remote: "remote", links: map[string]*link{}, changed: make(chan struct{})}
	l := &link{id: "session", remote: c.remote, carrier: c, conn: newConn(p.ctx, nil, nil)}
	c.links[l.id] = l
	p.carriers[c.remote] = c
	p.links[l.id] = l
	p.allSessions[l.id] = &peerSession{id: l.id, remote: c.remote, mode: "webrtc", link: l, mux: mux}
	if len(p.Connections()) != 1 || len(p.SessionStats()) != 1 {
		t.Fatal("healthy session not reported")
	}
	p.carrierStateChanged(c, webrtc.PeerConnectionStateDisconnected)
	if len(p.Connections()) != 0 || len(p.SessionStats()) != 0 {
		t.Fatal("disconnected carrier still reported as reachable")
	}
	if got := p.ConnectionRoutes(); got != (model.ConnectionRouteCounts{}) {
		t.Fatalf("disconnected carrier counted in routes: %+v", got)
	}
	p.mu.Lock()
	stale := c.changed
	if !c.disconnected {
		t.Fatal("disconnected carrier remained eligible for reuse")
	}
	p.mu.Unlock()
	p.carrierStateChanged(c, webrtc.PeerConnectionStateConnected)
	p.closeCarrierIfDisconnected(c, stale)
	p.mu.Lock()
	defer p.mu.Unlock()
	if c.closed || c.disconnected || p.carriers[c.remote] != c {
		t.Fatal("old disconnect timeout retired a recovered carrier")
	}
}

func TestNetworkResetCancelsSetupsAndRejectsOldSessions(t *testing.T) {
	p := New(Config{}, nil)
	p.ctx, p.cancel = context.WithCancel(context.Background())
	defer func() { _ = p.Close() }()
	outgoing, cancelOutgoing := context.WithCancel(p.ctx)
	defer cancelOutgoing()
	incoming, cancelIncoming := context.WithCancel(p.ctx)
	defer cancelIncoming()
	gatewayConnect, cancelConnect := context.WithCancel(p.ctx)
	defer cancelConnect()
	p.connectCancel = cancelConnect
	p.pending[sessionKey{"remote", ControlLane}] = &sessionDial{cancel: cancelOutgoing}
	p.incomingSetups["incoming"] = &incomingSetup{cancel: cancelIncoming}
	p.resolved["worker"] = "remote"
	p.lookups["remote"] = lookupEntry{node: model.Node{ID: "remote", Online: true}}
	left, right := net.Pipe()
	defer func() { _ = right.Close() }()
	mux, err := yamux.Client(left, muxConfig(ControlLane))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = mux.Close() }()
	l := &link{conn: newConn(p.ctx, nil, nil), generation: p.networkGeneration}
	defer func() { _ = l.conn.Close() }()
	p.resetNetwork()
	if outgoing.Err() == nil || incoming.Err() == nil || gatewayConnect.Err() == nil || len(p.pending) != 0 || len(p.resolved) != 0 || len(p.lookups) != 0 {
		t.Fatal("old network setup or directory cache survived")
	}
	if p.registerSession(&peerSession{id: "old", mux: mux, link: l}) {
		t.Fatal("setup from the old network repopulated the session cache")
	}
}

func TestDisconnectedCarrierRetiresEveryLaneAfterGrace(t *testing.T) {
	p := New(Config{}, nil)
	p.ctx, p.cancel = context.WithCancel(context.Background())
	defer func() { _ = p.Close() }()
	c := &rtcCarrier{remote: "remote", links: map[string]*link{}, changed: make(chan struct{})}
	p.carriers[c.remote] = c
	for _, lane := range []Lane{ControlLane, BulkLane, InteractiveLane} {
		l := &link{id: string(lane), carrier: c, conn: newConn(p.ctx, nil, nil)}
		c.links[l.id], p.links[l.id] = l, l
	}
	p.carrierStateChanged(c, webrtc.PeerConnectionStateDisconnected)
	ctx, cancel := context.WithTimeout(p.ctx, carrierDisconnectGrace+2*time.Second)
	defer cancel()
	for _, l := range c.links {
		select {
		case <-l.conn.ctx.Done():
		case <-ctx.Done():
			t.Fatalf("%s lane survived disconnect grace", l.id)
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if !c.closed || p.carriers[c.remote] != nil {
		t.Fatal("disconnected carrier remained cached")
	}
}
