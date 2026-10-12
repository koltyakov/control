package transport

import (
	"encoding/json"
	"sync/atomic"

	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/protocol"
	"github.com/pion/webrtc/v4"
)

const maxTrickleCandidates = 128

// Candidates wait for their SDP on both ends. Queues and total candidate counts
// are bounded, and workers follow the link lifetime, not the setup deadline.
type linkICE struct {
	local, remote           chan webrtc.ICECandidateInit
	localReady, remoteReady chan struct{}
	remoteCount             int // Protected by the peer mutex.
}

func newLinkICE() *linkICE {
	return &linkICE{
		local:      make(chan webrtc.ICECandidateInit, maxTrickleCandidates),
		remote:     make(chan webrtc.ICECandidateInit, maxTrickleCandidates),
		localReady: make(chan struct{}), remoteReady: make(chan struct{}),
	}
}

func (p *Peer) startICE(l *link) {
	var count atomic.Int32
	l.pc.OnICECandidate(func(candidate *webrtc.ICECandidate) {
		value := webrtc.ICECandidateInit{}
		if candidate != nil {
			value = candidate.ToJSON()
		}
		if count.Add(1) > maxTrickleCandidates {
			return
		}
		select {
		case l.ice.local <- value:
		case <-l.conn.ctx.Done():
		default:
			// Never block Pion's gathering loop behind gateway backpressure.
			p.worker(func() { _ = l.conn.Close() })
		}
	})
	p.worker(func() {
		select {
		case <-l.ice.localReady:
		case <-l.conn.ctx.Done():
			return
		}
		for {
			select {
			case candidate := <-l.ice.local:
				if err := p.send(l.conn.ctx, &protocol.Packet{Kind: "ice", To: l.remote, Session: l.id, Data: model.JSON(candidate)}); err != nil {
					// ICE already in the SDP may still be sufficient. Do not kill
					// a healthy direct carrier merely because signaling vanished.
					p.log.Debug("ICE candidate signaling stopped", "peer", l.remote)
					return
				}
				if candidate.Candidate == "" {
					return
				}
			case <-l.conn.ctx.Done():
				return
			}
		}
	})
	p.worker(func() {
		select {
		case <-l.ice.remoteReady:
		case <-l.conn.ctx.Done():
			return
		}
		for {
			select {
			case candidate := <-l.ice.remote:
				if err := l.pc.AddICECandidate(candidate); err != nil {
					p.log.Debug("remote ICE candidate rejected", "peer", l.remote)
				}
				if candidate.Candidate == "" {
					return
				}
			case <-l.conn.ctx.Done():
				return
			}
		}
	})
}

func (p *Peer) receiveICE(msg *protocol.Packet) {
	if len(msg.Data) > 4096 {
		return
	}
	var candidate webrtc.ICECandidateInit
	if json.Unmarshal(msg.Data, &candidate) != nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	var queue chan webrtc.ICECandidateInit
	if l := p.links[msg.Session]; l != nil && l.remote == msg.From && l.ice != nil && l.conn.ctx.Err() == nil {
		if l.ice.remoteCount >= maxTrickleCandidates {
			return
		}
		l.ice.remoteCount++
		queue = l.ice.remote
	} else if setup := p.incomingSetups[msg.Session]; setup != nil && setup.remote == msg.From {
		if setup.candidateCount >= maxTrickleCandidates {
			return
		}
		setup.candidateCount++
		queue = setup.candidates
	}
	select {
	case queue <- candidate:
	default:
	}
}
