package transport

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/koltyakov/control/internal/identity"
	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/wire"
)

func TestInteractiveTLSNegotiation(t *testing.T) {
	t.Parallel()
	newPeer := func() *Peer {
		t.Helper()
		id, err := identity.Generate()
		if err != nil {
			t.Fatal(err)
		}
		return New(Config{Identity: id}, nil)
	}
	dialer, receiver := newPeer(), newPeer()
	for _, tc := range []struct {
		name               string
		lane               Lane
		oldClient, oldPeer bool
		want               string
	}{
		{name: "interactive", lane: InteractiveLane, want: interactiveStreamsALPN},
		{name: "control", lane: ControlLane},
		{name: "bulk", lane: BulkLane},
		{name: "older worker", lane: InteractiveLane, oldPeer: true},
		{name: "older client", lane: InteractiveLane, oldClient: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			a, b := net.Pipe()
			defer func() { _ = a.Close(); _ = b.Close() }()
			cConfig := dialer.sessionTLSConfig(receiver.IdentityID(), tc.lane)
			sConfig := receiver.sessionTLSConfig(dialer.IdentityID(), IncomingLane)
			if tc.oldClient {
				cConfig = dialer.cfg.Identity.TLS(receiver.IdentityID())
			}
			if tc.oldPeer {
				sConfig = receiver.cfg.Identity.TLS(dialer.IdentityID())
			}
			server := tls.Server(b, sConfig)
			done := make(chan error, 1)
			go func() { done <- server.HandshakeContext(ctx) }()
			client := tls.Client(a, cConfig)
			if err := client.HandshakeContext(ctx); err != nil {
				t.Fatal(err)
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			if c, s := client.ConnectionState().NegotiatedProtocol, server.ConnectionState().NegotiatedProtocol; c != tc.want || s != tc.want {
				t.Fatalf("ALPN client=%q server=%q, want %q", c, s, tc.want)
			}
		})
	}
}

func TestTCPStreamCapacityWait(t *testing.T) {
	t.Parallel()
	for _, global := range []bool{false, true} {
		name := "session"
		if global {
			name = "process"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			p := New(Config{}, nil)
			p.ctx = t.Context()
			s := &peerSession{lane: InteractiveLane, outgoing: make(chan struct{}, 1)}
			full := s.outgoing
			if global {
				full = p.outgoingSlots[InteractiveLane]
			}
			for range cap(full) {
				full <- struct{}{}
			}
			ctx := context.WithValue(t.Context(), tcpSlotWaitKey{}, true)
			start := time.Now()
			if _, err := p.openStream(ctx, s); !errors.Is(err, ErrStreamCapacity) || time.Since(start) < tcpStreamSlotWait {
				t.Fatal("expected a bounded capacity error", err)
			}
			if len(full) != cap(full) || (global && len(s.outgoing) != 0) {
				t.Fatal("slot wait leaked a reservation")
			}
		})
	}
}

func TestInteractiveWorkerCapacityReply(t *testing.T) {
	t.Parallel()
	p := New(Config{}, func(string, net.Conn) { t.Error("refused stream invoked the handler") })
	p.ctx = t.Context()
	s := &peerSession{incoming: make(chan struct{}, 1)}
	slots := make(chan struct{}, 1)
	slots <- struct{}{}
	a, b := net.Pipe()
	defer p.wg.Wait()
	defer func() { _ = a.Close(); _ = b.Close() }()
	_ = a.SetDeadline(time.Now().Add(5 * time.Second))
	start := time.Now()
	p.queueInteractiveStream(s, b, slots)
	var response model.Response
	if err := wire.ReadFrame(a, &response); err != nil {
		t.Fatal(err)
	}
	if !IsStreamCapacity(response.Err()) || time.Since(start) < tcpStreamSlotWait {
		t.Fatalf("expected a bounded worker refusal, got %+v", response)
	}
	p.wg.Wait()
	if len(s.incoming) != 0 || len(slots) != 1 || len(p.interactiveWaiters) != 0 {
		t.Fatal("worker leaked a stream reservation")
	}
}
