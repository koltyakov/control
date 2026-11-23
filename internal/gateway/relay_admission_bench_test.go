package gateway

import (
	"testing"

	"github.com/koltyakov/control/internal/protocol"
)

// BenchmarkAdmitRelayData measures per-packet relay admission for an
// established, answered carrier: the steady state of a bulk transfer.
func BenchmarkAdmitRelayData(b *testing.B) {
	g := &Gateway{}
	source, dest := &connection{}, &connection{}
	open := &protocol.Packet{Kind: "open", To: "dest", Session: "session"}
	if !g.admitRelay("source", source, dest, open) {
		b.Fatal("open rejected")
	}
	answer := &protocol.Packet{Kind: "answer", To: "source", Session: "session"}
	if !g.admitRelay("dest", dest, source, answer) {
		b.Fatal("answer rejected")
	}
	data := &protocol.Packet{Kind: "data", To: "dest", Session: "session", Data: make([]byte, 16<<10)}
	b.ReportAllocs()
	for b.Loop() {
		if !g.admitRelay("source", source, dest, data) {
			b.Fatal("data rejected")
		}
	}
}
