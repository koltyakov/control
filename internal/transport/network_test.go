package transport

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http/httptest"
	"slices"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/hashicorp/yamux"
	"github.com/koltyakov/control/internal/gateway"
	"github.com/koltyakov/control/internal/identity"
	"github.com/koltyakov/control/internal/model"
	"github.com/pion/webrtc/v4"
)

func testNetworkInterface(t *testing.T, index int, name string, addresses ...string) networkInterface {
	t.Helper()
	iface := networkInterface{Interface: net.Interface{Index: index, Name: name, Flags: net.FlagUp}}
	for _, address := range addresses {
		ip, subnet, err := net.ParseCIDR(address)
		if err != nil {
			t.Fatal(err)
		}
		iface.addresses = append(iface.addresses, &net.IPNet{IP: ip, Mask: subnet.Mask})
	}
	return iface
}

func testNetworkFingerprint(t *testing.T, source string, interfaces ...networkInterface) string {
	t.Helper()
	ip := net.ParseIP(source)
	target := net.ParseIP("2001:db8:ffff::1")
	if ip.To4() != nil {
		target = net.ParseIP("192.0.2.1")
	}
	fingerprint, err := networkFingerprint(interfaces, []networkRoute{
		{target: &net.UDPAddr{IP: target, Port: 443}, source: ip},
	})
	if err != nil {
		t.Fatal(err)
	}
	return fingerprint
}

func TestNetworkFingerprintIPv6PrivacyAddresses(t *testing.T) {
	// Reproduce macOS alternating the selected temporary source while IPv4,
	// the interface inventory, and the VM bridge remain unchanged.
	wifi := testNetworkInterface(t, 15, "en0", "192.168.0.217/24", "2001:db8:1::1234/64", "2001:db8:1::5678/64")
	bridge := testNetworkInterface(t, 26, "bridge101", "10.211.55.2/24", "fd00:1::1/64")
	baseline := testNetworkFingerprint(t, "2001:db8:1::1234", wifi, bridge)
	for _, tc := range []struct {
		name       string
		source     string
		interfaces []networkInterface
	}{
		{"alternate temporary source", "2001:db8:1::5678", []networkInterface{wifi, bridge}},
		{"replace temporary address", "2001:db8:1::abcd", []networkInterface{testNetworkInterface(t, 15, "en0", "192.168.0.217/24", "2001:db8:1::abcd/64"), bridge}},
		{"unrelated address", "2001:db8:1::1234", []networkInterface{testNetworkInterface(t, 15, "en0", "2001:db8:1::1234/64", "2001:db8:2::1/64"), bridge}},
		{"unrelated adapter removed", "2001:db8:1::1234", []networkInterface{wifi}},
		{"unrelated adapter added", "2001:db8:1::1234", []networkInterface{wifi, bridge, testNetworkInterface(t, 30, "vpn", "fd00:2::1/64")}},
		{"reordered interfaces and addresses", "2001:db8:1::1234", []networkInterface{bridge, testNetworkInterface(t, 15, "en0", "2001:db8:1::5678/64", "2001:db8:1::1234/64", "192.168.0.217/24")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := testNetworkFingerprint(t, tc.source, tc.interfaces...); got != baseline {
				t.Fatalf("unrelated address change invalidated route:\n%s\nwant:\n%s", got, baseline)
			}
		})
	}
}

func TestNetworkFingerprintRouteChanges(t *testing.T) {
	for _, tc := range []struct {
		name          string
		before, after networkInterface
		source, next  string
	}{
		{"IPv6 interface", testNetworkInterface(t, 15, "en0", "2001:db8:1::1/64"), testNetworkInterface(t, 16, "en1", "2001:db8:1::1/64"), "2001:db8:1::1", "2001:db8:1::1"},
		{"IPv6 subnet", testNetworkInterface(t, 15, "en0", "2001:db8:1::1/64"), testNetworkInterface(t, 15, "en0", "2001:db8:2::1/64"), "2001:db8:1::1", "2001:db8:2::1"},
		{"IPv6 host route", testNetworkInterface(t, 15, "vpn", "fd00::1/128"), testNetworkInterface(t, 15, "vpn", "fd00::2/128"), "fd00::1", "fd00::2"},
		{"IPv4 DHCP", testNetworkInterface(t, 15, "en0", "192.168.0.217/24"), testNetworkInterface(t, 15, "en0", "192.168.0.218/24"), "192.168.0.217", "192.168.0.218"},
		{"IPv4 netmask", testNetworkInterface(t, 15, "en0", "192.168.0.217/24"), testNetworkInterface(t, 15, "en0", "192.168.0.217/25"), "192.168.0.217", "192.168.0.217"},
		{"unreachable", testNetworkInterface(t, 15, "en0", "2001:db8:1::1/64"), networkInterface{}, "2001:db8:1::1", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := testNetworkFingerprint(t, tc.source, tc.before)
			after := testNetworkFingerprint(t, tc.next, tc.after)
			if before == after {
				t.Fatal("real egress change retained the old fingerprint")
			}
		})
	}
}

func TestNetworkFingerprintDualStackSourceSelection(t *testing.T) {
	interfaces := []networkInterface{
		testNetworkInterface(t, 15, "en0", "192.168.0.217/24", "2001:db8:1::1234/64", "2001:db8:1::5678/64"),
		testNetworkInterface(t, 26, "bridge101", "10.211.55.2/24", "fd00:1::1/64"),
	}
	routes := []networkRoute{
		{target: &net.UDPAddr{IP: net.ParseIP("192.0.2.1"), Port: 443}, source: net.ParseIP("192.168.0.217")},
		{target: &net.UDPAddr{IP: net.ParseIP("2001:db8:ffff::1"), Port: 443}, source: net.ParseIP("2001:db8:1::1234")},
	}
	before, err := networkFingerprint(interfaces, routes)
	if err != nil {
		t.Fatal(err)
	}
	routes[1].source = net.ParseIP("2001:db8:1::5678")
	after, err := networkFingerprint(interfaces, routes)
	if err != nil || before != after {
		t.Fatalf("temporary IPv6 selection reset a dual-stack network: %q != %q, %v", before, after, err)
	}
}

