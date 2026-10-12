package transport

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/koltyakov/control/internal/gateway"
	"github.com/koltyakov/control/internal/identity"
	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/protocol"
	"github.com/pion/webrtc/v4"
)

// Keep timing fixtures independent of VPN adapters and unreachable host routes.
func loopbackWebRTCAPI(direct time.Duration, trickle bool) *webrtc.API {
	settings := webRTCSettings(direct, trickle)
	settings.SetIncludeLoopbackCandidate(true)
	settings.SetIPFilter(func(ip net.IP) bool { return ip.IsLoopback() })
	settings.SetNetworkTypes([]webrtc.NetworkType{webrtc.NetworkTypeUDP4})
	return webrtc.NewAPI(webrtc.WithSettingEngine(settings))
}

func TestTrickleICEConnectsBeforeSlowGatheringAndLegacyFallback(t *testing.T) {
	for _, name := range []string{"trickle", "older-gateway", "older-peer"} {
		legacy := name != "trickle"
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			// A local STUN endpoint that never answers, with no internet dependency.
			stun, err := net.ListenPacket("udp4", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = stun.Close() }()
			const token = "trickle-ice-test-token-1234567890"
			g, err := gateway.New(t.TempDir(), token, gateway.Options{STUNServers: []string{"stun:" + stun.LocalAddr().String()}})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = g.Close() }()
			handler := g.Handler()
			var authCalls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v1/auth" && (authCalls.Add(1) == 2 || name == "older-gateway") && legacy {
					record := httptest.NewRecorder()
					handler.ServeHTTP(record, r)
					var auth map[string]any
					if err := json.Unmarshal(record.Body.Bytes(), &auth); err != nil {
						t.Error(err)
						return
					}
					delete(auth, "trickleICE")
					_, _ = w.Write(model.JSON(auth))
					return
				}
				handler.ServeHTTP(w, r)
			}))
			defer server.Close()
			var peers []*Peer
			for _, name := range []string{"alpha", "beta"} {
				id, err := identity.Generate()
				if err != nil {
					t.Fatal(err)
				}
				p := New(Config{Gateway: server.URL, Token: token, Identity: id, Node: model.Node{ID: id.ID, Name: name, PublicKey: id.Public}}, func(_ string, conn net.Conn) { _, _ = io.Copy(conn, conn) })
				p.api, p.trickleAPI = loopbackWebRTCAPI(p.cfg.DirectTimeout, false), loopbackWebRTCAPI(p.cfg.DirectTimeout, true)
				p.networkSnapshot = func() (string, error) { return "unchanged", nil }
				if err := p.Start(ctx); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = p.Close() })
				peers = append(peers, p)
			}
			started := time.Now()
			conn, err := peers[0].Open(ctx, "beta")
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = conn.Close() }()
			elapsed := time.Since(started)
			if !legacy && elapsed >= maxSTUNGather {
				t.Fatalf("trickle waited for gathering: %s", elapsed)
			}
			if legacy && elapsed < maxSTUNGather {
				t.Fatal("legacy SDP did not wait for candidates", elapsed)
			}
			stream := conn.(*sessionStream)
			if stream.mode != "webrtc" || stream.session.route().Kind != "direct" {
				t.Fatal("host connectivity fell back to a relay", stream.mode, stream.session.route())
			}
			if (stream.session.link.ice == nil) != legacy {
				t.Fatal("wrong trickle negotiation")
			}
			if !legacy && stream.session.link.pc.ICEGatheringState() == webrtc.ICEGatheringStateComplete {
				t.Fatal("test did not exercise an ongoing gathering")
			}
			_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
			if _, err := conn.Write([]byte("ping")); err != nil {
				t.Fatal(err)
			}
			var echo [4]byte
			if _, err := io.ReadFull(conn, echo[:]); err != nil || string(echo[:]) != "ping" {
				t.Fatal(echo, err)
			}
		})
	}
}

