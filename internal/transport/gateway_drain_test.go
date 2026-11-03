package transport

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/koltyakov/control/internal/gateway"
	"github.com/koltyakov/control/internal/identity"
	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/protocol"
	"github.com/koltyakov/control/internal/update"
)

func TestGatewayDrainRetiresIdleRelayWithoutInterruptingStreams(t *testing.T) {
	t.Parallel()
	for _, relay := range []bool{true, false} {
		t.Run(fmt.Sprint(relay), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
			defer cancel()
			const token = "relay-drain-fixture-token-1234567890"
			g, err := gateway.New(t.TempDir(), token)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = g.Close() }()
			server := httptest.NewServer(g.Handler())
			defer server.Close()
			makePeer := func(name string, client bool) *Peer {
				id, err := identity.Generate()
				if err != nil {
					t.Fatal(err)
				}
				if client {
					name = "cli-" + id.ID[:16]
				}
				p := New(Config{Gateway: server.URL, Token: token, Client: client, Identity: id, RelayOnly: relay, Node: model.Node{ID: id.ID, Name: name, PublicKey: id.Public}}, func(_ string, conn net.Conn) { _, _ = io.Copy(conn, conn) })
				p.api, p.trickleAPI = loopbackWebRTCAPI(p.cfg.DirectTimeout, false), loopbackWebRTCAPI(p.cfg.DirectTimeout, true)
				p.networkSnapshot = func() (string, error) { return "unchanged", nil }
				if err := p.Start(ctx); err != nil {
					t.Fatal(err)
				}
				return p
			}
			a, b := makePeer("caller", true), makePeer("receiver", false)
			defer func() { _ = a.Close(); _ = b.Close() }()
			active, err := a.OpenLane(ctx, b.IdentityID(), InteractiveLane)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = active.Close() }()
			idle, err := a.Open(ctx, b.IdentityID())
			if err != nil {
				t.Fatal(err)
			}
			idleSession := idle.(*sessionStream).session
			_ = idle.Close()
			waitFor(t, ctx, func() bool { return idleSession.mux.NumStreams() == 0 })
			packet := &protocol.Packet{Kind: "gateway.drain", From: "forged", Session: identity.NewID(), Data: model.JSON(gateway.RelayDrainRequest{Until: time.Now().Add(10 * time.Second)})}
			a.receive(packet)
			a.mu.Lock()
			forged := !a.relayDrainUntil.IsZero()
			a.mu.Unlock()
			if forged {
				t.Fatal("peer forged gateway drain")
			}
			packet.From = update.GatewaySender
			a.receive(packet)
			b.receive(packet)
			if relay {
				waitFor(t, ctx, idleSession.mux.IsClosed)
				if conn, err := a.Open(ctx, b.IdentityID()); err == nil {
					_ = conn.Close()
					t.Fatal("new stream used a draining carrier")
				}
			} else if idleSession.mux.IsClosed() {
				t.Fatal("drain retired direct carrier")
			}
			_ = active.SetDeadline(time.Now().Add(3 * time.Second))
			if _, err := active.Write([]byte("once")); err != nil {
				t.Fatal("drain interrupted live stream", err)
			}
			var data [4]byte
			if _, err := io.ReadFull(active, data[:]); err != nil || string(data[:]) != "once" {
				t.Fatal("lost accepted bytes", string(data[:]), err)
			}
			activeSession := active.(*sessionStream).session
			_ = active.Close()
			if relay {
				waitFor(t, ctx, activeSession.mux.IsClosed)
			}
			packet.Kind = "gateway.resume"
			a.receive(packet)
			b.receive(packet)
			conn, err := a.Open(ctx, b.IdentityID())
			if err != nil {
				t.Fatal("resume did not restore relay admission", err)
			}
			_ = conn.Close()
		})
	}
}