func TestNetworkFingerprintIncompleteAndScopedSamples(t *testing.T) {
	iface := testNetworkInterface(t, 15, "en0", "fe80::1/64")
	routes := []networkRoute{{target: &net.UDPAddr{IP: net.ParseIP("fe80::2"), Zone: "en0", Port: 443}, source: net.ParseIP("fe80::1")}}
	if _, err := networkFingerprint([]networkInterface{iface}, routes); err != nil {
		t.Fatal(err)
	}
	routes[0].target.Zone = "15"
	if _, err := networkFingerprint([]networkInterface{iface}, routes); err != nil {
		t.Fatal("numeric interface zone was rejected", err)
	}
	routes[0].target.Zone = "en0"
	for _, tc := range []struct {
		name  string
		iface networkInterface
	}{
		{"missing source", testNetworkInterface(t, 15, "en0", "fe80::3/64")},
		{"wrong zone", testNetworkInterface(t, 16, "en1", "fe80::1/64")},
		{"down interface", networkInterface{Interface: net.Interface{Index: 15, Name: "en0"}, addresses: iface.addresses}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := networkFingerprint([]networkInterface{tc.iface}, routes); err == nil {
				t.Fatal("incomplete sample was accepted as a network switch")
			}
		})
	}
	// Lookup ordering, duplicate interface addresses, and target ordering must
	// not turn an equivalent sample into a network switch.
	routes = append(routes, networkRoute{target: &net.UDPAddr{IP: net.ParseIP("192.0.2.1"), Port: 443}})
	before, err := networkFingerprint([]networkInterface{iface}, routes)
	if err != nil {
		t.Fatal(err)
	}
	slices.Reverse(routes)
	iface.addresses = append(iface.addresses, iface.addresses...)
	after, err := networkFingerprint([]networkInterface{iface}, routes)
	if err != nil || before != after {
		t.Fatalf("equivalent samples differ: %q != %q, %v", before, after, err)
	}
}

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
	t.Parallel()
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
				p.api, p.trickleAPI = loopbackWebRTCAPI(p.cfg.DirectTimeout, false), loopbackWebRTCAPI(p.cfg.DirectTimeout, true)
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
			// Exercise the real monitor with the reproduced IPv6 source changes
			// while established WebRTC/relay streams carry traffic in every lane.
			wifi := testNetworkInterface(t, 15, "en0", "2001:db8:1::1234/64", "2001:db8:1::5678/64")
			samples := []string{
				testNetworkFingerprint(t, "2001:db8:1::1234", wifi),
				testNetworkFingerprint(t, "2001:db8:1::5678", wifi),
				testNetworkFingerprint(t, "2001:db8:1::1234", wifi),
				testNetworkFingerprint(t, "2001:db8:1::1234", wifi, testNetworkInterface(t, 26, "bridge101", "10.211.55.2/24")),
				testNetworkFingerprint(t, "2001:db8:1::abcd", testNetworkInterface(t, 15, "en0", "2001:db8:1::abcd/64")),
			}
			ticks := make(chan time.Time)
			if !switched.worker(func() {
				index := 0
				switched.monitorNetwork(func() (string, error) {
					value := samples[min(index, len(samples)-1)]
					index++
					return value, nil
				}, ticks)
			}) {
				t.Fatal("monitor did not start")
			}
			// Each send waits for the preceding sample comparison to finish;
			// the last tick is a barrier after all address-selection changes.
			for range len(samples) {
				select {
				case ticks <- time.Now():
				case <-ctx.Done():
					t.Fatal("network monitor stalled", ctx.Err())
				}
			}
			switched.mu.Lock()
			generation := switched.networkGeneration
			switched.mu.Unlock()
			if generation != 0 || switched.Stats().GatewayReconnects != 0 {
				t.Fatal("IPv6 source selection reset a healthy network")
			}
			for _, stream := range streams {
				if _, err := stream.Write([]byte("keep")); err != nil {
					t.Fatal("IPv6 source selection closed an established stream", err)
				}
				var echo [4]byte
				if _, err := io.ReadFull(stream, echo[:]); err != nil || string(echo[:]) != "keep" {
					t.Fatal("established stream did not survive IPv6 source selection", echo, err)
				}
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
			if received.Load() != 6 {
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
			if _, err := io.ReadFull(stream, echo[:]); err != nil || string(echo[:]) != "next" || received.Load() != 7 {
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
	synctest.Test(t, func(t *testing.T) {
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
		// Let the retirement goroutine arm its real production timer. The bubble
		// advances virtual time only after all of its goroutines are blocked.
		synctest.Wait()
		time.Sleep(carrierDisconnectGrace - time.Nanosecond)
		synctest.Wait()
		for _, l := range c.links {
			if l.conn.ctx.Err() != nil {
				t.Fatalf("%s lane retired before disconnect grace", l.id)
			}
		}
		time.Sleep(time.Nanosecond)
		synctest.Wait()
		for _, l := range c.links {
			if l.conn.ctx.Err() == nil {
				t.Fatalf("%s lane survived disconnect grace", l.id)
			}
		}
		p.mu.Lock()
		defer p.mu.Unlock()
		if !c.closed || p.carriers[c.remote] != nil {
			t.Fatal("disconnected carrier remained cached")
		}
	})
}
