package gateway

import (
	"sync"

	"github.com/koltyakov/control/internal/protocol"
)

const (
	// A bulk stream can legally have 4 MiB in flight, plus TLS framing.
	// Leave headroom above that window and reserve 2 MiB for other sessions.
	relaySessionPackets = 768
	relaySessionBytes   = 6 << 20
	// One drained session is kept for alternating push/pop traffic, but only
	// with small ring storage. Larger rings are released with their session.
	relaySpareItems = 8
	// Data cannot use the last packets and bytes of a destination queue, so a
	// close still reaches a destination whose queue is full of bulk data.
	relayControlPackets = 64
	relayControlBytes   = relayQueueBytes / 16
)

type relayItem struct {
	packet   *protocol.Packet
	sequence uint64
}

// relayStreamKey identifies a stream without allocating a concatenated key.
type relayStreamKey struct{ from, session string }

// relaySession is a FIFO ring. Vacated slots are cleared so the scheduler never
// retains a packet after it has been popped.
type relaySession struct {
	key   relayStreamKey
	items []relayItem
	head  int
	n     int
	bytes int64
}

func (s *relaySession) front() relayItem { return s.items[s.head] }

func (s *relaySession) pushBack(item relayItem) {
	if s.n == len(s.items) {
		// Growth stops at the per-session packet bound enforced by push, plus
		// the slot reserved for a closing packet.
		items := make([]relayItem, min(max(2*len(s.items), 4), relaySessionPackets+1))
		for i := range s.n {
			items[i] = s.items[(s.head+i)%len(s.items)]
		}
		s.items, s.head = items, 0
	}
	s.items[(s.head+s.n)%len(s.items)] = item
	s.n++
}

func (s *relaySession) popFront() *protocol.Packet {
	p := s.items[s.head].packet
	s.items[s.head] = relayItem{}
	s.head = (s.head + 1) % len(s.items)
	s.n--
	return p
}

// Round-robin sessions preserve each stream's order and reserve destination
// capacity for other streams. Offline notifications are global FIFO barriers.
type relayScheduler struct {
	mu       sync.Mutex
	ready    chan struct{}
	sessions map[relayStreamKey]*relaySession
	order    []*relaySession
	spare    *relaySession
	barriers []relayItem
	sequence uint64
	count    int
	bytes    int64
}

func newRelayScheduler() *relayScheduler {
	return &relayScheduler{ready: make(chan struct{}, 1), sessions: make(map[relayStreamKey]*relaySession)}
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
	packets, bytes := relayQueuePackets, int64(relayQueueBytes)
	if p.Kind == "data" {
		packets, bytes = packets-relayControlPackets, bytes-relayControlBytes
	}
	if q.count >= packets || q.bytes+size > bytes {
		return false
	}
	key := relayStreamKey{p.From, p.Session}
	s := q.sessions[key]
	// A close ends its session and the gateway has already recorded it, so it
	// may use one slot beyond the per-session bound that its data filled.
	if s != nil && (p.Kind == "close" && s.n > relaySessionPackets || p.Kind != "close" && (s.n >= relaySessionPackets || s.bytes+size > relaySessionBytes)) {
		return false
	}
	q.sequence++
	item := relayItem{p, q.sequence}
	if p.Kind == "offline" {
		q.barriers = append(q.barriers, item)
	} else {
		if s == nil {
			if s, q.spare = q.spare, nil; s == nil {
				s = &relaySession{}
			}
			s.key = key
			q.sessions[key] = s
			q.order = append(q.order, s)
		}
		s.pushBack(item)
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
	for i, s := range q.order {
		if len(q.barriers) > 0 && s.front().sequence > q.barriers[0].sequence {
			continue
		}
		p = s.popFront()
		s.bytes -= packetRetainedBytes(p)
		last := len(q.order) - 1
		copy(q.order[i:], q.order[i+1:])
		q.order[last] = nil
		q.order = q.order[:last]
		if s.n == 0 {
			q.retire(s)
		} else {
			q.order = append(q.order, s)
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

// retire removes a drained session. Its ring holds only cleared slots.
func (q *relayScheduler) retire(s *relaySession) {
	delete(q.sessions, s.key)
	s.key, s.head, s.bytes = relayStreamKey{}, 0, 0
	if q.spare == nil && len(s.items) <= relaySpareItems {
		q.spare = s
	}
}
