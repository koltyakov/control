package transport

import (
	"encoding/json"
	"time"

	"github.com/koltyakov/control/internal/gateway"
	"github.com/koltyakov/control/internal/protocol"
	"github.com/koltyakov/control/internal/update"
)

func (p *Peer) receiveGatewayDrain(msg *protocol.Packet) {
	if msg.From != update.GatewaySender || !validChannelID(msg.Session) || len(msg.Data) > 1024 {
		return
	}
	var request gateway.RelayDrainRequest
	if json.Unmarshal(msg.Data, &request) != nil {
		return
	}
	p.mu.Lock()
	if msg.Kind == "gateway.resume" {
		if p.relayDrainID == msg.Session {
			p.relayDrainUntil, p.relayDrainID = time.Time{}, ""
		}
		p.mu.Unlock()
		return
	}
	if !time.Now().Before(request.Until) || request.Until.After(time.Now().Add(time.Minute)) {
		p.mu.Unlock()
		return
	}
	p.relayDrainID, p.relayDrainUntil = msg.Session, request.Until
	var sessions []*peerSession
	// Only outgoing owners retire carriers. An incoming side can appear idle
	// while a remote OpenStream is still in flight. The owner counts that open
	// before sending it, and waits for yamux's stream/FIN bookkeeping to drain.
	for _, s := range p.allSessions {
		if s.mode == "relay" && s.lane != IncomingLane && !s.draining {
			s.draining = true
			sessions = append(sessions, s)
		}
	}
	p.mu.Unlock()
	for _, s := range sessions {
		p.worker(func() { p.drainSession(s) })
	}
}
