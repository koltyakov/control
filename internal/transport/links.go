package transport

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"time"

	"github.com/hashicorp/yamux"
	"github.com/koltyakov/control/internal/identity"
	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/protocol"
	"github.com/pion/webrtc/v4"
)

func (p *Peer) newLink(ctx context.Context, id, remote, mode string, shared ...*rtcCarrier) (*link, error) {
	l := &link{id: id, remote: remote, mode: mode, answer: make(chan webrtc.SessionDescription, 1), ready: make(chan struct{})}
	l.conn = newConn(p.ctx, func(ctx context.Context, b []byte) error {
		return p.send(ctx, &protocol.Packet{Kind: "data", To: remote, Session: id, Data: b})
	}, func() {
		l.dcMu.Lock()
		dc := l.dc
		l.dcMu.Unlock()
		if dc != nil {
			p.closeAsync(func() { _ = dc.Close() })
		}
		p.mu.Lock()
		delete(p.links, id)
		p.linkIndex.Delete(id)
		closePC := p.releaseCarrierLocked(l, id)
		notify := l.generation == p.networkGeneration
		p.mu.Unlock()
		if closePC {
			p.closeAsync(func() { _ = l.pc.Close() })
		}
		if notify {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			_ = p.send(ctx, &protocol.Packet{Kind: "close", To: remote, Session: id})
		}
	})
	if mode == "webrtc" {
		if len(shared) != 0 {
			l.carrier, l.pc = shared[0], shared[0].pc
		} else {
			p.mu.Lock()
			servers := p.iceServers
			trickle := p.cfg.Node.TrickleICE && p.trickle[remote]
			p.mu.Unlock()
			api := p.api
			if trickle {
				api = p.trickleAPI
			}
			pc, err := api.NewPeerConnection(webrtc.Configuration{ICEServers: servers})
			if err != nil {
				l.conn.cancel()
				return nil, err
			}
			l.pc = pc
			l.carrier = &rtcCarrier{pc: pc, remote: remote, links: map[string]*link{}, changed: make(chan struct{})}
			if trickle {
				l.ice = newLinkICE()
			}
			pc.OnDataChannel(func(dc *webrtc.DataChannel) { p.receiveChannel(l, dc) })
			pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
				p.carrierStateChanged(l.carrier, state)
			})
		}
	}
	p.mu.Lock()
	if err := ctx.Err(); err != nil {
		p.mu.Unlock()
		l.conn.cancel()
		if l.pc != nil && len(shared) == 0 {
			_ = l.pc.Close()
		}
		return nil, err
	}
	if _, exists := p.links[id]; exists || len(p.links) >= maxPeerLinks || (l.carrier != nil && (l.carrier.closed || l.carrier.disconnected)) {
		p.mu.Unlock()
		l.conn.cancel()
		if l.pc != nil && len(shared) == 0 {
			_ = l.pc.Close()
		}
		return nil, errors.New("duplicate session or peer link limit reached")
	}
	p.links[id] = l
	p.linkIndex.Store(id, l)
	l.generation = p.networkGeneration
	if l.ice != nil {
		if setup := p.incomingSetups[id]; setup != nil && setup.remote == remote {
			l.ice.remote = setup.candidates
			l.ice.remoteCount = setup.candidateCount
		}
	}
	l.conn.packetSize = p.packetSizeLocked(remote)
	if l.carrier != nil {
		l.carrier.links[id] = l
		l.carrier.shared = p.cfg.Node.PeerChannels && p.channels[remote]
		if old := p.carriers[remote]; l.carrier.shared && (old == nil || old.closed || old.disconnected) {
			p.carriers[remote] = l.carrier
		}
	}
	p.mu.Unlock()
	if l.ice != nil {
		p.startICE(l)
	}
	return l, nil
}

func (p *Peer) bindDataChannel(l *link, dc *webrtc.DataChannel) {
	if !dc.Ordered() || dc.MaxRetransmits() != nil || dc.MaxPacketLifeTime() != nil {
		_ = dc.Close()
		return
	}
	bound := false
	l.dcOnce.Do(func() { bound = true; p.bindReliableChannel(l, dc) })
	if !bound {
		_ = dc.Close()
	}
}

// overflow records a receive queue that filled faster than TLS consumed it.
// The link closes; logging is limited so a burst cannot flood the log.
func (p *Peer) overflow(l *link) {
	if l.conn.ctx.Err() != nil {
		return
	}
	p.stats.linkOverflows.Add(1)
	now := time.Now().UnixNano()
	last := p.stats.lastOverflowLog.Load()
	if now-last > int64(10*time.Second) && p.stats.lastOverflowLog.CompareAndSwap(last, now) {
		p.log.Warn("peer link receive queue overflowed; closing link", "peer", l.remote, "mode", l.mode)
	}
}

