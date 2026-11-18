package gateway

import (
	"slices"
	"time"

	"github.com/koltyakov/control/internal/identity"
	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/protocol"
	"github.com/koltyakov/control/internal/update"
)

// One bounded proxy group drains at a time. Child reservations are persisted by
// their updaters but are not software commits and consume no commit slots.
// Losing this coordinator never grants restart permission: the proxy still
// requires a fresh idle report, its own socket fence, and durable commit intent.
type proxyRollout struct {
	deployment, parent, token     string
	children                      []string
	leases                        map[string]time.Time
	deadline, parkUntil, lastHint time.Time
	releasing                     bool
}

const proxyAttemptWindow = 45 * time.Second

func managedEndpoint(n model.Node, url string) bool {
	return slices.ContainsFunc(n.ProxyEndpoints, func(e model.ProxyEndpoint) bool { return e.Managed && e.URL == url })
}

// proxyGroups uses both enrollment-owned dependencies and current signed route
// metadata. A name suffix or network address alone never establishes a child.
// Offline enrolled children remain unknown, not idle.
func (g *Gateway) proxyGroups() map[string][]string {
	g.mu.Lock()
	defer g.mu.Unlock()
	groups := map[string][]string{}
	for id, child := range g.nodes {
		if g.machineStates[id].Unregistered {
			continue
		}
		route := child.GatewayProxy
		if d, ok := g.proxyDependencies[id]; ok && d.UserID == child.UserID {
			route = &model.GatewayProxy{Worker: d.Worker, URL: d.URL}
		}
		if route == nil || route.Worker == id {
			continue
		}
		parent, exists := g.nodes[route.Worker]
		if exists && parent.UserID == child.UserID && managedEndpoint(parent, route.URL) {
			groups[parent.ID] = append(groups[parent.ID], id)
		}
	}
	for parent := range groups {
		slices.Sort(groups[parent])
	}
	return groups
}

func proxyPending(d *update.Deployment, peer rolloutPeer) bool {
	asset, ok := d.Manifest.Asset(peer.node.OS, peer.node.Software.Arch)
	return ok && !update.IsCurrent(peer.status.Software, d.Manifest.Version, asset) &&
		!updateStartupRefreshRequired(d, peer.status)
}

// coordinateProxyRollout runs under rolloutMu before ordinary node advancement.
// It suppresses child commits until their proxy is applied and their own
// reservation has been explicitly released and freshly acknowledged.
func (g *Gateway) coordinateProxyRollout(d *update.Deployment, peers []rolloutPeer, gatewayCurrent bool, now time.Time, send func(string, string, update.Command)) {
	g.proxyBlocks = map[string]bool{}
	byID := make(map[string]rolloutPeer, len(peers))
	for _, peer := range peers {
		byID[peer.node.ID] = peer
	}
	groups := g.proxyGroups()
	parents := make([]string, 0, len(groups))
	for parent, children := range groups {
		p := byID[parent]
		if !proxyPending(d, p) {
			continue
		}
		parents = append(parents, parent)
		g.proxyBlocks[parent] = true
		// A prepare may already have fenced the listener. Release that fence
		// if the preflight fails, but never undo an outstanding commit.
		if p.fresh && !p.proxyWorkIdle && p.status.ID == d.ID && p.status.Paused && !slices.Contains(d.InFlight, parent) {
			send(parent, "update.resume", update.Command{ID: d.ID})
		}
		for _, child := range children {
			// Legacy children must gain the negotiated drain support first.
			if byID[child].status.ProxyDrain {
				g.proxyBlocks[child] = true
			}
		}
	}
	slices.Sort(parents)
	group := g.proxyRollout
	if group != nil {
		p := byID[group.parent]
		failed := group.deployment != d.ID || !slices.Equal(groups[group.parent], group.children) ||
			!now.Before(group.deadline) || p.fresh && p.status.State == "error"
		done := p.fresh && updateAppliedAndResumed(d, p.status)
		if (failed || done) && !group.releasing {
			group.releasing = true
			group.lastHint = time.Time{}
		}
		if !group.releasing && group.parkUntil.IsZero() {
			for _, child := range group.children {
				s := byID[child]
				if !s.fresh || s.status.Busy || s.status.State == "error" ||
					s.status.ProxyFor != "" && s.status.ProxyFor != group.parent {
					group.releasing = true
					group.lastHint = time.Time{}
				}
			}
		}
		// Once committed, the proxy can disconnect normally. Before then a
		// lost report or unrelated proxy workload makes holding children futile.
		if !group.releasing && (!p.fresh || !p.proxyWorkIdle) && !slices.Contains(d.InFlight, group.parent) {
			group.releasing = true
			group.lastHint = time.Time{}
		}
		if group.releasing {
			g.releaseProxyGroup(group, byID, send, now)
			group = g.proxyRollout
		}
	}
	if group == nil && !now.Before(g.proxyRetryAt) {
		for _, parent := range parents {
			p := byID[parent]
			if !p.fresh || !p.proxyWorkIdle || p.status.ID != d.ID || !stagedStatus(p.status) || p.status.State == "restarting" ||
				!g.nodeMayUpdate(d, p.status, gatewayCurrent) || slices.Contains(d.InFlight, parent) {
				continue
			}
			children := groups[parent]
			eligible := len(children) > 0 && len(children) <= 256
			for _, child := range children {
				s := byID[child]
				eligible = eligible && s.fresh && s.status.ID == d.ID && s.status.ProxyDrain &&
					!s.status.Busy && s.status.State != "error" && s.status.State != "downloading" &&
					(!s.status.Paused || s.status.ProxyFor == parent) && !slices.Contains(d.InFlight, child)
			}
			if !eligible {
				continue
			}
			group = &proxyRollout{deployment: d.ID, parent: parent, token: identity.NewID(), children: children,
				leases: map[string]time.Time{}, deadline: now.Add(proxyAttemptWindow)}
			g.proxyRollout = group
			break
		}
	}
	if group != nil && !group.releasing {
		g.advanceProxyGroup(d, group, byID, send, now)
	}
	// Recover orphaned reservations after coordinator restart or supersession.
	// Never convert their old pause into an ordinary child software commit.
	for _, peer := range peers {
		if peer.status.ProxyFor == "" {
			continue
		}
		g.proxyBlocks[peer.node.ID] = true
		if group == nil || group.releasing || !slices.Contains(group.children, peer.node.ID) {
			send(peer.node.ID, "update.resume", update.Command{ID: peer.status.ID})
		}
	}
}

