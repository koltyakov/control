package transport

import (
	"context"
	"io"
	"net"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/koltyakov/control/internal/gateway"
	"github.com/koltyakov/control/internal/identity"
	"github.com/koltyakov/control/internal/model"
	"github.com/pion/webrtc/v4"
)

func TestRelayReprobePreservesStreamsAndSharesFailureBackoff(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	const token = "relay-reprobe-test-token-1234567890"
	g, err := gateway.New(t.TempDir(), token)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = g.Close() }()
	server := httptest.NewServer(g.Handler())
	defer server.Close()
	makePeer := func(name string) *Peer {
		id, err := identity.Generate()
		if err != nil {
			t.Fatal(err)
		}
		p := New(Config{Gateway: server.URL, Token: token, Identity: id, DirectTimeout: time.Second, Node: model.Node{ID: id.ID, Name: name, PublicKey: id.Public}}, func(_ string, conn net.Conn) { _, _ = io.Copy(conn, conn) })
		p.api, p.trickleAPI = loopbackWebRTCAPI(p.cfg.DirectTimeout, false), loopbackWebRTCAPI(p.cfg.DirectTimeout, true)
		p.networkSnapshot = func() (string, error) { return "unchanged", nil }
		if name == "caller" {
			// No TCP ICE mux exists, so this carrier cannot find a usable route.
			var settings webrtc.SettingEngine
			settings.SetNetworkTypes([]webrtc.NetworkType{webrtc.NetworkTypeTCP4})
			p.trickleAPI = webrtc.NewAPI(webrtc.WithSettingEngine(settings))
		}
		if err := p.Start(ctx); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = p.Close() })
		return p
	}
	a, b := makePeer("caller"), makePeer("receiver")
	echo := func(conn net.Conn) {
		t.Helper()
		_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
		if _, err := conn.Write([]byte("once")); err != nil {
			t.Fatal(err)
		}
		var bytes [4]byte
		if _, err := io.ReadFull(conn, bytes[:]); err != nil || string(bytes[:]) != "once" {
			t.Fatal(bytes, err)
		}
	}
	old, err := a.Open(ctx, b.IdentityID())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = old.Close() }()
	oldSession := old.(*sessionStream).session
	for _, lane := range []Lane{BulkLane, InteractiveLane} {
		conn, err := a.OpenLane(ctx, b.IdentityID(), lane)
		if err != nil {
			t.Fatal(err)
		}
		echo(conn)
		_ = conn.Close()
	}
	if stats := a.Stats(); stats.RelayFallbacks != 1 || stats.RelaySessions != 3 {
		t.Fatalf("each lane repeated the same failed ICE attempt: %+v", stats)
	}
	forceProbe := func() {
		a.mu.Lock()
		a.directRetry[b.IdentityID()] = time.Now().Add(-time.Second)
		a.mu.Unlock()
		conn, err := a.Open(ctx, b.IdentityID())
		if err != nil {
			t.Fatal(err)
		}
		echo(conn)
		_ = conn.Close()
		waitFor(t, ctx, func() bool {
			a.mu.Lock()
			defer a.mu.Unlock()
			return len(a.pending) == 0
		})
	}
	forceProbe()
	if stats := a.Stats(); stats.RelayFallbacks != 2 || stats.RelaySessions != 3 {
		t.Fatalf("failed probe recreated the relay: %+v", stats)
	}
	echo(old)
	// All setup workers have finished. Restore UDP ICE for the next probe.
	a.trickleAPI = loopbackWebRTCAPI(a.cfg.DirectTimeout, true)
	forceProbe()
	conn, err := a.Open(ctx, b.IdentityID())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	if stream := conn.(*sessionStream); stream.mode != "webrtc" || stream.session.route().Kind != "direct" {
		t.Fatal("new stream did not use the recovered direct route", stream.mode)
	}
	echo(conn)
	echo(old)
	if oldSession.mux.IsClosed() {
		t.Fatal("upgrade interrupted an existing relay stream")
	}
	_ = old.Close()
	waitFor(t, ctx, oldSession.mux.IsClosed)
}

func TestCancelledDialIsNotReusedByLaterCallers(t *testing.T) {
	p := New(Config{}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	d := &sessionDial{cancel: cancel, waiters: 2}
	p.pending[sessionKey{"alias", ControlLane}] = d
	p.pending[sessionKey{"id", ControlLane}] = d
	p.releaseDialWaiterLocked(d)
	if ctx.Err() != nil || len(p.pending) != 2 {
		t.Fatal("one waiter cancelled shared setup")
	}
	p.releaseDialWaiterLocked(d)
	if ctx.Err() == nil || len(p.pending) != 0 {
		t.Fatal("last waiter left a cancelled setup reusable")
	}
}
