package gateway

import (
	"fmt"
	"github.com/koltyakov/control/internal/protocol"
	"strings"
	"testing"
)

func TestRelaySessionFairnessAndOrdering(t *testing.T) {
	q := newRelayScheduler()
	for i := range 32 {
		if !q.push(&protocol.Packet{Kind: "data", From: "bulk", Session: "a", Data: []byte{byte(i)}}) {
			t.Fatal("enqueue")
		}
	}
	q.push(&protocol.Packet{Kind: "data", From: "small", Session: "b", Data: []byte("small")})
	q.push(&protocol.Packet{Kind: "close", From: "small", Session: "b"})
	q.push(&protocol.Packet{Kind: "offline", From: "bulk"})
	q.push(&protocol.Packet{Kind: "data", From: "new", Session: "c", Data: []byte("after")})
	if p := q.pop(); p.From != "bulk" || p.Data[0] != 0 {
		t.Fatal("first", p)
	}
	if p := q.pop(); p.From != "small" || p.Kind != "data" {
		t.Fatal("small stream starved", p)
	}
	next := byte(1)
	offline, closed := false, false
	for p := q.pop(); p != nil; p = q.pop() {
		switch p.Kind {
		case "offline":
			if next != 32 {
				t.Fatal("offline overtook data")
			}
			offline = true
		case "close":
			closed = true
		case "data":
			if p.From == "bulk" {
				if p.Data[0] != next {
					t.Fatal("data reordered")
				}
				next++
			} else if p.From == "new" && !offline {
				t.Fatal("data crossed barrier")
			}
		}
	}
	if !offline || !closed || q.bytes != 0 || q.count != 0 {
		t.Fatal("queue not drained")
	}
}

func TestRelayQueueAccommodatesBulkWindow(t *testing.T) {
	q := newRelayScheduler()
	// Include TLS framing beyond the 4 MiB application receive window.
	for range 257 {
		if !q.push(&protocol.Packet{Kind: "data", From: "bulk", Session: "a", Data: make([]byte, 16406)}) {
			t.Fatal("legal bulk window overflowed")
		}
	}
	if !q.push(&protocol.Packet{Kind: "data", From: "other", Session: "b", Data: make([]byte, 64<<10)}) {
		t.Fatal("bulk window consumed all destination capacity")
	}
}

func benchmarkRelayPacket(session string) *protocol.Packet {
	return &protocol.Packet{Kind: "data", From: strings.Repeat("f", 64), Session: session + strings.Repeat("s", 32-len(session)), Data: make([]byte, 64<<10)}
}

// Alternating push/pop drains the only session every iteration.
func BenchmarkRelaySchedulerEmptyPushPop(b *testing.B) {
	q, p := newRelayScheduler(), benchmarkRelayPacket("a")
	b.ReportAllocs()
	for b.Loop() {
		if !q.push(p) || q.pop() != p {
			b.Fatal("push/pop")
		}
	}
}

// A backlog keeps sessions live while packets churn through them.
func BenchmarkRelaySchedulerQueuedPushPop(b *testing.B) {
	for _, sessions := range []int{1, 16} {
		b.Run(fmt.Sprintf("sessions=%d", sessions), func(b *testing.B) {
			q := newRelayScheduler()
			packets := make([]*protocol.Packet, sessions)
			for i := range packets {
				packets[i] = benchmarkRelayPacket(fmt.Sprint(i))
			}
			for i := range 64 {
				if !q.push(packets[i%sessions]) {
					b.Fatal("prefill")
				}
			}
			b.ReportAllocs()
			i := 0
			for b.Loop() {
				if !q.push(packets[i%sessions]) || q.pop() == nil {
					b.Fatal("push/pop")
				}
				i++
			}
		})
	}
}

func assertRelaySchedulerReleased(t *testing.T, q *relayScheduler) {
	t.Helper()
	if q.count != 0 || q.bytes != 0 || len(q.sessions) != 0 || len(q.order) != 0 || len(q.barriers) != 0 {
		t.Fatalf("queue not drained: count=%d bytes=%d sessions=%d order=%d", q.count, q.bytes, len(q.sessions), len(q.order))
	}
	for _, s := range q.order[:cap(q.order)] {
		if s != nil {
			t.Fatal("drained session still referenced by order storage")
		}
	}
	if s := q.spare; s != nil {
		if len(s.items) > relaySpareItems || s.n != 0 || s.bytes != 0 || s.key != (relayStreamKey{}) {
			t.Fatalf("spare session retained state: items=%d n=%d", len(s.items), s.n)
		}
		for _, item := range s.items {
			if item.packet != nil {
				t.Fatal("spare session retained a popped packet")
			}
		}
	}
}

