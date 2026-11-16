package gateway

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"time"

	"github.com/koltyakov/control/internal/identity"
	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/protocol"
	"github.com/koltyakov/control/internal/update"
)

type pendingProxyManagement struct {
	node     string
	peer     *connection
	mutation bool
	reply    chan model.ProxyManagementReply
}

func (g *Gateway) proxyCountsLocked(userID, node string) model.ProxyDependents {
	out := model.ProxyDependents{}
	for _, d := range g.managedDependentsLocked(userID, time.Now())[node] {
		out.Invitations += d.Invitations
		out.Children += d.Children
		out.Retiring += d.Retiring
	}
	return out
}

func (g *Gateway) manageProxy(w http.ResponseWriter, r *http.Request) {
	q := model.ProxyManagementRequest{ID: identity.NewID(), ExpiresAt: time.Now().Add(30 * time.Second).Unix()}
	if r.Method == http.MethodPut {
		q.Configure = &model.ProxyConfigure{}
		r.Body = http.MaxBytesReader(w, r.Body, 8192)
		d := json.NewDecoder(r.Body)
		d.DisallowUnknownFields()
		if d.Decode(q.Configure) != nil || d.Decode(new(any)) != io.EOF {
			http.Error(w, "expected explicit proxy settings", http.StatusBadRequest)
			return
		}
		if q.Configure.Enabled {
			if _, err := q.Configure.Endpoint(); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
		}
	}
	p := g.requestPrincipal(r)
	g.mu.Lock()
	n, ok := g.nodes[r.PathValue("id")]
	if !ok || n.UserID != p.UserID || g.owners[n.ID] != p.UserID {
		g.mu.Unlock()
		http.NotFound(w, r)
		return
	}
	peer := g.peers[n.ID]
	counts := g.proxyCountsLocked(p.UserID, n.ID)
	if !n.GatewayProxyManagement {
		g.mu.Unlock()
		if q.Configure == nil {
			consoleJSON(w, model.ProxyConfiguration{Interfaces: []model.ProxyInterface{}, Dependents: counts})
		} else {
			http.Error(w, "machine does not support proxy management; update its node and check proxy.manage permission", http.StatusConflict)
		}
		return
	}
	if !n.Online || peer == nil || n.Disabled || g.machinePending(n.ID) {
		g.mu.Unlock()
		http.Error(w, "machine must be online and enabled with no pending policy", http.StatusConflict)
		return
	}
	// The same lock protects invitation creation/redemption and dependency
	// commits. A mutation fence remains until an actual worker reply or a
	// later successful inspection, including after a caller timeout.
	if q.Configure != nil && counts.Total() > 0 {
		g.mu.Unlock()
		http.Error(w, "proxy has dependent invitations, children, or unacknowledged retirements", http.StatusConflict)
		return
	}
	if q.Configure != nil && n.ProxyManagementPending {
		g.mu.Unlock()
		http.Error(w, "previous proxy change is uncertain; inspect proxy settings first", http.StatusConflict)
		return
	}
	for _, pending := range g.proxyRequests {
		if pending.node == n.ID && (q.Configure != nil || !pending.mutation) {
			g.mu.Unlock()
			http.Error(w, "proxy request is pending or uncertain; inspect before changing settings", http.StatusConflict)
			return
		}
	}
	if len(g.proxyRequests) >= 64 {
		g.mu.Unlock()
		http.Error(w, "too many proxy management requests", http.StatusTooManyRequests)
		return
	}
	if g.proxyRequests == nil {
		g.proxyRequests = map[string]pendingProxyManagement{}
	}
	reply := make(chan model.ProxyManagementReply, 1)
	g.proxyRequests[q.ID] = pendingProxyManagement{node: n.ID, peer: peer, mutation: q.Configure != nil, reply: reply}
	if q.Configure != nil {
		fenced := n
		fenced.ProxyManagementPending = true
		g.nodes[n.ID] = fenced
		if err := g.commit(change{nodes: []string{n.ID}, audit: []AuditEvent{g.auditEventLocked(r, "", "machine.proxy.configure", n.Name, n.ID, "requested")}}); err != nil {
			g.nodes[n.ID] = n
			delete(g.proxyRequests, q.ID)
			g.mu.Unlock()
			http.Error(w, "could not record proxy intent; nothing was sent", http.StatusInternalServerError)
			return
		}
	}
	sent := peer.enqueue(&protocol.Packet{Kind: "proxy.request", From: update.GatewaySender, Data: model.JSON(q)})
	if !sent {
		delete(g.proxyRequests, q.ID)
	}
	g.mu.Unlock()
	if !sent {
		http.Error(w, "machine connection is busy; nothing was sent", http.StatusServiceUnavailable)
		return
	}
	// The durable node fence, not this reply channel, retains uncertainty.
	// Bound abandoned requests by their HTTP wait lifetime. Late replies are
	// reconciled by a fresh serialized inspection, never by mutation replay.
	defer func() { g.mu.Lock(); delete(g.proxyRequests, q.ID); g.mu.Unlock() }()
	timer := time.NewTimer(10 * time.Second)
	defer timer.Stop()
	select {
	case result := <-reply:
		if result.Error != "" {
			http.Error(w, "machine refused proxy settings or persistence failed; check live interfaces, source CIDRs, active connections, and inspect before retrying", http.StatusConflict)
			return
		}
		g.mu.Lock()
		result.Configuration.Dependents = g.proxyCountsLocked(p.UserID, n.ID)
		g.mu.Unlock()
		consoleJSON(w, result.Configuration)
	case <-timer.C:
		http.Error(w, "machine did not confirm; outcome is uncertain and settings may have applied. Inspect proxy settings before another change; do not replay", http.StatusGatewayTimeout)
	case <-r.Context().Done():
		return
	}
}

func (g *Gateway) receiveProxyManagement(n model.Node, peer *connection, data []byte) bool {
	if len(data) > 64<<10 {
		return false
	}
	var reply model.ProxyManagementReply
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if d.Decode(&reply) != nil || d.Decode(new(any)) != io.EOF || len(reply.ID) != 32 || len(reply.Error) > 256 || len(reply.Configuration.Interfaces) > 256 || model.ValidateProxyEndpoints(reply.Endpoints) != nil {
		return false
	}
	for _, e := range reply.Endpoints {
		if len(e.Sources) != 0 {
			return false
		}
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	pending, ok := g.proxyRequests[reply.ID]
	if !ok {
		return true
	}
	if pending.node != n.ID || pending.peer != peer || g.peers[n.ID] != peer {
		return false
	}
	if reply.Error == "" {
		current := g.nodes[n.ID]
		updated := current
		updated.ProxyEndpoints = reply.Endpoints
		updated.ProxyManagementPending = false
		g.nodes[n.ID] = updated
		if err := g.commit(change{nodes: []string{n.ID}}); err != nil {
			g.nodes[n.ID] = current
			reply.Error = "gateway could not persist observed configuration; outcome is uncertain"
		} else {
			// A successful serialized worker inspection reconciles a lost
			// mutation reply without sending that mutation again.
			if pending.mutation || current.ProxyManagementPending {
				if peer.health != nil {
					health := *peer.health
					health.Proxies = nil
					peer.health = &health
					g.publishTelemetry(n.ID)
				}
			}
			for id, other := range g.proxyRequests {
				if other.node == n.ID && (id == reply.ID || other.mutation) {
					delete(g.proxyRequests, id)
				}
			}
		}
	} else {
		delete(g.proxyRequests, reply.ID)
	}
	select {
	case pending.reply <- reply:
	default:
	}
	return true
}
