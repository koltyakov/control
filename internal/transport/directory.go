package transport

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/koltyakov/control/internal/gateway"
	"github.com/koltyakov/control/internal/model"
)

func (p *Peer) Nodes(ctx context.Context) ([]model.Node, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, gateway.URL(p.cfg.Gateway, "/v1/nodes"), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+p.cfg.Token)
	resp, err := p.cfg.Route.Client(15*time.Second, true).Do(req)
	if err != nil {
		return nil, gatewayNetworkError(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, directoryStatusError("directory", resp)
	}
	var nodes []model.Node
	err = json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&nodes)
	p.mu.Lock()
	userID := p.userID
	p.mu.Unlock()
	for _, node := range nodes {
		if userID == "" || node.UserID != userID {
			return nil, errors.New("directory returned a node outside the authenticated fleet")
		}
	}
	return nodes, err
}

// Lookup separates fleet-machine discovery from identity checks for live clients.
// Online results are reused briefly; the gateway's offline notification for an
// identity, a directory change, or a reconnect evicts them early.
func (p *Peer) Lookup(ctx context.Context, name string) (model.Node, error) {
	now := time.Now()
	p.mu.Lock()
	clientSessions, userID, generation := p.clientSessions, p.userID, p.directoryGeneration
	if node, ok := p.cachedLookupLocked(name, now); ok {
		p.mu.Unlock()
		p.stats.lookupHits.Add(1)
		return node, nil
	}
	p.stats.lookupMisses.Add(1)
	// Concurrent misses for one name share a single gateway request. It runs
	// detached from any one caller, so a caller's cancellation cannot fail
	// the others; each caller still stops waiting with its own context.
	f := p.lookupFlights[name]
	if f == nil || f.generation != generation {
		f = &flight[model.Node]{done: make(chan struct{}), generation: generation}
		if p.lookupFlights == nil {
			p.lookupFlights = map[string]*flight[model.Node]{}
		}
		p.lookupFlights[name] = f
		p.mu.Unlock()
		p.fly(func(ctx context.Context) {
			node, err := p.lookup(ctx, name, clientSessions, userID, generation)
			p.mu.Lock()
			if err == nil && generation == p.directoryGeneration {
				p.storeLookupLocked(node, time.Now(), name, node.ID)
			}
			if p.lookupFlights[name] == f {
				delete(p.lookupFlights, name)
			}
			p.mu.Unlock()
			f.value, f.err = node, err
			close(f.done)
		})
	} else {
		p.mu.Unlock()
	}
	select {
	case <-ctx.Done():
		return model.Node{}, ctx.Err()
	case <-f.done:
		return f.value, f.err
	}
}

// flight is one shared request; waiters read value and err after done closes.
type flight[T any] struct {
	done       chan struct{}
	generation uint64
	value      T
	err        error
}

// lookupTimeout bounds a shared lookup, which no single caller can cancel.
const lookupTimeout = 20 * time.Second

// fly runs a shared request under the peer lifetime, inline once the peer is
// stopping.
func (p *Peer) fly(run func(context.Context)) {
	base := p.ctx
	if base == nil {
		base = context.Background()
	}
	shared := func() {
		ctx, cancel := context.WithTimeout(base, lookupTimeout)
		defer cancel()
		run(ctx)
	}
	if !p.worker(shared) {
		shared()
	}
}

