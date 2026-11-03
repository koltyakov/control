package gateway

import (
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/koltyakov/control/internal/identity"
	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/protocol"
	"github.com/koltyakov/control/internal/update"
)

const gatewayDrainWindow = 30 * time.Second

// RelayDrainRequest is a bounded, gateway-authenticated transport instruction.
// It never cancels a stream and expires even if its resume packet is lost.
type RelayDrainRequest struct {
	Until time.Time `json:"until"`
}

type relayKey struct{ first, second, session string }
type managedRelay struct {
	first, second             *connection
	firstClosed, secondClosed bool
	answered                  bool
}

// This mutex is separate from directory persistence. Relay admission and the
// final restart fence share it, without putting SQLite on the forwarding path.
type gatewayMaintenance struct {
	mu             sync.Mutex
	relays         map[relayKey]managedRelay
	counts         map[*connection]int
	drainID        string
	until, retryAt time.Time
	switching      bool
	httpActive     int
}

func (g *Gateway) maintenanceHandler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Protect admitted mutations and executable delivery. Observation and
		// WebSocket registration must not perpetually renew the HTTP blocker.
		protected := r.Method != http.MethodGet && r.Method != http.MethodHead ||
			strings.HasPrefix(r.URL.Path, "/v1/updates/blobs/") || strings.HasPrefix(r.URL.Path, "/install/")
		m := &g.maintenance
		m.mu.Lock()
		if m.switching {
			m.mu.Unlock()
			w.Header().Set("Retry-After", "1")
			http.Error(w, "gateway restarting", http.StatusServiceUnavailable)
			return
		}
		if protected {
			m.httpActive++
		}
		m.mu.Unlock()
		if protected {
			defer func() { m.mu.Lock(); m.httpActive--; m.mu.Unlock() }()
		}
		next.ServeHTTP(w, r)
	})
}

func relayIdentity(from, to, session string) (relayKey, bool) {
	if from < to {
		return relayKey{from, to, session}, true
	}
	return relayKey{to, from, session}, false
}

// Called only after normal fleet-scoped routing. A carrier is retained until
// both authenticated endpoints close it, or one connection actually disappears.
// Data without an open is tracked conservatively for legacy senders too.
func (g *Gateway) admitRelay(from string, source, dest *connection, p *protocol.Packet) bool {
	m := &g.maintenance
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.switching {
		return false
	}
	g.routeMu.RLock()
	current := g.routes == nil || g.routes[from] == source && g.routes[p.To] == dest
	g.routeMu.RUnlock()
	if !current {
		return false
	}
	key, first := relayIdentity(from, p.To, p.Session)
	r, exists := m.relays[key]
	if exists && ((first && (r.first != source || r.second != dest)) || (!first && (r.second != source || r.first != dest))) {
		return false
	}
	opening := p.Kind == "open" && len(p.Data) == 0
	if opening && (exists || time.Now().Before(m.until)) {
		return false
	}
	if (opening || p.Kind == "data") && !exists {
		if time.Now().Before(m.until) || m.counts[source] >= 256 || m.counts[dest] >= 256 {
			return false
		}
		if m.relays == nil {
			m.relays, m.counts = make(map[relayKey]managedRelay), make(map[*connection]int)
		}
		r = managedRelay{first: source, second: dest}
		if !first {
			r.first, r.second = dest, source
		}
		m.counts[source]++
		m.counts[dest]++
		m.relays[key], exists = r, true
	}
	if p.Kind == "close" && exists {
		if first {
			r.firstClosed = true
		} else {
			r.secondClosed = true
		}
		if !r.answered || r.firstClosed && r.secondClosed {
			m.removeRelay(key, r)
		} else {
			m.relays[key] = r
		}
	}
	if (p.Kind == "answer" || p.Kind == "data") && exists {
		r.answered = true
		m.relays[key] = r
	}
	return true
}

func (m *gatewayMaintenance) removeRelay(key relayKey, r managedRelay) {
	delete(m.relays, key)
	for _, c := range []*connection{r.first, r.second} {
		m.counts[c]--
		if m.counts[c] == 0 {
			delete(m.counts, c)
		}
	}
}

func (g *Gateway) forgetRelays(c *connection) {
	m := &g.maintenance
	m.mu.Lock()
	defer m.mu.Unlock()
	for key, r := range m.relays {
		if r.first == c || r.second == c {
			m.removeRelay(key, r)
		}
	}
}

// drainGateway is called on the rollout loop. Timeouts restore admission, not
// permission to sever traffic. Repeat hints cover connections joining mid-drain.
func (g *Gateway) drainGateway(now time.Time) bool {
	m := &g.maintenance
	m.mu.Lock()
	if !m.until.IsZero() && !now.Before(m.until) {
		m.until, m.retryAt = time.Time{}, now.Add(gatewayDrainWindow)
		id := m.drainID
		m.drainID = ""
		m.mu.Unlock()
		g.drainHint("gateway.resume", id, time.Time{})
		return false
	}
	if now.Before(m.retryAt) {
		m.mu.Unlock()
		return false
	}
	if m.until.IsZero() {
		m.drainID, m.until = identity.NewID(), now.Add(gatewayDrainWindow)
	}
	id, until := m.drainID, m.until
	m.mu.Unlock()
	g.drainHint("gateway.drain", id, until)
	return true
}

func (g *Gateway) drainHint(kind, id string, until time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, c := range g.peers {
		if c.relayDrain && (kind == "gateway.resume" || c.drainID != id) {
			if c.enqueue(&protocol.Packet{Kind: kind, From: update.GatewaySender, Session: id, Data: model.JSON(RelayDrainRequest{Until: until})}) {
				c.drainID = id
			}
		}
	}
}

func (g *Gateway) cancelGatewayDrain() {
	m := &g.maintenance
	m.mu.Lock()
	id := m.drainID
	m.until, m.retryAt, m.drainID, m.switching = time.Time{}, time.Time{}, "", false
	m.mu.Unlock()
	if id != "" {
		g.drainHint("gateway.resume", id, time.Time{})
	}
}

// reserveRestart atomically excludes relay and HTTP admission. Busy nodes and
// idle client registrations are not blockers. Queue reservations cover writes
// in progress, not just packets still waiting in the scheduler.
func (g *Gateway) reserveRestart(epoch int) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	m := &g.maintenance
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.relays) != 0 || m.httpActive != 0 || g.closed {
		return false
	}
	for id, c := range g.peers {
		if !c.client {
			info := g.nodes[id].Software
			if info.NodeGatewayMax != 0 && !info.SupportsGateway(epoch) || info.NodeGatewayMax == 0 && epoch > 1 {
				return false
			}
		}
	}
	if g.admission != nil && g.admission.packets.used.Load() != 0 {
		return false
	}
	m.switching, g.restarting = true, true
	return true
}

func (g *Gateway) gatewayBlockers() map[string]any {
	m := &g.maintenance
	m.mu.Lock()
	defer m.mu.Unlock()
	state := "staged"
	if !m.until.IsZero() {
		state = "draining"
	}
	if m.switching {
		state = "switching"
	}
	return map[string]any{"state": state, "relayCarriers": len(m.relays), "activeRequests": m.httpActive}
}
