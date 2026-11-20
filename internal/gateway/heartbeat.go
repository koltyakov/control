package gateway

import (
	"context"
	"time"

	"github.com/coder/websocket"
	"github.com/koltyakov/control/internal/identity"
	"github.com/koltyakov/control/internal/protocol"
	"github.com/koltyakov/control/internal/update"
	"google.golang.org/protobuf/proto"
)

const (
	gatewayHeartbeatInterval = 5 * time.Second
	gatewayHeartbeatTimeout  = 10 * time.Second
	// gatewayProbeTimeout bounds an early check requested by a reconnecting
	// identity. A healthy connection answers well within it.
	gatewayProbeTimeout = 3 * time.Second
)

// WebSocket proxies can answer transport-level pings without reaching a node.
// Application challenges must reach its packet reader. Waiting for their echo
// must not pause relay traffic on this socket.
func (c *connection) writePackets(ctx context.Context) {
	interval := 20 * time.Second
	if c.heartbeat {
		interval = gatewayHeartbeatInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	timer := time.NewTimer(gatewayHeartbeatTimeout)
	timer.Stop()
	defer timer.Stop()
	var deadline <-chan time.Time
	var pending string
	var timerDeadline time.Time
	priorityBurst := 0
	var relayReady <-chan struct{}
	if c.relay != nil {
		relayReady = c.relay.ready
	}
	for {
		if ctx.Err() != nil {
			return
		}
		// Liveness must not starve behind a continuous signaling backlog.
		if pending != "" && !timerDeadline.IsZero() && !time.Now().Before(timerDeadline) {
			return
		}
		// Ready signaling always writes before further bulk relay data.
		if priorityBurst < 8 {
			select {
			case packet := <-c.out:
				if err := c.writeQueuedPacket(ctx, packet); err != nil {
					return
				}
				priorityBurst++
				continue
			default:
			}
		}
		priorityBurst = 0
		select {
		case <-ctx.Done():
			return
		case <-deadline:
			return
		case pong := <-c.pong:
			if pending != "" && pong == pending {
				timer.Stop()
				deadline, pending = nil, ""
			}
		case packet := <-c.out:
			if err := c.writeQueuedPacket(ctx, packet); err != nil {
				return
			}
		case packet := <-c.outData:
			if err := c.writeQueuedPacket(ctx, packet); err != nil {
				return
			}
		case <-relayReady:
			packet := c.relay.pop()
			if packet == nil {
				continue
			}
			if err := c.writeQueuedPacket(ctx, packet); err != nil {
				return
			}
		case <-ticker.C:
			if pending != "" {
				continue
			}
			if !c.checkLiveness(ctx, &pending, gatewayHeartbeatTimeout) {
				return
			}
			if pending != "" {
				timer.Reset(gatewayHeartbeatTimeout)
				timerDeadline = time.Now().Add(gatewayHeartbeatTimeout)
				deadline = timer.C
			}
		case <-c.probe:
			if pending == "" {
				if !c.checkLiveness(ctx, &pending, gatewayProbeTimeout) {
					return
				}
			} else if time.Until(timerDeadline) <= gatewayProbeTimeout {
				continue
			}
			// An outstanding challenge also proves liveness; only shorten its wait.
			if pending != "" {
				timer.Reset(gatewayProbeTimeout)
				timerDeadline = time.Now().Add(gatewayProbeTimeout)
				deadline = timer.C
			}
		}
	}
}

// checkLiveness sends an application challenge and records it in pending; the
// read loop delivers its echo through pong. Older nodes without application
// heartbeats get a synchronous WebSocket ping and leave pending empty. It
// returns false when the write or ping fails.
func (c *connection) checkLiveness(ctx context.Context, pending *string, timeout time.Duration) bool {
	if !c.heartbeat {
		ping, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		return c.ws.Ping(ping) == nil
	}
	nonce := identity.NewID()
	*pending = nonce
	c.challenge.Store(&nonce)
	return c.writePacket(ctx, &protocol.Packet{Kind: "gateway.ping", From: update.GatewaySender, Session: nonce}) == nil
}

// maxRetainedWriteBytes matches wsio's ordinary relay packet storage.
const maxRetainedWriteBytes = 128 << 10

// writePacket reuses ordinary marshal storage. Write completes synchronously,
// so oversized storage is released here instead of waiting for the next packet.
func (c *connection) writePacket(ctx context.Context, packet *protocol.Packet) error {
	data, err := proto.MarshalOptions{}.MarshalAppend(c.writeBuffer[:0], packet)
	if err != nil {
		return err
	}
	write, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	err = c.ws.Write(write, websocket.MessageBinary, data)
	if cap(data) <= maxRetainedWriteBytes {
		c.writeBuffer = data
	} else {
		c.writeBuffer = nil
	}
	return err
}