func (p *Peer) bindReliableChannel(l *link, dc *webrtc.DataChannel) {
	l.dcMu.Lock()
	l.dc = dc
	l.dcMu.Unlock()
	if l.conn.ctx.Err() != nil {
		_ = dc.Close()
		return
	}
	low := make(chan struct{}, 1)
	dc.SetBufferedAmountLowThreshold(channelBufferedLow)
	dc.OnBufferedAmountLow(func() {
		select {
		case low <- struct{}{}:
		default:
		}
	})
	l.conn.send = func(ctx context.Context, b []byte) error {
		for dc.BufferedAmount() > channelBufferedMax {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-low:
			}
		}
		return dc.Send(b)
	}
	dc.OnClose(func() { go func() { _ = l.conn.Close() }() })
	dc.OnOpen(func() {
		raw, err := dc.DetachWithDeadline()
		if err != nil {
			_ = l.conn.Close()
			return
		}
		p.mu.Lock()
		if p.closed || l.conn.ctx.Err() != nil {
			p.mu.Unlock()
			_ = raw.Close()
			return
		}
		p.wg.Add(1)
		p.mu.Unlock()
		go func() {
			defer p.wg.Done()
			defer func() { _ = l.conn.Close() }()
			stop := context.AfterFunc(l.conn.ctx, func() { _ = raw.Close() })
			defer stop()
			defer func() { _ = raw.Close() }()
			// Pion's callback loop allocates a fresh message for every read.
			// Detached reads reuse this bounded buffer; push copies into the
			// existing size-class pools until TLS consumes each packet.
			buffer := make([]byte, largePacketSize+1)
			l.readyOnce.Do(func() { close(l.ready) })
			for {
				count, text, readErr := raw.ReadDataChannel(buffer)
				if readErr != nil || text || count == 0 || count > largePacketSize {
					return
				}
				if !l.conn.push(buffer[:count]) {
					p.overflow(l)
					return
				}
			}
		}()
	})
}

func (p *Peer) accept(ctx context.Context, msg *protocol.Packet) {
	// Check the scoped directory before processing an SDP offer or accepting
	// relay bytes. TLS then proves possession of this enrolled identity.
	if _, err := p.Resolve(ctx, msg.From); err != nil {
		return
	}
	if ctx.Err() != nil {
		return
	}
	mode := "relay"
	if len(msg.Data) > 0 {
		mode = "webrtc"
	}
	if mode == "webrtc" && p.cfg.RelayOnly {
		_ = p.send(p.ctx, &protocol.Packet{Kind: "close", To: msg.From, Session: msg.Session})
		return
	}
	l, err := p.newLink(ctx, msg.Session, msg.From, mode)
	if err != nil {
		return
	}
	handedOver := false
	defer func() {
		if !handedOver {
			_ = l.conn.Close()
		}
	}()
	if mode == "webrtc" {
		var offer webrtc.SessionDescription
		if json.Unmarshal(msg.Data, &offer) != nil {
			return
		}
		if err = l.pc.SetRemoteDescription(offer); err != nil {
			return
		}
		if l.ice != nil {
			close(l.ice.remoteReady)
		}
		answer, e := l.pc.CreateAnswer(nil)
		if e != nil {
			return
		}
		gather := webrtc.GatheringCompletePromise(l.pc)
		if err = l.pc.SetLocalDescription(answer); err != nil {
			return
		}
		if l.ice == nil {
			select {
			case <-gather:
			case <-ctx.Done():
				return
			case <-l.conn.ctx.Done():
				return
			}
		}
		if err = p.send(ctx, &protocol.Packet{Kind: "answer", To: msg.From, Session: msg.Session, Data: model.JSON(l.pc.LocalDescription())}); err != nil {
			return
		}
		if l.ice != nil {
			close(l.ice.localReady)
		}
		select {
		case <-l.ready:
		case <-ctx.Done():
			return
		case <-l.conn.ctx.Done():
			return
		}
	} else {
		if err = p.send(ctx, &protocol.Packet{Kind: "answer", To: msg.From, Session: msg.Session}); err != nil {
			return
		}
	}
	s, err := p.secureSession(ctx, l, msg.Session, msg.From, mode, IncomingLane)
	if err != nil {
		return
	}
	if p.registerSession(s) != nil {
		_ = s.mux.Close()
		return
	}
	handedOver = p.worker(func() { p.serve(s); _ = l.conn.Close() })
	if !handedOver {
		_ = s.mux.Close()
	}
}