func TestRelaySessionRingOrderingAndRetention(t *testing.T) {
	q := newRelayScheduler()
	next, want := 0, 0
	push := func(session string) {
		t.Helper()
		if !q.push(&protocol.Packet{Kind: "data", From: "n", Session: session, Data: []byte{byte(next)}}) {
			t.Fatal("enqueue")
		}
		next++
	}
	// Interleave so the ring wraps repeatedly while growing.
	for range 40 {
		push("a")
		push("a")
		push("a")
		if p := q.pop(); p == nil || int(p.Data[0]) != want {
			t.Fatal("ring reordered packets", p, want)
		}
		want++
	}
	for p := q.pop(); p != nil; p = q.pop() {
		if int(p.Data[0]) != want {
			t.Fatal("ring reordered packets", p.Data[0], want)
		}
		want++
	}
	if want != next {
		t.Fatal("packets lost", want, next)
	}
	// A large drained ring is released rather than kept for reuse.
	assertRelaySchedulerReleased(t, q)
	if q.spare != nil {
		t.Fatal("large ring retained after drain")
	}
	// Small rings are reused for alternating traffic without leaking the key.
	push("b")
	q.pop()
	spare := q.spare
	if spare == nil {
		t.Fatal("small ring not reused")
	}
	push("c")
	push("c")
	if q.sessions[relayStreamKey{"n", "c"}] != spare || len(q.sessions) != 1 {
		t.Fatal("spare session not rekeyed")
	}
	if !q.push(&protocol.Packet{Kind: "offline", From: "n"}) {
		t.Fatal("enqueue offline")
	}
	push("d")
	if p := q.pop(); p.Session != "c" {
		t.Fatal("first", p)
	}
	if p := q.pop(); p.Session != "c" {
		t.Fatal("data crossed earlier offline barrier", p)
	}
	if p := q.pop(); p.Kind != "offline" {
		t.Fatal("offline barrier", p)
	}
	if p := q.pop(); p.Session != "d" {
		t.Fatal("after barrier", p)
	}
	assertRelaySchedulerReleased(t, q)
}

func TestRelaySessionBoundsAreExact(t *testing.T) {
	q := newRelayScheduler()
	for range relaySessionPackets {
		if !q.push(&protocol.Packet{Kind: "data", From: "n", Session: "a"}) {
			t.Fatal("legal session packet rejected")
		}
	}
	if q.push(&protocol.Packet{Kind: "data", From: "n", Session: "a"}) {
		t.Fatal("session packet bound exceeded")
	}
	if len(q.sessions[relayStreamKey{"n", "a"}].items) != relaySessionPackets+1 {
		t.Fatal("session ring grew beyond its packet bound and close slot")
	}
	if !q.push(&protocol.Packet{Kind: "data", From: "n", Session: "b"}) {
		t.Fatal("other session rejected")
	}
	for q.pop() != nil {
	}
	assertRelaySchedulerReleased(t, q)

	// Byte bounds use the full retained packet charge.
	p := &protocol.Packet{Kind: "data", From: "n", Session: "a", Data: make([]byte, 64<<10)}
	accepted := int64(0)
	for q.push(p) {
		accepted += packetRetainedBytes(p)
	}
	if accepted > relaySessionBytes || accepted+packetRetainedBytes(p) <= relaySessionBytes || q.bytes != accepted {
		t.Fatal("session byte bound inexact", accepted)
	}
	for q.pop() != nil {
	}
	assertRelaySchedulerReleased(t, q)
}

func TestRelayCloseBypassesFullDataQueues(t *testing.T) {
	q := newRelayScheduler()
	for q.push(&protocol.Packet{Kind: "data", From: "n", Session: "a"}) {
	}
	if !q.push(&protocol.Packet{Kind: "close", From: "n", Session: "a"}) {
		t.Fatal("close rejected by its own full session")
	}
	if q.push(&protocol.Packet{Kind: "close", From: "n", Session: "a"}) {
		t.Fatal("repeated closes grew a full session without bound")
	}
	for i := 0; q.push(&protocol.Packet{Kind: "data", From: "n", Session: fmt.Sprint("s", i)}); i++ {
	}
	if q.count != relayQueuePackets-relayControlPackets {
		t.Fatal("data consumed control headroom", q.count)
	}
	if !q.push(&protocol.Packet{Kind: "close", From: "n", Session: "s0"}) {
		t.Fatal("close rejected by a destination queue full of data")
	}
	for q.pop() != nil {
	}
	assertRelaySchedulerReleased(t, q)
}
