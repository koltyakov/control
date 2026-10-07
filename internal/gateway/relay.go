package gateway

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/koltyakov/control/internal/protocol"
)

const (
	// relayQueuePackets bounds queued packets per destination connection.
	relayQueuePackets = 1024
	// relayQueueBytes bounds queued relay payload per destination connection.
	relayQueueBytes = 8 << 20
)

// counters are cumulative gateway diagnostics, exposed only to the superuser.
type counters struct {
	connects        atomic.Int64
	disconnects     atomic.Int64
	authFailures    atomic.Int64
	relayPackets    atomic.Int64
	relayBytes      atomic.Int64
	relayOverflows  atomic.Int64
	relayDropped    atomic.Int64
	lastOverflowLog atomic.Int64
}

// GatewayMetrics is the superuser diagnostic snapshot.
type GatewayMetrics struct {
	ConnectedMachines int   `json:"connectedMachines"`
	ConnectedClients  int   `json:"connectedClients"`
	QueuedRelayBytes  int64 `json:"queuedRelayBytes"`
	Connects          int64 `json:"connects"`
	Disconnects       int64 `json:"disconnects"`
	AuthFailures      int64 `json:"authFailures"`
	RelayPackets      int64 `json:"relayPackets"`
	RelayBytes        int64 `json:"relayBytes"`
	// RelayOverflows counts relay sessions closed because their destination's
	// queue was full. The destination's gateway connection stays open.
	RelayOverflows int64 `json:"relayOverflows"`
	// RelayDropped counts signaling packets dropped for a full destination queue.
	RelayDropped int64 `json:"relayDropped"`
}

func (g *Gateway) metricsHandler(w http.ResponseWriter, _ *http.Request) {
	m := GatewayMetrics{
		Connects: g.counters.connects.Load(), Disconnects: g.counters.disconnects.Load(),
		AuthFailures: g.counters.authFailures.Load(), RelayPackets: g.counters.relayPackets.Load(),
		RelayBytes: g.counters.relayBytes.Load(), RelayOverflows: g.counters.relayOverflows.Load(),
		RelayDropped: g.counters.relayDropped.Load(),
	}
	g.routeMu.RLock()
	for _, c := range g.routes {
		if c.client {
			m.ConnectedClients++
		} else {
			m.ConnectedMachines++
		}
		m.QueuedRelayBytes += c.queued.Load()
	}
	g.routeMu.RUnlock()
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(m)
}

// route returns the connected destination within the sender's fleet. It uses
// a separate read lock so relay forwarding never waits on database commits.
func (g *Gateway) route(to, userID string) *connection {
	g.routeMu.RLock()
	defer g.routeMu.RUnlock()
	if dest := g.routes[to]; dest != nil && dest.userID == userID {
		return dest
	}
	return nil
}

// enqueue offers a packet to a connection's writer without blocking the
// sender's read loop. Relay payload is bounded in bytes as well as packets.
func (c *connection) enqueue(p *protocol.Packet) bool {
	size := int64(len(p.Data))
	if p.Kind == "data" && c.queued.Load()+size > relayQueueBytes {
		return false
	}
	select {
	case c.out <- p:
		if p.Kind == "data" {
			c.queued.Add(size)
		}
		return true
	default:
		return false
	}
}

// relayOverflow closes one relay session whose destination cannot keep up.
// Dropping a data packet breaks that session's byte stream, so the sender is
// told to close it; the destination's gateway connection and its other
// sessions stay open. Logging is limited so a burst cannot flood the log.
func (g *Gateway) relayOverflow(source *connection, p *protocol.Packet) {
	g.counters.relayOverflows.Add(1)
	_ = source.enqueue(&protocol.Packet{Kind: "close", From: p.To, Session: p.Session, Data: []byte("relay queue full")})
	now := time.Now().UnixNano()
	last := g.counters.lastOverflowLog.Load()
	if now-last > int64(10*time.Second) && g.counters.lastOverflowLog.CompareAndSwap(last, now) {
		slog.Warn("relay destination queue full; closing relay session", "from", p.From, "to", p.To)
	}
}
