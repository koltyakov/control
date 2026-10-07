package transport

import (
	"context"
	"crypto/tls"
	"errors"
	"strings"
	"time"

	"github.com/hashicorp/yamux"
	"github.com/koltyakov/control/internal/identity"
	"github.com/pion/webrtc/v4"
)

// A carrier owns one ICE/DTLS/SCTP connection. Traffic lanes are independent
// ordered data channels, each with its own pinned TLS and stream multiplexer.
// Carrier state and link references are protected by the peer mutex.
type rtcCarrier struct {
	pc             *webrtc.PeerConnection
	remote         string
	links          map[string]*link
	shared, closed bool
}

func (p *Peer) releaseCarrierLocked(l *link, id string) bool {
	c := l.carrier
	if c == nil {
		return false
	}
	delete(c.links, id)
	if len(c.links) != 0 {
		return false
	}
	c.closed = true
	if p.carriers[c.remote] == c {
		delete(p.carriers, c.remote)
	}
	return true
}

func (p *Peer) closeCarrier(c *rtcCarrier) {
	p.mu.Lock()
	c.closed = true
	links := make([]*link, 0, len(c.links))
	for _, l := range c.links {
		links = append(links, l)
	}
	if p.carriers[c.remote] == c {
		delete(p.carriers, c.remote)
	}
	p.mu.Unlock()
	for _, l := range links {
		go func() { _ = l.conn.Close() }()
	}
}

func (p *Peer) receiveChannel(base *link, dc *webrtc.DataChannel) {
	if dc.Label() == "control-v1" {
		p.bindDataChannel(base, dc)
		return
	}
	id, ok := strings.CutPrefix(dc.Label(), "control-v1/")
	p.mu.Lock()
	shared := base.carrier.shared && !base.carrier.closed
	p.mu.Unlock()
	if !ok || !shared || !validChannelID(id) {
		_ = dc.Close()
		return
	}
	select {
	case p.setups <- struct{}{}:
	default:
		_ = dc.Close()
		return
	}
	// Bind before this callback returns. Pion starts delivering the channel's
	// messages afterwards and drops any that arrive without a handler, which
	// lost the dialer's TLS ClientHello and stalled setup until the direct
	// timeout forced the relay. Only the handshake runs asynchronously.
	l, err := p.newLink(id, base.carrier.remote, "webrtc", base.carrier)
	if err != nil {
		<-p.setups
		_ = dc.Close()
		return
	}
	p.bindDataChannel(l, dc)
	if !p.worker(func() {
		defer func() { <-p.setups }()
		p.acceptChannel(l)
	}) {
		<-p.setups
		_ = l.conn.Close()
	}
}

func validChannelID(id string) bool {
	if len(id) != 32 {
		return false
	}
	for _, ch := range id {
		if (ch < '0' || ch > '9') && (ch < 'a' || ch > 'f') {
			return false
		}
	}
	return true
}

func (p *Peer) acceptChannel(l *link) {
	handedOver := false
	defer func() {
		if !handedOver {
			_ = l.conn.Close()
		}
	}()
	ctx, cancel := context.WithTimeout(p.ctx, 15*time.Second)
	defer cancel()
	secure := tls.Server(l.conn, p.cfg.Identity.TLS(l.remote))
	if err := secure.HandshakeContext(ctx); err != nil {
		return
	}
	mux, err := yamux.Server(newFrameConn(secure, l.conn), muxConfig(IncomingLane))
	if err != nil {
		return
	}
	s := &peerSession{mux: mux, id: l.id, remote: l.remote, mode: "webrtc", lane: IncomingLane, outgoing: make(chan struct{}, 128), incoming: make(chan struct{}, 128)}
	if !p.registerSession(s) {
		_ = mux.Close()
		return
	}
	// Release the setup reservation before serving this long-lived channel.
	// The callback's reservation is released by its caller, so serve separately.
	handedOver = p.worker(func() { p.serve(s); _ = l.conn.Close() })
	if !handedOver {
		_ = mux.Close()
	}
}

func (p *Peer) dialChannel(ctx context.Context, carrier *rtcCarrier, lane Lane) (_ *peerSession, err error) {
	id := identity.NewID()
	l, err := p.newLink(id, carrier.remote, "webrtc", carrier)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			_ = l.conn.Close()
		}
	}()
	dc, err := carrier.pc.CreateDataChannel("control-v1/"+id, nil)
	if err != nil {
		return nil, err
	}
	p.bindDataChannel(l, dc)
	select {
	case <-l.ready:
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-l.conn.ctx.Done():
		return nil, errors.New("peer closed data channel")
	}
	secure := tls.Client(l.conn, p.cfg.Identity.TLS(carrier.remote))
	if err := secure.HandshakeContext(ctx); err != nil {
		return nil, err
	}
	mux, err := yamux.Client(newFrameConn(secure, l.conn), muxConfig(lane))
	if err != nil {
		return nil, err
	}
	return &peerSession{mux: mux, id: id, remote: carrier.remote, mode: "webrtc", lane: lane, outgoing: make(chan struct{}, 128), incoming: make(chan struct{}, 128)}, nil
}