func (p *Peer) lookup(ctx context.Context, name string, clientSessions bool, userID string, generation uint64) (model.Node, error) {
	if clientSessions && len(name) == 64 {
		return p.lookupID(ctx, name, userID)
	}
	hints := p.cfg.NameHints
	if clientSessions && hints != nil {
		// A hint only selects which gateway record to read. The gateway's
		// current record for that identity must still carry the requested
		// name; renames, retirements and errors fall back to the directory.
		if id := hints.Hint(name); len(id) == 64 {
			if peer, err := p.lookupID(ctx, id, userID); err == nil && peer.Name == name {
				return peer, nil
			}
		}
	}
	nodes, err := p.sharedNodes(ctx, generation)
	if err != nil {
		return model.Node{}, err
	}
	if hints != nil {
		hints.Remember(nodes)
	}
	// One directory read answers later name lookups for every listed machine.
	now := time.Now()
	p.mu.Lock()
	if generation == p.directoryGeneration {
		for _, node := range nodes {
			p.storeLookupLocked(node, now, node.ID, node.Name)
		}
	}
	p.mu.Unlock()
	for _, node := range nodes {
		if node.ID == name || node.Name == name {
			return node, nil
		}
	}
	return model.Node{}, fmt.Errorf("unknown node %q", name)
}

// sharedNodes coalesces concurrent directory reads made by lookups of
// different names. The caller's context is already detached; see Lookup.
func (p *Peer) sharedNodes(ctx context.Context, generation uint64) ([]model.Node, error) {
	p.mu.Lock()
	f := p.directoryFlight
	if f == nil || f.generation != generation {
		f = &flight[[]model.Node]{done: make(chan struct{}), generation: generation}
		p.directoryFlight = f
		p.mu.Unlock()
		f.value, f.err = p.Nodes(ctx)
		p.mu.Lock()
		if p.directoryFlight == f {
			p.directoryFlight = nil
		}
		p.mu.Unlock()
		close(f.done)
		return f.value, f.err
	}
	p.mu.Unlock()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-f.done:
		return f.value, f.err
	}
}

// lookupID reads one identity's scoped gateway record without the directory.
func (p *Peer) lookupID(ctx context.Context, id, userID string) (model.Node, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, gateway.URL(p.cfg.Gateway, "/v1/peers/"+url.PathEscape(id)), nil)
	if err != nil {
		return model.Node{}, err
	}
	req.Header.Set("Authorization", "Bearer "+p.cfg.Token)
	resp, err := p.cfg.Route.Client(15*time.Second, true).Do(req)
	if err != nil {
		return model.Node{}, gatewayNetworkError(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusOK {
		var peer model.Node
		if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&peer); err != nil {
			return model.Node{}, err
		}
		if peer.ID != id || userID == "" || peer.UserID != userID {
			return model.Node{}, errors.New("gateway returned a peer outside the authenticated fleet")
		}
		return peer, nil
	}
	if resp.StatusCode != http.StatusNotFound {
		return model.Node{}, directoryStatusError("peer directory", resp)
	}
	return model.Node{}, fmt.Errorf("unknown node %q", id)
}

func (p *Peer) Resolve(ctx context.Context, name string) (model.Node, error) {
	peer, err := p.Lookup(ctx, name)
	if err != nil {
		return peer, err
	}
	if !peer.Online {
		// Targets reappear after their gateway connection or the gateway
		// itself reconnects.
		return peer, transientSetup(fmt.Sprintf("node %s is offline", name))
	}
	now := time.Now()
	p.mu.Lock()
	p.pruneMembersLocked(now)
	p.members[peer.ID] = true
	p.memberSeen[peer.ID] = now
	p.channels[peer.ID] = peer.PeerChannels
	p.largePackets[peer.ID] = peer.LargePackets
	p.trickle[peer.ID] = peer.TrickleICE
	if peer.ClientOwner != "" {
		p.owners[peer.ID] = peer.ClientOwner
	}
	p.mu.Unlock()
	return peer, nil
}

// IsMember only accepts identities attested by this gateway's scoped directory.
// Identity ownership is immutable, so established direct sessions can remain
// usable during a gateway outage without crossing fleet boundaries.
func (p *Peer) IsMember(id string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.members[id]
}

// Owner is derived only from authenticated gateway metadata, never from an RPC.
// Transport identities still identify artifact-grant recipients and TLS peers.
func (p *Peer) Owner(id string) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if owner := p.owners[id]; owner != "" {
		return owner
	}
	return id
}

func (p *Peer) IdentityID() string { return p.cfg.Identity.ID }
