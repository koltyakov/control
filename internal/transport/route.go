package transport

import (
	"github.com/koltyakov/control/internal/model"
	"github.com/pion/webrtc/v4"
)

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
