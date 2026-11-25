package gateway

import (
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/koltyakov/control/internal/identity"
	"github.com/koltyakov/control/internal/model"
)

// Client transport identities are not enrolled machines. Only their immutable
// ownership and role are durable; routing metadata exists while connected.
func (g *Gateway) validateClient(n model.Node, p principal, ownerID string) error {
	if strings.HasPrefix(p.KeyID, "install:") {
		return errors.New("installation credentials cannot open client sessions")
	}
	if _, exists := g.nodes[n.ID]; exists {
		return errors.New("machine identities cannot open client sessions")
	}
	if _, exists := g.nodes[ownerID]; exists || g.machineStates[ownerID].Unregistered {
		return errors.New("machine identities cannot own client sessions")
	}
	if g.clientTransports[ownerID] != "" {
		return errors.New("transport identities cannot become client owners")
	}
	if g.owners[ownerID] != "" && !g.clientOwners[ownerID] {
		return errors.New("permanent machine identities cannot become client owners")
	}
	if owner := g.owners[ownerID]; owner != "" && owner != p.UserID {
		return errors.New("client ownership cannot change accounts")
	}
	if n.ID != ownerID && g.owners[n.ID] != "" && g.clientTransports[n.ID] != ownerID {
		return errors.New("session TLS identities must not reuse permanent identities")
	}
	if n.Name != "cli-"+ownerID[:16] || len(n.Capabilities) != 0 || n.System != nil || len(n.Labels) != 0 || n.ClientSessions || n.ClientOwners || n.InstructionDelegation || n.InstructionOnly {
		return errors.New("client sessions cannot advertise machine capabilities or resources")
	}
	for _, machine := range g.nodes {
		if machine.UserID == p.UserID && machine.Name == n.Name {
			return errors.New("client identity name conflicts with a machine")
		}
	}
	for id := range g.clientOwners {
		if id != ownerID && g.owners[id] == p.UserID && "cli-"+id[:16] == n.Name {
			return errors.New("client identity name conflicts with another client")
		}
	}
	if conflict, err := g.storedClientName(p.UserID, n.Name, ownerID); err != nil || conflict {
		return errors.New("client name reservation cannot be satisfied")
	}
	// The authenticated reservation already counts pending and live clients
	// atomically. connectPeer rechecks that this principal owns that reservation.
	return nil
}

// Reserve derived client names even while disconnected so machine enrollment
// cannot impersonate a client trusted by a receiver's name-based access rule.
func (g *Gateway) clientNameReserved(user, name string) bool {
	// Only derived "cli-" plus 16 identity characters can collide.
	if !strings.HasPrefix(name, "cli-") || len(name) != 20 {
		return false
	}
	for id := range g.clientOwners {
		if g.owners[id] == user && name == "cli-"+id[:16] {
			return true
		}
	}
	reserved, err := g.storedClientName(user, name, "")
	return reserved || err != nil
}

// lookupPeer is only for peer authentication, never machine discovery. Client
// metadata disappears on disconnect and is available only within its account.
func (g *Gateway) lookupPeer(w http.ResponseWriter, r *http.Request) {
	p := g.authenticate(bearer(r))
	// Machines come from the published directory snapshot, so peer
	// authentication during session setup never waits behind a commit.
	id := r.PathValue("id")
	g.observationMu.RLock()
	var entry observation
	ok := false
	if fleet := g.observed[p.UserID]; fleet != nil {
		entry, ok = fleet.nodes[id]
	}
	g.observationMu.RUnlock()
	n := entry.node
	n.ControlPending = false // Directory-only state, absent from the record.
	if !ok {
		g.mu.Lock()
		n, ok = g.clients[id]
		g.mu.Unlock()
	}
	if !ok || n.UserID != p.UserID {
		http.NotFound(w, r)
		return
	}
	n.GatewayProxy = nil // Peer authentication does not expose owner configuration.
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(n)
}

// Remove only the precise capability-free registrations created by the previous
// standalone implementation. Keep their identities and task ownership intact.
func (g *Gateway) migrateLegacyClients() error {
	changed := false
	for id, n := range g.nodes {
		if len(n.PublicKey) != ed25519.PublicKeySize || identity.ID(n.PublicKey) != id || n.Name != "cli-"+id[:16] || len(n.Capabilities) != 0 || n.System != nil || len(n.Labels) != 0 || n.Managed || n.Disabled || n.ClientSessions || g.machineStates[id].Revision != 0 {
			continue
		}
		if _, labelled := g.labelOverlays[id]; labelled {
			continue // Owner-managed labels make it a lifecycle-managed machine.
		}
		bound := false
		for _, i := range g.installations {
			if i.RedeemedID == id {
				bound = true
			}
		}
		if bound {
			continue
		}
		delete(g.nodes, id)
		g.clientOwners[id] = true
		changed = true
	}
	if changed {
		return g.persist()
	}
	return nil
}
