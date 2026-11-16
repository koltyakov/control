package transport

import (
	"encoding/json"
	"time"

	"github.com/koltyakov/control/internal/gateway"
	"github.com/koltyakov/control/internal/protocol"
	"github.com/koltyakov/control/internal/update"
)

type peerDrain struct {
	id    string
	until time.Time
}

func (p *Peer) relayDrainingLocked(remote string) bool {
	now := time.Now()
	return now.Before(p.relayDrainUntil) || now.Before(p.relayPeerDrains[remote].until) ||
		p.cfg.Identity != nil && now.Before(p.relayPeerDrains[p.cfg.Identity.ID].until)
}

func (p *Peer) receiveGatewayDrain(msg *protocol.Packet) {
	if msg.From != update.GatewaySender || !validChannelID(msg.Session) || len(msg.Data) > 1024 {
		return
	}
	var request gateway.RelayDrainRequest
	if json.Unmarshal(msg.Data, &request) != nil {
		return
	}
	if request.Peer != "" && !update.ValidDigest(request.Peer) {
		return
	}
	p.mu.Lock()
	if msg.Kind == "gateway.resume" {
		if request.Peer != "" {
			if p.relayPeerDrains[request.Peer].id == msg.Session {
				delete(p.relayPeerDrains, request.Peer)
			}
		} else if p.relayDrainID == msg.Session {
			p.relayDrainUntil, p.relayDrainID = time.Time{}, ""
		}
		p.mu.Unlock()
		return
	}
	if !time.Now().Before(request.Until) || request.Until.After(time.Now().Add(time.Minute)) {
		p.mu.Unlock()
		return
	}
	if request.Peer == "" {
		p.relayDrainID, p.relayDrainUntil = msg.Session, request.Until
	} else {
		for id, drain := range p.relayPeerDrains {
			if !time.Now().Before(drain.until) {
				delete(p.relayPeerDrains, id)
			}
		}
		if p.relayPeerDrains == nil {
			p.relayPeerDrains = map[string]peerDrain{}
		}
		if len(p.relayPeerDrains) >= 256 && p.relayPeerDrains[request.Peer].id == "" {
			p.mu.Unlock()
			return
		}
		p.relayPeerDrains[request.Peer] = peerDrain{msg.Session, request.Until}
	}
	var sessions []*peerSession
	// Only outgoing owners retire carriers. An incoming side can appear idle
	// while a remote OpenStream is still in flight. The owner counts that open
	// before sending it, and waits for yamux's stream/FIN bookkeeping to drain.
	for _, s := range p.allSessions {
		if s.mode == "relay" && s.lane != IncomingLane && !s.draining && p.relayDrainingLocked(s.remote) {
			s.draining = true
			sessions = append(sessions, s)
		}
	}
	p.mu.Unlock()
	for _, s := range sessions {
		p.worker(func() { p.drainSession(s) })
	}
}

// ParkGateway is used only after the node has durably reserved idle admission.
// No relay, pending connection setup, or admitted HTTP request may be severed.
// The ordinary reconnect loop wakes at the bounded route deadline.
func (p *Peer) ParkGateway(until time.Time) bool {
	p.mu.Lock()
	if p.closed || p.connectCancel != nil || p.ws == nil {
		p.mu.Unlock()
		return false
	}
	for _, l := range p.links {
		if l.mode == "relay" {
			p.mu.Unlock()
			return false
		}
	}
	if !p.cfg.Route.ParkIfIdle(until) {
		p.mu.Unlock()
		return false
	}
	ws := p.ws
	p.gatewayWS.Store(nil)
	p.mu.Unlock()
	_ = ws.CloseNow()
	return true
}