func (g *Gateway) advanceProxyGroup(d *update.Deployment, group *proxyRollout, peers map[string]rolloutPeer, send func(string, string, update.Command), now time.Time) {
	g.proxyBlocks[group.parent] = true
	reserved := true
	for _, child := range group.children {
		g.proxyBlocks[child] = true
		p := peers[child]
		s := p.status
		if !p.fresh && p.node.Online {
			// A new connection generation must acknowledge its own reservation;
			// an old process's cached lease is not permission to park this one.
			reserved = false
		}
		if p.fresh && s.ID == d.ID && s.Paused && s.ProxyFor == group.parent && !s.Busy && s.LeaseUntil.After(group.deadline.Add(30*time.Second)) {
			group.leases[child] = s.LeaseUntil
		} else if group.parkUntil.IsZero() || p.fresh {
			reserved = false
			if p.fresh && !s.Busy {
				g.mu.Lock()
				dependency := g.proxyDependencies[child]
				route := g.nodes[child].GatewayProxy
				url := dependency.URL
				if url == "" && route != nil {
					url = route.URL
				}
				g.mu.Unlock()
				send(child, "update.proxy-prepare", update.Command{ID: d.ID, ProxyFor: group.parent, ProxyURL: url, LeaseUntil: now.Add(2 * time.Minute).UTC()})
			}
		}
		if !now.Before(group.leases[child]) {
			reserved = false
		}
	}
	if !reserved {
		return
	}
	// Scope relay admission and outgoing-owner drain hints to these children.
	// Existing carriers must close naturally, including their FIN bookkeeping.
	if group.lastHint.IsZero() || now.Sub(group.lastHint) >= 5*time.Second {
		g.proxyDrainHint(group, false, now)
		group.lastHint = now
	}
	g.mu.Lock()
	g.maintenance.mu.Lock()
	drained := true
	for _, child := range group.children {
		if c := g.peers[child]; c != nil && g.maintenance.counts[c] != 0 {
			drained = false
		}
	}
	g.maintenance.mu.Unlock()
	g.mu.Unlock()
	if !drained {
		return
	}
	if group.parkUntil.IsZero() {
		group.parkUntil = now.Add(30 * time.Second).UTC()
	}
	for _, child := range group.children {
		g.mu.Lock()
		dependency := g.proxyDependencies[child]
		route := g.nodes[child].GatewayProxy
		url := dependency.URL
		if url == "" && route != nil {
			url = route.URL
		}
		g.mu.Unlock()
		send(child, "update.proxy-park", update.Command{ID: d.ID, ProxyFor: group.parent, ProxyURL: url, ParkUntil: group.parkUntil})
	}
	// The proxy still reports busy until every actual CONNECT tunnel ends. No
	// stale child report or zero byte count substitutes for that socket fence.
	delete(g.proxyBlocks, group.parent)
}

func (g *Gateway) releaseProxyGroup(group *proxyRollout, peers map[string]rolloutPeer, send func(string, string, update.Command), now time.Time) {
	if group.lastHint.IsZero() || now.Sub(group.lastHint) >= 5*time.Second {
		g.proxyDrainHint(group, true, now)
		group.lastHint = now
	}
	released := true
	for _, child := range group.children {
		p := peers[child]
		g.proxyBlocks[child] = true
		if !p.fresh || p.status.Paused || p.status.ProxyFor != "" {
			released = false
			send(child, "update.resume", update.Command{ID: group.deployment})
		}
	}
	// Lost peers eventually recover through their own bounded reservation.
	if released || now.After(group.deadline.Add(2*time.Minute)) {
		g.proxyRollout = nil
		g.proxyRetryAt = now.Add(30 * time.Second)
	}
}

func (g *Gateway) proxyDrainHint(group *proxyRollout, resume bool, now time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.maintenance.mu.Lock()
	if g.maintenance.peerDrains == nil {
		g.maintenance.peerDrains = map[string]time.Time{}
	}
	for id, until := range g.maintenance.peerDrains {
		if !now.Before(until) {
			delete(g.maintenance.peerDrains, id)
		}
	}
	until := minTime(group.deadline, now.Add(time.Minute))
	for _, child := range group.children {
		if resume {
			delete(g.maintenance.peerDrains, child)
		} else {
			g.maintenance.peerDrains[child] = until
		}
	}
	g.maintenance.mu.Unlock()
	kind := "gateway.drain"
	if resume {
		kind = "gateway.resume"
	}
	user := g.nodes[group.parent].UserID
	for _, c := range g.peers {
		if !c.relayDrain || c.userID != user {
			continue
		}
		for _, child := range group.children {
			c.enqueue(&protocol.Packet{Kind: kind, From: update.GatewaySender, Session: group.token,
				Data: model.JSON(RelayDrainRequest{Until: until, Peer: child})})
		}
	}
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}
