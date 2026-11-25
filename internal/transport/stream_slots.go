package transport

import (
	"context"
	"errors"
	"net"
	"time"

	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/wire"
)

const (
	interactiveStreamsALPN = "control-interactive-streams-v1"
	legacyMuxStreams       = 128
	interactiveMuxStreams  = 448
	outgoingInteractive    = 512
	incomingInteractive    = 512
	interactiveWaiters     = 32
	tcpStreamSlotWait      = 500 * time.Millisecond
	streamRejectTimeout    = 100 * time.Millisecond
	streamCapacityCode     = "stream_capacity"
)

// ErrStreamCapacity is returned before an application operation starts.
var ErrStreamCapacity = errors.New("peer stream capacity exhausted")

// IsStreamCapacity recognizes local admission timeouts and upgraded peers'
// explicit admission refusals, but not unrelated dial or network timeouts.
func IsStreamCapacity(err error) bool {
	var rpc *model.RPCError
	return errors.Is(err, ErrStreamCapacity) || (errors.As(err, &rpc) && rpc.Code == streamCapacityCode)
}

type tcpSlotWaitKey struct{}

func reserveStreamSlots(ctx, peerCtx context.Context, session, global chan struct{}) (func(), error) {
	select {
	case session <- struct{}{}:
	case <-ctx.Done():
		return nil, context.Cause(ctx)
	case <-peerCtx.Done():
		return nil, peerCtx.Err()
	}
	select {
	case global <- struct{}{}:
		if ctx.Err() == nil && peerCtx.Err() == nil {
			return func() { <-global; <-session }, nil
		}
		<-global
	case <-ctx.Done():
	case <-peerCtx.Done():
	}
	<-session
	if ctx.Err() != nil {
		return nil, context.Cause(ctx)
	}
	return nil, peerCtx.Err()
}

func tryIncomingSlots(session, global chan struct{}) (func(), bool) {
	select {
	case session <- struct{}{}:
	default:
		return nil, false
	}
	select {
	case global <- struct{}{}:
		return func() { <-global; <-session }, true
	default:
		<-session
		return nil, false
	}
}

// Only negotiated interactive sessions may receive a framed admission error.
// Queueing and the reply are bounded even when a peer stops reading.
func (p *Peer) queueInteractiveStream(s *peerSession, stream net.Conn, slots chan struct{}) {
	select {
	case p.interactiveWaiters <- struct{}{}:
	default:
		// Backpressure this session's accept loop rather than spawning
		// unbounded goroutines to send capacity replies.
		rejectInteractiveStream(stream)
		return
	}
	if !p.worker(func() {
		ctx, cancel := context.WithTimeoutCause(p.ctx, tcpStreamSlotWait, ErrStreamCapacity)
		defer cancel()
		defer func() { <-p.interactiveWaiters }()
		release, err := reserveStreamSlots(ctx, p.ctx, s.incoming, slots)
		if err == nil {
			p.dispatchStream(s, stream, release)
			return
		}
		if IsStreamCapacity(err) && p.ctx.Err() == nil {
			rejectInteractiveStream(stream)
		} else {
			abortStream(stream)
		}
	}) {
		<-p.interactiveWaiters
		abortStream(stream)
	}
}

func rejectInteractiveStream(stream net.Conn) {
	_ = stream.SetWriteDeadline(time.Now().Add(streamRejectTimeout))
	_ = wire.WriteFrame(stream, model.Response{Error: ErrStreamCapacity.Error(), Code: streamCapacityCode})
	abortStream(stream)
}
