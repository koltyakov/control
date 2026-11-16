package gateway

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/koltyakov/control/internal/buildinfo"
	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/protocol"
	"github.com/koltyakov/control/internal/update"
)

type sentUpdate struct {
	id, kind string
	command  update.Command
}

type proxyRolloutFixture struct {
	g        *Gateway
	d        *update.Deployment
	info     buildinfo.Info
	parent   string
	children []string
	now      time.Time
	sent     []sentUpdate
}

func newProxyRolloutFixture(t *testing.T) *proxyRolloutFixture {
	t.Helper()
	g, d, info := deferralFixture(t)
	clear(g.nodes)
	clear(g.peers)
	clear(g.updateStatus)
	f := &proxyRolloutFixture{g: g, d: d, info: info, parent: strings.Repeat("a", 64),
		children: []string{strings.Repeat("b", 64), strings.Repeat("c", 64)}, now: time.Now()}
	old := info
	old.Version, old.SHA256 = "older", strings.Repeat("0", 64)
	for _, id := range append([]string{f.parent}, f.children...) {
		g.nodes[id] = model.Node{ID: id, UserID: "fleet", OS: info.OS, Software: old, Online: true}
		g.peers[id] = &connection{userID: "fleet", relayDrain: true, out: make(chan *protocol.Packet, 256)}
		g.updateStatus[id] = update.Status{ID: d.ID, Software: old, State: "staged", SeenAt: f.now}
	}
	parent := g.nodes[f.parent]
	parent.ProxyEndpoints = []model.ProxyEndpoint{{URL: "http://10.0.0.1:3128", Managed: true}}
	g.nodes[f.parent] = parent
	s := g.updateStatus[f.parent]
	s.Busy = true
	g.updateStatus[f.parent] = s
	for _, id := range f.children {
		g.proxyDependencies[id] = proxyDependency{Worker: f.parent, UserID: "fleet", URL: parent.ProxyEndpoints[0].URL}
		s := g.updateStatus[id]
		s.ProxyDrain = true
		g.updateStatus[id] = s
	}
	return f
}

func (f *proxyRolloutFixture) snapshot() []rolloutPeer {
	var peers []rolloutPeer
	for _, id := range append([]string{f.parent}, f.children...) {
		n, s := f.g.nodes[id], f.g.updateStatus[id]
		peers = append(peers, rolloutPeer{node: n, status: s, fresh: f.g.peers[id] != nil && f.now.Sub(s.SeenAt) < updateStatusFreshness})
	}
	return peers
}

func (f *proxyRolloutFixture) tick(t *testing.T) {
	t.Helper()
	f.sent = nil
	send := func(id, kind string, c update.Command) { f.sent = append(f.sent, sentUpdate{id, kind, c}) }
	peers := f.snapshot()
	f.g.coordinateProxyRollout(f.g.updates.Current(), peers, true, f.now, send)
	if _, err := f.g.advanceNodes(f.g.updates.Current(), peers, true, send); err != nil {
		t.Fatal(err)
	}
}

func (f *proxyRolloutFixture) has(id, kind string) bool {
	return slices.ContainsFunc(f.sent, func(s sentUpdate) bool { return s.id == id && s.kind == kind })
}

func (f *proxyRolloutFixture) reserveChildren() {
	for _, id := range f.children {
		s := f.g.updateStatus[id]
		s.ProxyFor, s.Paused, s.State, s.LeaseUntil = f.parent, true, "ready", f.now.Add(2*time.Minute)
		f.g.updateStatus[id] = s
	}
}

