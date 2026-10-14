package transport

import (
	"slices"
	"testing"

	"github.com/koltyakov/control/internal/model"
)

func TestConnectionRoutesCountPeersAcrossLanes(t *testing.T) {
	session := func(peer, kind string) model.PeerSession {
		return model.PeerSession{Peer: peer, Route: &model.PeerRoute{Kind: kind}}
	}
	sessions := []model.PeerSession{
		session("direct", "gateway-relay"), session("direct", "turn"), session("direct", "direct"),
		session("turn", "gateway-relay"), session("turn", "unknown"), session("turn", "turn"), session("turn", "turn"),
		session("relay", "gateway-relay"), session("relay", "gateway-relay"),
		session("unknown", "gateway-relay"), session("unknown", "unknown"),
		{Peer: "missing"}, session("unexpected", "future-route"),
	}
	want := model.ConnectionRouteCounts{Direct: 1, TURN: 1, Relay: 1, Unknown: 3}
	for range 2 {
		if got := connectionRouteCounts(sessions); got != want {
			t.Fatalf("connection routes = %+v, want %+v", got, want)
		}
		slices.Reverse(sessions)
	}
	if got := connectionRouteCounts(nil); got != (model.ConnectionRouteCounts{}) {
		t.Fatalf("empty sessions reported connections: %+v", got)
	}
}
