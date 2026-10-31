package gateway

import (
	"sync"

	"github.com/koltyakov/control/internal/protocol"
)

type relayItem struct {
	packet   *protocol.Packet
	sequence uint64
}
type relaySession struct {
	items []relayItem
	bytes int64
}

// Round-robin sessions preserve each stream's order and reserve destination
// capacity for other streams. Offline notifications are global FIFO barriers.
type relayScheduler struct {
	mu       sync.Mutex
	ready    chan struct{}
	sessions map[string]*relaySession
	order    []string
	barriers []relayItem
	sequence uint64
	count    int
	bytes    int64
}

func newRelayScheduler() *relayScheduler {
	return &relayScheduler{ready: make(chan struct{}, 1), sessions: make(map[string]*relaySession)}
}

func (q *relayScheduler) signal() {
	select {
	case q.ready <- struct{}{}:
	default:
	}
}

func (q *relayScheduler) push(p *protocol.Packet) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	size := packetRetainedBytes(p)
	if q.count >= relayQueuePackets || q.bytes+size > relayQueueBytes {
		return false
	}
	key := p.From + "\x00" + p.Session
	s := q.sessions[key]
	// A bulk stream can legally have 4 MiB in flight, plus TLS framing.
	// Leave headroom above that window and reserve 2 MiB for other sessions.
	if s != nil && (len(s.items) >= 768 || s.bytes+size > 6<<20) {
		return false
	}
	q.sequence++
	item := relayItem{p, q.sequence}
	if p.Kind == "offline" {
		q.barriers = append(q.barriers, item)
	} else {
		if s == nil {
			s = &relaySession{}
			q.sessions[key] = s
			q.order = append(q.order, key)
		}
		s.items = append(s.items, item)
		s.bytes += size
	}
	q.bytes += size
	q.count++
	q.signal()
	return true
}

func (q *relayScheduler) pop() *protocol.Packet {
	q.mu.Lock()
	defer q.mu.Unlock()
	var p *protocol.Packet
	for i, key := range q.order {
		s := q.sessions[key]
		if len(q.barriers) > 0 && s.items[0].sequence > q.barriers[0].sequence {
			continue
		}
		p = s.items[0].packet
		s.items[0] = relayItem{}
		s.items = s.items[1:]
		s.bytes -= packetRetainedBytes(p)
		q.order = append(q.order[:i], q.order[i+1:]...)
		if len(s.items) == 0 {
			delete(q.sessions, key)
		} else {
			q.order = append(q.order, key)
		}
		break
	}
	if p == nil && len(q.barriers) > 0 {
		p = q.barriers[0].packet
		q.barriers[0] = relayItem{}
		q.barriers = q.barriers[1:]
	}
	if p == nil {
		return nil
	}
	q.count--
	q.bytes -= packetRetainedBytes(p)
	if q.count > 0 {
		q.signal()
	}
	return p
}
