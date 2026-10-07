package transport

import (
	"sync/atomic"
	"time"

	"github.com/koltyakov/control/internal/model"
	"github.com/pion/webrtc/v4"
)

const (
	// lookupTTL bounds how long a gateway identity lookup is reused. Offline
	// notifications and directory changes evict entries sooner.
	lookupTTL        = 15 * time.Second
	maxLookupEntries = 4096
	// Directory membership without a live link is forgotten after this idle
	// period once the map grows; active sessions always keep their member.
	memberIdleTTL    = 10 * time.Minute
	maxIdleMembers   = 1024
	maxSCTPReceive   = 4 << 20
	maxSTUNGather    = 2 * time.Second
	minSTUNGather    = 250 * time.Millisecond
	reconnectInitial = time.Second
	reconnectMax     = 30 * time.Second
	// A restarting gateway usually returns within seconds. Retry at the initial
	// cadence this many times before backing off, so callers are not left
	// waiting on a long delay after it is back.
	reconnectFastAttempts = 10
)

type lookupEntry struct {
	node    model.Node
	expires time.Time
}

type transportStats struct {
	gatewayReconnects     atomic.Int64
	gatewayConnectFailure atomic.Int64
	directSessions        atomic.Int64
	relaySessions         atomic.Int64
	relayFallbacks        atomic.Int64
	linkOverflows         atomic.Int64
	lookupHits            atomic.Int64
	lookupMisses          atomic.Int64
	lastOverflowLog       atomic.Int64
}

// Stats reports cumulative transport counters for local diagnostics.
func (p *Peer) Stats() model.TransportStats {
	return model.TransportStats{
		GatewayReconnects:      p.stats.gatewayReconnects.Load(),
		GatewayConnectFailures: p.stats.gatewayConnectFailure.Load(),
		DirectSessions:         p.stats.directSessions.Load(),
		RelaySessions:          p.stats.relaySessions.Load(),
		RelayFallbacks:         p.stats.relayFallbacks.Load(),
		LinkOverflows:          p.stats.linkOverflows.Load(),
		LookupHits:             p.stats.lookupHits.Load(),
		LookupMisses:           p.stats.lookupMisses.Load(),
	}
}

// newWebRTCAPI keeps candidate gathering well inside the direct-connection
// budget, so one unreachable STUN server cannot force every session onto the
// relay. A larger SCTP window raises the per-carrier throughput ceiling on
// high-latency paths, where it limits all lanes sharing the carrier.
func newWebRTCAPI(direct time.Duration) *webrtc.API {
	var settings webrtc.SettingEngine
	settings.SetSTUNGatherTimeout(max(minSTUNGather, min(maxSTUNGather, direct*2/5)))
	settings.SetSCTPMaxReceiveBufferSize(maxSCTPReceive)
	return webrtc.NewAPI(webrtc.WithSettingEngine(settings))
}

// Called with p.mu held.
func (p *Peer) cachedLookupLocked(name string, now time.Time) (model.Node, bool) {
	entry, ok := p.lookups[name]
	if !ok {
		return model.Node{}, false
	}
	if !now.Before(entry.expires) {
		delete(p.lookups, name)
		return model.Node{}, false
	}
	return entry.node, true
}

// Called with p.mu held. Only online records are cached so a reconnecting
// peer is never reported offline from a stale entry.
func (p *Peer) storeLookupLocked(node model.Node, now time.Time, keys ...string) {
	if !node.Online {
		return
	}
	if len(p.lookups) >= maxLookupEntries {
		for key, entry := range p.lookups {
			if !now.Before(entry.expires) {
				delete(p.lookups, key)
			}
		}
		if len(p.lookups) >= maxLookupEntries {
			p.lookups = map[string]lookupEntry{}
		}
	}
	entry := lookupEntry{node: node, expires: now.Add(lookupTTL)}
	for _, key := range keys {
		if key != "" {
			p.lookups[key] = entry
		}
	}
}

// Called with p.mu held.
func (p *Peer) evictLookupLocked(id string) {
	for key, entry := range p.lookups {
		if entry.node.ID == id {
			delete(p.lookups, key)
		}
	}
}

// Called with p.mu held. A member with a link, session, or pending setup is
// still in use and must remain authorized for incoming streams.
func (p *Peer) memberInUseLocked(id string) bool {
	for _, l := range p.links {
		if l.remote == id {
			return true
		}
	}
	for _, s := range p.allSessions {
		if s.remote == id {
			return true
		}
	}
	for _, setup := range p.incomingSetups {
		if setup.remote == id {
			return true
		}
	}
	return false
}

// Called with p.mu held.
func (p *Peer) forgetMemberLocked(id string) {
	delete(p.members, id)
	delete(p.memberSeen, id)
	delete(p.owners, id)
	delete(p.channels, id)
	delete(p.largePackets, id)
}

// Called with p.mu held. Client transport identities are per process, so
// long-lived nodes would otherwise retain every caller they ever resolved.
func (p *Peer) pruneMembersLocked(now time.Time) {
	if len(p.members) <= maxIdleMembers {
		return
	}
	for id := range p.members {
		if now.Sub(p.memberSeen[id]) > memberIdleTTL && !p.memberInUseLocked(id) {
			p.forgetMemberLocked(id)
		}
	}
}

// Called with p.mu held.
func (p *Peer) packetSizeLocked(remote string) int {
	if p.largePackets[remote] {
		return largePacketSize
	}
	return legacyPacketSize
}