func TestEarlyICECandidatesBoundToSenderAndSession(t *testing.T) {
	p := New(Config{}, nil)
	p.ctx, p.cancel = context.WithCancel(context.Background())
	defer func() { _ = p.Close() }()
	setup := &incomingSetup{remote: "peer", candidates: make(chan webrtc.ICECandidateInit, maxTrickleCandidates)}
	p.incomingSetups["session"] = setup
	packet := &protocol.Packet{Kind: "ice", From: "foreign", Session: "session", Data: model.JSON(webrtc.ICECandidateInit{Candidate: "candidate"})}
	p.receive(packet)
	if len(setup.candidates) != 0 {
		t.Fatal("foreign sender injected a candidate")
	}
	packet.From = "peer"
	for range maxTrickleCandidates + 10 {
		p.receive(packet)
	}
	if len(setup.candidates) != maxTrickleCandidates || setup.candidateCount != maxTrickleCandidates {
		t.Fatal("early candidate queue was not bounded")
	}
	p.cfg.Node.TrickleICE, p.trickle["peer"] = true, true
	l, err := p.newLink(p.ctx, "session", "peer", "webrtc")
	if err != nil {
		t.Fatal(err)
	}
	ice := l.ice
	if ice.remote != setup.candidates || ice.remoteCount != maxTrickleCandidates {
		t.Fatal("early candidates were lost while creating the link")
	}
	<-ice.remote
	p.receive(packet)
	if len(ice.remote) != maxTrickleCandidates-1 {
		t.Fatal("draining bypassed the total candidate bound")
	}
	packet.Session = "unknown"
	p.receive(packet)
	if len(ice.remote) != maxTrickleCandidates-1 {
		t.Fatal("unknown session injected a candidate")
	}
}

func TestGatewaySTUNDefaultsAndExplicitOptOut(t *testing.T) {
	const token = "stun-defaults-test-token-1234567890"
	g, err := gateway.New(t.TempDir(), token, gateway.Options{STUNServers: []string{"stun:127.0.0.1:3478"}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = g.Close() }()
	server := httptest.NewServer(g.Handler())
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	peers := echoPeers(t, ctx, server.URL, token, true, "defaults")
	p := peers[0]
	if len(p.iceServers) != 1 || p.iceServers[0].URLs[0] != "stun:127.0.0.1:3478" {
		t.Fatal("gateway STUN defaults not inherited")
	}
	_ = p.Close()
	// Reconnect with an explicit empty list, as a persisted profile does.
	cfg := p.cfg
	cfg.ICEServers = []webrtc.ICEServer{}
	other := New(cfg, nil)
	if err := other.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if len(other.iceServers) != 0 {
		t.Fatal("explicit host-only profile inherited STUN")
	}
	_ = other.Close()
}

func TestCandidateRouteDistinguishesTURN(t *testing.T) {
	for _, tc := range []struct {
		local, remote webrtc.ICECandidateType
		kind          string
	}{
		{webrtc.ICECandidateTypeHost, webrtc.ICECandidateTypeSrflx, "direct"},
		{webrtc.ICECandidateTypeRelay, webrtc.ICECandidateTypeHost, "turn"},
		{webrtc.ICECandidateTypeSrflx, webrtc.ICECandidateTypeRelay, "turn"},
	} {
		pair := &webrtc.ICECandidatePair{Local: &webrtc.ICECandidate{Typ: tc.local, Protocol: webrtc.ICEProtocolUDP}, Remote: &webrtc.ICECandidate{Typ: tc.remote}}
		route := candidateRoute(pair)
		if route.Kind != tc.kind || route.LocalCandidate != tc.local.String() || route.RemoteCandidate != tc.remote.String() || route.Protocol != "udp" {
			t.Fatal(route)
		}
	}
	if candidateRoute(nil).Kind != "unknown" || (&peerSession{mode: "relay"}).route().Kind != "gateway-relay" {
		t.Fatal("missing route was mislabeled")
	}
}