// muxConfig sets each side's receive window. Windows bound buffering per
// stream; on high-latency paths they also cap per-stream throughput.
func muxConfig(lane Lane) *yamux.Config {
	cfg := yamux.DefaultConfig()
	cfg.LogOutput = io.Discard
	cfg.StreamOpenTimeout = 15 * time.Second
	cfg.StreamCloseTimeout = 5 * time.Second
	cfg.AcceptBacklog = 128
	switch lane {
	case BulkLane:
		// Artifact and clipboard downloads.
		cfg.MaxStreamWindowSize = 4 << 20
	case InteractiveLane, IncomingLane:
		// Forwarded TCP responses, and uploads to the accepting side.
		cfg.MaxStreamWindowSize = 1 << 20
	}
	return cfg
}

func (p *Peer) dial(ctx context.Context, remote, mode string, lane Lane) (_ *peerSession, err error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if mode == "relay" {
		p.mu.Lock()
		draining := p.relayDrainingLocked(remote)
		p.mu.Unlock()
		if draining {
			return nil, transientSetup("gateway relay is draining for maintenance")
		}
	}
	if mode == "webrtc" {
		p.mu.Lock()
		carrier := p.carriers[remote]
		if carrier != nil && (carrier.closed || carrier.disconnected) {
			carrier = nil
		}
		p.mu.Unlock()
		if carrier != nil {
			return p.dialChannel(ctx, carrier, lane)
		}
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	id := identity.NewID()
	l, err := p.newLink(ctx, id, remote, mode)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			_ = l.conn.Close()
		}
	}()
	var offer []byte
	if mode == "webrtc" {
		dc, e := l.pc.CreateDataChannel("control-v1", nil)
		if e != nil {
			return nil, e
		}
		p.bindDataChannel(l, dc)
		sdp, e := l.pc.CreateOffer(nil)
		if e != nil {
			return nil, e
		}
		gather := webrtc.GatheringCompletePromise(l.pc)
		if e = l.pc.SetLocalDescription(sdp); e != nil {
			return nil, e
		}
		if l.ice == nil {
			select {
			case <-gather:
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-l.conn.ctx.Done():
				return nil, errors.New("peer closed during candidate gathering")
			}
		}
		offer = model.JSON(l.pc.LocalDescription())
	}
	if err = p.send(ctx, &protocol.Packet{Kind: "open", To: remote, Session: id, Data: offer}); err != nil {
		return nil, err
	}
	if l.ice != nil {
		close(l.ice.localReady)
	}
	if mode == "webrtc" {
		select {
		case answer := <-l.answer:
			if err = l.pc.SetRemoteDescription(answer); err != nil {
				return nil, err
			}
			if l.ice != nil {
				close(l.ice.remoteReady)
			}
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-l.conn.ctx.Done():
			return nil, errors.New("peer rejected direct session")
		}
	}
	select {
	case <-l.ready:
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-l.conn.ctx.Done():
		// The remote side or the gateway closed this setup (peer offline,
		// relay limit or maintenance). The request was never sent.
		return nil, transientSetup("peer closed session")
	}
	return p.secureSession(ctx, l, id, remote, mode, lane)
}

// secureSession runs the pinned TLS 1.3 handshake on a link and multiplexes
// it. Every WebRTC, relay and shared-channel path uses it, so identity
// pinning and session construction cannot drift between transports.
// IncomingLane makes this side the TLS and yamux server; any other lane
// dials. The caller owns l.conn on error.
func (p *Peer) secureSession(ctx context.Context, l *link, id, remote, mode string, lane Lane) (*peerSession, error) {
	config := p.cfg.Identity.TLS(remote)
	var secure *tls.Conn
	if lane == IncomingLane {
		secure = tls.Server(l.conn, config)
	} else {
		secure = tls.Client(l.conn, config)
	}
	if err := secure.HandshakeContext(ctx); err != nil {
		return nil, err
	}
	frames := newFrameConn(secure, l.conn)
	var mux *yamux.Session
	var err error
	if lane == IncomingLane {
		mux, err = yamux.Server(frames, muxConfig(lane))
	} else {
		mux, err = yamux.Client(frames, muxConfig(lane))
	}
	if err != nil {
		return nil, err
	}
	return &peerSession{mux: mux, link: l, id: id, remote: remote, lane: lane, mode: mode, outgoing: make(chan struct{}, 128), incoming: make(chan struct{}, 128)}, nil
}
