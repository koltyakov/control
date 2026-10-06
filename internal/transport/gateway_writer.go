package transport

import (
	"context"
	"errors"
	"time"

	"github.com/coder/websocket"
)

type gatewayWrite struct {
	ctx  context.Context
	ws   *websocket.Conn
	data []byte
	done chan error
}

// An application stream must not own a shared WebSocket write context: coder's
// WebSocket closes the socket when an in-progress write is cancelled. Serialize
// bounded writes under the peer lifetime instead, retaining each original socket
// so a reconnect cannot replay a queued packet onto a different connection.
func (p *Peer) writeGateway() {
	defer p.drainWrites()
	for {
		select {
		case <-p.ctx.Done():
			return
		case request := <-p.writes:
			if err := request.ctx.Err(); err != nil {
				request.done <- err
				continue
			}
			ctx, cancel := context.WithTimeout(p.ctx, 10*time.Second)
			err := request.ws.Write(ctx, websocket.MessageBinary, request.data)
			cancel()
			request.done <- err
		}
	}
}

func (p *Peer) drainWrites() {
	for {
		select {
		case request := <-p.writes:
			request.done <- errors.New("gateway writer stopped")
		default:
			return
		}
	}
}