func TestProxyRolloutReservesDrainsProxyFirstThenResumesChildren(t *testing.T) {
	f := newProxyRolloutFixture(t)
	f.tick(t)
	for _, child := range f.children {
		if !f.has(child, "update.proxy-prepare") || f.has(child, "update.prepare") || f.has(child, "update.commit") {
			t.Fatal("did not reserve child separately from its software update", f.sent)
		}
	}
	if f.has(f.parent, "update.prepare") {
		t.Fatal("proxy reserved before children acknowledged")
	}
	f.reserveChildren()
	f.g.maintenance.counts = map[*connection]int{f.g.peers[f.children[0]]: 1}
	f.tick(t)
	if f.has(f.children[0], "update.proxy-park") {
		t.Fatal("park interrupted a relay carrier")
	}
	f.g.maintenance.counts[f.g.peers[f.children[0]]] = 0
	f.tick(t)
	if !f.has(f.children[0], "update.proxy-park") || f.has(f.parent, "update.prepare") {
		t.Fatal("proxy socket check was bypassed", f.sent)
	}
	s := f.g.updateStatus[f.parent]
	s.Busy = false
	f.g.updateStatus[f.parent] = s
	f.tick(t)
	if !f.has(f.parent, "update.prepare") {
		t.Fatal("drained proxy did not reserve")
	}
	s.Paused, s.State, s.LeaseUntil = true, "ready", f.now.Add(time.Minute)
	f.g.updateStatus[f.parent] = s
	f.tick(t)
	if !f.has(f.parent, "update.commit") || !slices.Contains(f.g.updates.Current().InFlight, f.parent) {
		t.Fatal("proxy commit lacked durable intent", f.sent)
	}
	for _, child := range f.children {
		if f.has(child, "update.resume") || f.has(child, "update.commit") {
			t.Fatal("child resumed or updated before proxy confirmation")
		}
	}
	s.Software, s.State = f.info, "applied"
	f.g.updateStatus[f.parent] = s
	f.tick(t)
	if !f.has(f.parent, "update.resume") || f.has(f.children[0], "update.resume") {
		t.Fatal("proxy must leave maintenance before its children resume", f.sent)
	}
	s.Paused = false
	f.g.updateStatus[f.parent] = s
	f.tick(t)
	for _, child := range f.children {
		if !f.has(child, "update.resume") || f.has(child, "update.prepare") {
			t.Fatal("child did not resume before update", f.sent)
		}
	}
	for _, child := range f.children {
		cs := f.g.updateStatus[child]
		cs.ProxyFor, cs.Paused, cs.State = "", false, "staged"
		f.g.updateStatus[child] = cs
	}
	f.tick(t)
	f.tick(t)
	for _, child := range f.children {
		if !f.has(child, "update.prepare") {
			t.Fatal("resumed child never became update eligible", f.sent)
		}
	}
}

func TestProxyRolloutWaitsForBusyStaleOfflineAndLegacyChildren(t *testing.T) {
	for _, name := range []string{"busy", "stale", "offline", "legacy", "own-update"} {
		t.Run(name, func(t *testing.T) {
			f := newProxyRolloutFixture(t)
			child := f.children[0]
			s := f.g.updateStatus[child]
			switch name {
			case "busy":
				s.Busy = true
			case "stale":
				s.SeenAt = f.now.Add(-time.Minute)
			case "offline":
				delete(f.g.peers, child)
				n := f.g.nodes[child]
				n.Online = false
				f.g.nodes[child] = n
			case "legacy":
				s.ProxyDrain = false
			case "own-update":
				s.Paused = true
				s.State = "ready"
				s.LeaseUntil = f.now.Add(time.Minute)
			}
			f.g.updateStatus[child] = s
			// An apparent idle gap at the proxy cannot authorize the restart.
			p := f.g.updateStatus[f.parent]
			p.Busy = false
			f.g.updateStatus[f.parent] = p
			f.tick(t)
			if f.g.proxyRollout != nil || f.has(f.parent, "update.prepare") || f.has(child, "update.proxy-prepare") {
				t.Fatal("unknown or busy child was treated as reservable", f.sent)
			}
			if name == "legacy" && !f.has(child, "update.prepare") {
				t.Fatal("legacy child cannot bootstrap drain support")
			}
		})
	}
}

func TestProxyRolloutFailureReleasesHoldsWithoutClearingUncertainCommit(t *testing.T) {
	for _, name := range []string{"busy-after-request", "timeout", "proxy-error", "superseded", "changed-membership", "lost-child", "new-connection"} {
		t.Run(name, func(t *testing.T) {
			f := newProxyRolloutFixture(t)
			f.tick(t)
			f.reserveChildren()
			f.tick(t)
			child := f.children[0]
			switch name {
			case "busy-after-request":
				f.g.proxyRollout.parkUntil = time.Time{}
				s := f.g.updateStatus[child]
				s.Busy = true
				s.Paused = false
				f.g.updateStatus[child] = s
			case "timeout":
				f.g.proxyRollout.deadline = f.now.Add(-time.Second)
			case "proxy-error":
				s := f.g.updateStatus[f.parent]
				s.State = "error"
				f.g.updateStatus[f.parent] = s
			case "superseded":
				f.g.proxyRollout.deployment = "old"
			case "changed-membership":
				delete(f.g.proxyDependencies, child)
			case "lost-child":
				f.g.proxyRollout.parkUntil = time.Time{}
				delete(f.g.peers, child)
			case "new-connection":
				s := f.g.updateStatus[child]
				s.SeenAt = f.now.Add(-time.Minute)
				f.g.updateStatus[child] = s
				f.g.peers[child] = &connection{out: make(chan *protocol.Packet, 256)}
			}
			if err := f.g.updates.NodeInstall(f.d.ID, f.parent, true); err != nil {
				t.Fatal(err)
			}
			f.tick(t)
			if name == "new-connection" {
				if f.has(f.parent, "update.prepare") || !f.g.proxyBlocks[f.parent] {
					t.Fatal("old reservation authorized new connection")
				}
			} else if !f.has(child, "update.resume") {
				t.Fatal("failure stranded a child hold", f.sent)
			}
			if name != "proxy-error" && !slices.Contains(f.g.updates.Current().InFlight, f.parent) {
				t.Fatal("failure fabricated proxy restart success")
			}
			if f.has(child, "update.commit") {
				t.Fatal("failure converted hold into child commit")
			}
		})
	}
}

