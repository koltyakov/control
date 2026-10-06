package model

import "testing"

func TestActiveTunnelCounts(t *testing.T) {
	active := []Activity{
		{Kind: "tunnel", Operation: "tcp.open"},
		{Kind: "tunnel", Operation: "tcp.accept"},
		{Kind: "tunnel", Operation: "tcp.listen"},
		{Kind: "task", Operation: "tcp.listen"},
		{Kind: "tunnel", Operation: "other"},
	}
	if got := ActiveTunnelCounts(active); *got != (TunnelCounts{Forward: 2, Reverse: 1}) {
		t.Fatalf("live counts = %+v", got)
	}
	if got := ActiveTunnelCounts(nil); *got != (TunnelCounts{}) {
		t.Fatalf("empty activity = %+v", got)
	}
}
