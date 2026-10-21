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

const gatewayHeartbeatInterval = 5 * time.Second

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
	timer := time.NewTimer(10 * time.Second)
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
				if err := c.writePacket(ctx, packet); err != nil {
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
			if err := c.writePacket(ctx, packet); err != nil {
				return
			}
		case packet := <-c.outData:
			if packet.Kind == "data" {
				c.queued.Add(-int64(len(packet.Data)))
			}
			if err := c.writePacket(ctx, packet); err != nil {
				return
			}
		case <-relayReady:
			packet := c.relay.pop()
			if packet == nil {
				continue
			}
			if packet.Kind == "data" {
				c.queued.Add(-int64(len(packet.Data)))
			}
			if err := c.writePacket(ctx, packet); err != nil {
				return
			}
		case <-ticker.C:
			if pending != "" {
				continue
			}
			if c.heartbeat {
				pending = identity.NewID()
				if err := c.writePacket(ctx, &protocol.Packet{Kind: "gateway.ping", From: update.GatewaySender, Session: pending}); err != nil {
					return
				}
				timer.Reset(10 * time.Second)
				timerDeadline = time.Now().Add(10 * time.Second)
				deadline = timer.C
			} else {
				ping, cancel := context.WithTimeout(ctx, 10*time.Second)
				err := c.ws.Ping(ping)
				cancel()
				if err != nil {
					return
				}
			}
		}
	}
}

func (c *connection) writePacket(ctx context.Context, packet *protocol.Packet) error {
	if cap(c.writeBuffer) > 128<<10 {
		c.writeBuffer = nil
	}
	data, err := proto.MarshalOptions{}.MarshalAppend(c.writeBuffer[:0], packet)
	if err != nil {
		return err
	}
	c.writeBuffer = data
	write, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return c.ws.Write(write, websocket.MessageBinary, data)
}
