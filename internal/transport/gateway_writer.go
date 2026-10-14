package transport

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/coder/websocket"
)

type gatewayWrite struct {
	ctx    context.Context
	ws     *websocket.Conn
	data   []byte
	pooled *[]byte // backing store of data, recycled after the write
	done   chan error
}

// gatewayWriteBuffers recycles marshaled gateway packets, which are
// dominated by 16–64 KiB relay payloads.
var gatewayWriteBuffers = sync.Pool{New: func() any {
	b := make([]byte, 0, 4096)
	return &b
}}

func getWriteBuffer() *[]byte { return gatewayWriteBuffers.Get().(*[]byte) }

func putWriteBuffer(b *[]byte) {
	if b == nil || cap(*b) > 1<<20 {
		return
	}
	*b = (*b)[:0]
	gatewayWriteBuffers.Put(b)
}

// An application stream must not own a shared WebSocket write context: coder's
// WebSocket closes the socket when an in-progress write is cancelled. Serialize
// bounded writes under the peer lifetime instead, retaining each original socket
// so a reconnect cannot replay a queued packet onto a different connection.
func (p *Peer) writeGateway() {
	defer p.drainWrites()
	write := func(request *gatewayWrite) {
		if err := request.ctx.Err(); err != nil {
			putWriteBuffer(request.pooled)
			request.done <- err
			return
		}
		ctx, cancel := context.WithTimeout(p.ctx, 10*time.Second)
		err := request.ws.Write(ctx, websocket.MessageBinary, request.data)
		cancel()
		putWriteBuffer(request.pooled)
		request.done <- err
	}
	priorityBurst := 0
	for {
		if p.ctx.Err() != nil {
			return
		}
		// Signaling and pongs precede bulk data. Close stays in the data queue
		// so it cannot discard earlier bytes from that session.
		if priorityBurst < 8 {
			select {
			case request := <-p.priorityWrites:
				write(request)
				priorityBurst++
				continue
			default:
			}
		}
		priorityBurst = 0
		select {
		case <-p.ctx.Done():
			return
		case request := <-p.priorityWrites:
			write(request)
		case request := <-p.writes:
			write(request)
		}
	}
}

func (p *Peer) drainWrites() {
	for {
		select {
		case request := <-p.priorityWrites:
			putWriteBuffer(request.pooled)
			request.done <- errors.New("gateway writer stopped")
		case request := <-p.writes:
			putWriteBuffer(request.pooled)
			request.done <- errors.New("gateway writer stopped")
		default:
			return
		}
	}
}
