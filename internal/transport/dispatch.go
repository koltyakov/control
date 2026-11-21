package transport

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/koltyakov/control/internal/protocol"
	"github.com/koltyakov/control/internal/update"
	"github.com/pion/webrtc/v4"
)

// gatewayOnlyKind reports packet kinds that only the gateway originates.
// receive drops them from any other sender before a handler runs, so a new
// management kind cannot forget the sender check.
func gatewayOnlyKind(kind string) bool {
	switch kind {
	case "proxy.request", "secrets.request", "gateway.drain", "gateway.resume", "gateway.ping",
		"directory.changed", "machine.state", "wake.request", "startup.request":
		return true
	}
	return strings.HasPrefix(kind, "update.")
}

// runBounded starts a gateway management handler in one of a fixed number of
// slots, dropping the request when they are full so the gateway reader never
// blocks and request bursts cannot spawn unbounded goroutines.
func (p *Peer) runBounded(slots chan struct{}, fn func()) {
	select {
	case slots <- struct{}{}:
		if !p.worker(func() { defer func() { <-slots }(); fn() }) {
			<-slots
		}
	default:
	}
}

func (p *Peer) receive(msg *protocol.Packet) {
	if gatewayOnlyKind(msg.Kind) && msg.From != update.GatewaySender {
		return
	}
	switch {
	case msg.Kind == "proxy.request":
		if p.proxyManagement != nil && len(msg.Data) <= 8192 {
			p.runBounded(p.proxySlots, func() { p.proxyManagement(msg.Data) })
		}
		return
	case msg.Kind == "secrets.request":
		if p.secrets != nil && len(msg.Data) <= 12<<10 {
			p.runBounded(p.secretSlots, func() { p.secrets(msg.Data) })
		}
		return
	case msg.Kind == "gateway.drain" || msg.Kind == "gateway.resume":
		p.receiveGatewayDrain(msg)
		return
	case msg.Kind == "lan.probe":
		p.receiveLANProbe(msg)
		return
	case msg.Kind == "gateway.ping":
		if validChannelID(msg.Session) {
			// Do not block the gateway reader while the shared writer drains.
			p.worker(func() {
				_ = p.send(p.ctx, &protocol.Packet{Kind: "gateway.pong", Session: msg.Session})
			})
		}
		return
	case msg.Kind == "directory.changed":
		p.mu.Lock()
		p.resolved = map[string]string{}
		p.lookups = map[string]lookupEntry{}
		p.directoryGeneration++
		p.mu.Unlock()
		return
	case msg.Kind == "machine.state":
		p.signalMachineState()
		return
	case msg.Kind == "wake.request":
		if p.wake != nil {
			data := msg.Data
			// Sending can wait on the network; keep the gateway reader free.
			p.runBounded(p.controlSlots, func() { p.wake(data) })
		}
		return
	case msg.Kind == "startup.request":
		if p.startup != nil && len(msg.Data) <= 1024 {
			p.runBounded(p.controlSlots, func() { p.startup(msg.Data) })
		}
		return
	case strings.HasPrefix(msg.Kind, "update."):
		if p.control != nil {
			p.control(msg.Kind, msg.Data)
		}
		return
	}
	if msg.Kind == "offline" {
		p.mu.Lock()
		// The identity left the gateway: stop reusing its authority lookup, and
		// forget its membership unless a direct link is still carrying traffic.
		p.evictLookupLocked(msg.From)
		// Lookups started before this notification must not restore stale entries.
		p.directoryGeneration++
		if !p.memberInUseLocked(msg.From) {
			p.forgetMemberLocked(msg.From)
		}
		var connections []*packetConn
		for _, l := range p.links {
			if l.remote == msg.From && l.mode == "relay" {
				connections = append(connections, l.conn)
			}
		}
		p.mu.Unlock()
		for _, c := range connections {
			go func() { _ = c.Close() }()
		}
		return
	}
	if msg.Kind == "open" {
		p.mu.Lock()
		_, exists := p.links[msg.Session]
		exists = exists || p.incomingSetups[msg.Session] != nil
		full := len(p.links) >= maxPeerLinks || p.closed || p.remoteLinksLocked(msg.From) >= maxRemoteLinks
		p.mu.Unlock()
		if exists {
			return
		}
		if full {
			p.rejectSetup(msg)
			return
		}
		select {
		case p.setups <- struct{}{}:
			ctx, cancel := context.WithTimeout(p.ctx, 20*time.Second)
			setup := &incomingSetup{remote: msg.From, cancel: cancel, candidates: make(chan webrtc.ICECandidateInit, maxTrickleCandidates)}
			p.mu.Lock()
			p.incomingSetups[msg.Session] = setup
			p.mu.Unlock()
			cleanup := func() { cancel(); <-p.setups; p.mu.Lock(); delete(p.incomingSetups, msg.Session); p.mu.Unlock() }
			if !p.worker(func() { defer cleanup(); p.accept(ctx, msg) }) {
				cleanup()
			}
		default:
			// Refuse excess setup without blocking the gateway reader on a write.
			p.rejectSetup(msg)
		}
		return
	}
	if msg.Kind == "ice" {
		p.receiveICE(msg)
		return
	}
	p.mu.Lock()
	l := p.links[msg.Session]
	setup := p.incomingSetups[msg.Session]
	p.mu.Unlock()
	if l == nil || l.remote != msg.From {
		if msg.Kind == "close" && setup != nil && setup.remote == msg.From {
			setup.cancel()
		}
		return
	}
	switch msg.Kind {
	case "data":
		if l.mode == "relay" && !l.conn.pushOwned(msg.Data) {
			if len(msg.Data) > 0 && len(msg.Data) <= largePacketSize {
				p.overflow(l)
			}
			go func() { _ = l.conn.Close() }()
		}
	case "answer":
		if l.mode == "relay" {
			l.readyOnce.Do(func() { close(l.ready) })
			return
		}
		var answer webrtc.SessionDescription
		if json.Unmarshal(msg.Data, &answer) == nil {
			select {
			case l.answer <- answer:
			default:
			}
		}
	case "close":
		go func() { _ = l.conn.Close() }()
	}
}

const (
	// maxPeerLinks bounds all incoming and outgoing links.
	maxPeerLinks = 256
	// maxRemoteLinks keeps one remote from taking every incoming link.
	maxRemoteLinks = 64
)

// remoteLinksLocked counts established links and pending incoming setups
// with one remote. Caller holds p.mu.
func (p *Peer) remoteLinksLocked(remote string) int {
	n := 0
	for _, l := range p.links {
		if l.remote == remote {
			n++
		}
	}
	for _, setup := range p.incomingSetups {
		if setup.remote == remote {
			n++
		}
	}
	return n
}

// rejectSetup answers a refused open with close so the dialer fails fast
// instead of waiting out its setup timeout. Replies are best effort and
// bounded; excess refusals are dropped as before.
func (p *Peer) rejectSetup(msg *protocol.Packet) {
	select {
	case p.rejects <- struct{}{}:
	default:
		return
	}
	reply := &protocol.Packet{Kind: "close", To: msg.From, Session: msg.Session}
	if !p.worker(func() {
		defer func() { <-p.rejects }()
		ctx, cancel := context.WithTimeout(p.ctx, 2*time.Second)
		defer cancel()
		_ = p.send(ctx, reply)
	}) {
		<-p.rejects
	}
}
