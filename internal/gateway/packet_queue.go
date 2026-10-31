package gateway

import (
	"context"
	"sync"

	"github.com/koltyakov/control/internal/protocol"
)

// packetQueues owns reservations from enqueue through write completion. Closing
// excludes senders holding an old routing pointer, then drains all retained data.
type packetQueues struct {
	mu              sync.Mutex
	closed          bool
	local           packetBudget
	global, account *packetBudget
	rejected        *admission
}

func packetRetainedBytes(p *protocol.Packet) int64 {
	// Include spare payload capacity, unknown protobuf fields and the packet /
	// scheduler bookkeeping. Socket buffers and fixed channel slots are separate.
	return 128 + int64(cap(p.Data)) + int64(cap(p.ProtoReflect().GetUnknown())) +
		int64(len(p.Kind)) + int64(len(p.From)) + int64(len(p.To)) + int64(len(p.Session))
}

func (c *connection) enqueueBounded(p *protocol.Packet) bool {
	q := c.queues
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return false
	}
	size, data := packetRetainedBytes(p), p.Kind == "data"
	if !q.global.reserve(size, data) {
		q.rejected.queueRejected.Add(1)
		return false
	}
	if !q.account.reserve(size, data) {
		q.global.used.Add(-size)
		q.rejected.queueRejected.Add(1)
		return false
	}
	if !q.local.reserve(size, data) {
		q.account.used.Add(-size)
		q.global.used.Add(-size)
		q.rejected.queueRejected.Add(1)
		return false
	}
	if data {
		c.queued.Add(int64(len(p.Data)))
	}
	accepted := false
	if p.Kind == "data" || p.Kind == "close" || p.Kind == "offline" {
		accepted = c.relay.push(p)
	} else {
		select {
		case c.out <- p:
			accepted = true
		default:
		}
	}
	if !accepted {
		c.releasePacket(p)
		q.rejected.queueRejected.Add(1)
	}
	return accepted
}

func (c *connection) releasePacket(p *protocol.Packet) {
	if p.Kind == "data" {
		c.queued.Add(-int64(len(p.Data)))
	}
	if q := c.queues; q != nil {
		size := packetRetainedBytes(p)
		q.local.used.Add(-size)
		q.account.used.Add(-size)
		q.global.used.Add(-size)
	}
}

func (c *connection) closeQueues() {
	q := c.queues
	if q == nil {
		return
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return
	}
	q.closed = true
	for {
		select {
		case p := <-c.out:
			c.releasePacket(p)
		default:
			goto relay
		}
	}
relay:
	for p := c.relay.pop(); p != nil; p = c.relay.pop() {
		c.releasePacket(p)
	}
}

func (c *connection) writeQueuedPacket(ctx context.Context, p *protocol.Packet) error {
	defer c.releasePacket(p)
	err := c.writePacket(ctx, p)
	if err != nil && c.queues != nil {
		c.queues.rejected.writeFailures.Add(1)
	}
	return err
}
