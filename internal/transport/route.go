package transport

import (
	"github.com/koltyakov/control/internal/model"
	"github.com/pion/webrtc/v4"
)

// ConnectionRoutes separates direct ICE paths, TURN, and the gateway relay.
// Like Connections, it counts each usable peer once and prefers WebRTC over
// gateway relay sessions, including relays draining after a direct re-probe.
func (p *Peer) ConnectionRoutes() model.ConnectionRouteCounts {
	return connectionRouteCounts(p.SessionStats())
}

func connectionRouteCounts(sessions []model.PeerSession) model.ConnectionRouteCounts {
	// Prefer confirmed direct paths, then TURN, then unknown WebRTC paths.
	priority := map[string]int{"gateway-relay": 1, "unknown": 2, "turn": 3, "direct": 4}
	peers := map[string]string{}
	for _, s := range sessions {
		kind := "unknown"
		if s.Route != nil && priority[s.Route.Kind] > 0 {
			kind = s.Route.Kind
		}
		if priority[kind] > priority[peers[s.Peer]] {
			peers[s.Peer] = kind
		}
	}
	var counts model.ConnectionRouteCounts
	for _, kind := range peers {
		switch kind {
		case "direct":
			counts.Direct++
		case "turn":
			counts.TURN++
		case "gateway-relay":
			counts.Relay++
		default:
			counts.Unknown++
		}
	}
	return counts
}

func (s *peerSession) route() *model.PeerRoute {
	if s.mode == "relay" {
		return &model.PeerRoute{Kind: "gateway-relay"}
	}
	if s.link == nil || s.link.pc == nil || s.link.pc.SCTP() == nil {
		return &model.PeerRoute{Kind: "unknown"}
	}
	pair, err := s.link.pc.SCTP().Transport().ICETransport().GetSelectedCandidatePair()
	if err != nil || pair == nil {
		return &model.PeerRoute{Kind: "unknown"}
	}
	return candidateRoute(pair)
}

func candidateRoute(pair *webrtc.ICECandidatePair) *model.PeerRoute {
	if pair == nil || pair.Local == nil || pair.Remote == nil {
		return &model.PeerRoute{Kind: "unknown"}
	}
	kind := "direct"
	if pair.Local.Typ == webrtc.ICECandidateTypeRelay || pair.Remote.Typ == webrtc.ICECandidateTypeRelay {
		kind = "turn"
	}
	return &model.PeerRoute{Kind: kind, LocalCandidate: pair.Local.Typ.String(), RemoteCandidate: pair.Remote.Typ.String(), Protocol: pair.Local.Protocol.String()}
}