func TestProxyRolloutRecoveryAndFleetScopedDrains(t *testing.T) {
	f := newProxyRolloutFixture(t)
	f.reserveChildren() // Restored markers, with no in-memory coordinator.
	f.tick(t)
	if f.g.proxyRollout == nil || f.has(f.children[0], "update.resume") {
		t.Fatal("did not adopt fresh restored reservations")
	}
	foreign := &connection{userID: "other-fleet", relayDrain: true, out: make(chan *protocol.Packet, 8)}
	f.g.peers["foreign"] = foreign
	group := f.g.proxyRollout
	f.g.proxyDrainHint(group, false, f.now)
	if len(foreign.out) != 0 {
		t.Fatal("drain revealed another fleet's child identities")
	}
	for _, id := range f.children {
		c := f.g.peers[id]
		seen := false
		for len(c.out) > 0 {
			packet := <-c.out
			var request RelayDrainRequest
			if err := json.Unmarshal(packet.Data, &request); err != nil {
				t.Fatal(err)
			}
			if packet.Kind == "gateway.drain" && slices.Contains(f.children, request.Peer) {
				seen = true
			}
		}
		if !seen {
			t.Fatal("missing scoped outgoing-owner drain")
		}
	}
	// A hold belonging to an obsolete coordinator is released, not committed.
	f.g.proxyRollout = nil
	f.g.proxyRetryAt = f.now.Add(time.Minute)
	f.tick(t)
	if !f.has(f.children[0], "update.resume") || f.has(f.children[0], "update.commit") {
		t.Fatal("orphan hold was reused")
	}
}

func TestProxyRolloutLeavesUnrelatedNodeCommitsIndependent(t *testing.T) {
	f := newProxyRolloutFixture(t)
	peers := f.snapshot()
	f.g.coordinateProxyRollout(f.d, peers, true, f.now, func(string, string, update.Command) {})
	id := strings.Repeat("d", 64)
	s := update.Status{ID: f.d.ID, Software: peers[0].status.Software, State: "ready", Paused: true, LeaseUntil: time.Now().Add(time.Minute)}
	peers = append(peers, rolloutPeer{node: model.Node{ID: id, OS: s.Software.OS, Software: s.Software, Online: true}, status: s, fresh: true})
	committed := false
	_, err := f.g.advanceNodes(f.d, peers, true, func(node, kind string, _ update.Command) {
		if kind == "update.commit" && node == id {
			committed = true
		}
	})
	if err != nil || !committed {
		t.Fatal("proxy group stopped an unrelated idle update", err)
	}
}

func TestProxyRelayFencePreservesLiveTrafficAndExpires(t *testing.T) {
	child, caller, other := &connection{}, &connection{}, &connection{}
	g := &Gateway{}
	open := &protocol.Packet{Kind: "open", To: "child", Session: "live"}
	if !g.admitRelay("caller", caller, child, open) {
		t.Fatal("fixture open failed")
	}
	g.maintenance.peerDrains = map[string]time.Time{"child": time.Now().Add(time.Minute)}
	if g.admitRelay("caller", caller, child, &protocol.Packet{Kind: "open", To: "child", Session: "new"}) ||
		g.admitRelay("child", child, caller, &protocol.Packet{Kind: "open", To: "caller", Session: "new"}) {
		t.Fatal("new carrier passed child maintenance fence")
	}
	if !g.admitRelay("caller", caller, child, &protocol.Packet{Kind: "data", To: "child", Session: "live", Data: []byte("accepted")}) {
		t.Fatal("scoped fence interrupted admitted bytes")
	}
	if !g.admitRelay("caller", caller, other, &protocol.Packet{Kind: "open", To: "other", Session: "unrelated"}) {
		t.Fatal("scoped fence blocked unrelated traffic")
	}
	g.maintenance.peerDrains["child"] = time.Now().Add(-time.Second)
	if !g.admitRelay("caller", caller, child, &protocol.Packet{Kind: "open", To: "child", Session: "recovered"}) {
		t.Fatal("lost coordinator fenced child forever")
	}
}
