package gateway

import (
	"github.com/koltyakov/control/internal/protocol"
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
