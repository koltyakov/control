package transport

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
)

type gatewayWrite struct {
	ctx    context.Context
	ws     *websocket.Conn
	data   []byte
	pooled *[]byte // backing store of data, recycled after the write
	done   chan error
	// timeout bounds the sender's wait; abandoned tells the writer the sender
	// stopped waiting, so the packet must not be written.
	timeout   *time.Timer
	abandoned atomic.Bool
}

// gatewaySendTimeout bounds queueing plus writing one gateway packet.
const gatewaySendTimeout = 10 * time.Second

var gatewayWrites = sync.Pool{New: func() any {
	timer := time.NewTimer(time.Hour)
	timer.Stop()
	return &gatewayWrite{done: make(chan error, 1), timeout: timer}
}}

func getGatewayWrite(ctx context.Context, ws *websocket.Conn, data []byte, pooled *[]byte) *gatewayWrite {
	request := gatewayWrites.Get().(*gatewayWrite)
	request.ctx, request.ws, request.data, request.pooled = ctx, ws, data, pooled
	request.abandoned.Store(false)
	request.timeout.Reset(gatewaySendTimeout)
	return request
}

// putGatewayWrite recycles a request the writer no longer references: one
// never queued, or one whose result the sender received.
func putGatewayWrite(request *gatewayWrite) {
	request.timeout.Stop()
	request.ctx, request.ws, request.data, request.pooled = nil, nil, nil, nil
	gatewayWrites.Put(request)
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
	// One write context serves every write; a stall timer cancels it, which
	// closes that socket as a per-write timeout would. Only then is it rebuilt.
	var ctx context.Context
	var cancel context.CancelFunc
	var stall *time.Timer
	defer func() {
		if cancel != nil {
			stall.Stop()
			cancel()
		}
	}()
	write := func(request *gatewayWrite) {
		if request.abandoned.Load() {
			putWriteBuffer(request.pooled)
			request.done <- context.Canceled
			return
		}
		if err := request.ctx.Err(); err != nil {
			putWriteBuffer(request.pooled)
			request.done <- err
			return
		}
		if ctx == nil || ctx.Err() != nil {
			ctx, cancel = context.WithCancel(p.ctx)
			stall = time.AfterFunc(gatewaySendTimeout, cancel)
		} else {
			stall.Reset(gatewaySendTimeout)
		}
		err := request.ws.Write(ctx, websocket.MessageBinary, request.data)
		stall.Stop()
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
