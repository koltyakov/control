package gateway

import (
	"crypto/ed25519"
	"encoding/json"
	"net/http"
	"time"

	"github.com/koltyakov/control/internal/identity"
	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/protocol"
	"github.com/koltyakov/control/internal/update"
)

func (g *Gateway) machineRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/node/state", g.nodeState)
	mux.HandleFunc("PATCH /v1/fleet/nodes/{id}", g.fleetAdmin(g.setMachineState))
}

// Called under g.mu. An enabling machine remains ineligible until it has
// acknowledged the persisted policy, so selectors never race ahead of it.
func (g *Gateway) machinePending(id string) bool {
	state := g.machineStates[id]
	c := g.peers[id]
	return c != nil && c.lifecycleRevision != state.Revision
}

func (g *Gateway) nodeState(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	var proof model.MachineStateProof
	if json.NewDecoder(r.Body).Decode(&proof) != nil || len(proof.PublicKey) != ed25519.PublicKeySize || time.Since(proof.SignedAt) > 5*time.Minute || time.Until(proof.SignedAt) > 5*time.Minute || !ed25519.Verify(proof.PublicKey, proof.Message(), proof.Signature) {
		http.Error(w, "invalid machine state proof", http.StatusUnauthorized)
		return
	}
	id := identity.ID(proof.PublicKey)
	g.mu.Lock()
	if g.clientOwners[id] || g.clientTransports[id] != "" {
		g.mu.Unlock()
		http.NotFound(w, r)
		return
	}
	state := g.machineStates[id]
	if c := g.peers[id]; c != nil && !proof.InspectOnly {
		if n, ok := g.nodes[id]; ok && !n.Managed {
			updated := n
			updated.Managed = true
			g.nodes[id] = updated
			if err := g.commit(change{nodes: []string{id}}); err != nil {
				g.nodes[id] = n
				g.mu.Unlock()
				http.Error(w, "could not persist node management support", 500)
				return
			}
		}
		c.lifecycleAt = time.Now()
		if proof.Revision == state.Revision {
			c.lifecycleRevision = proof.Revision
		}
	}
	g.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(state)
}

func (g *Gateway) setMachineState(w http.ResponseWriter, r *http.Request) {
	p := g.authenticate(bearer(r))
	r.Body = http.MaxBytesReader(w, r.Body, 1024)
	var q struct {
		Disabled *bool   `json:"disabled"`
		Name     *string `json:"name"`
	}
	if json.NewDecoder(r.Body).Decode(&q) != nil || (q.Disabled == nil) == (q.Name == nil) {
		http.Error(w, "provide either disabled or name", 400)
		return
	}
	if q.Name != nil && !validName.MatchString(*q.Name) {
		http.Error(w, "name must start with a letter or digit and contain 1..63 letters, digits, dots, hyphens or underscores", 400)
		return
	}
	id := r.PathValue("id")
	g.mu.Lock()
	defer g.mu.Unlock()
	n, ok := g.nodes[id]
	if !ok || n.UserID != p.UserID {
		http.NotFound(w, r)
		return
	}
	if q.Name != nil {
		g.renameMachine(w, n, *q.Name)
		return
	}
	if !n.Managed {
		http.Error(w, "upgrade this node before managing its state", http.StatusConflict)
		return
	}
	old := g.machineStates[id]
	state := old
	if state.Disabled != *q.Disabled {
		state.Disabled = *q.Disabled
		state.Revision++
		g.machineStates[id] = state
		updated := n
		updated.Disabled = state.Disabled
		g.nodes[id] = updated
		if err := g.commit(change{nodes: []string{id}, machineStates: []string{id}}); err != nil {
			g.nodes[id] = n
			g.machineStates[id] = old
			http.Error(w, "could not persist machine state", 500)
			return
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(state)
}

// Called under g.mu. Renaming changes only gateway routing metadata, not work
// admission or the signed installation proof retained for response recovery.
func (g *Gateway) renameMachine(w http.ResponseWriter, n model.Node, name string) {
	if g.clientNameReserved(n.UserID, name) {
		http.Error(w, "name is reserved for a client identity", http.StatusConflict)
		return
	}
	for id, other := range g.nodes {
		if id != n.ID && other.UserID == n.UserID && other.Name == name {
			http.Error(w, "name is already registered", http.StatusConflict)
			return
		}
	}
	for _, i := range g.installations {
		if i.UserID == n.UserID && g.installationName(i) == name && !i.Revoked && i.RedeemedID != n.ID && (i.RedeemedID != "" || time.Now().Before(i.ExpiresAt)) {
			http.Error(w, "name has an existing invitation or enrollment", http.StatusConflict)
			return
		}
	}
	old := g.machineStates[n.ID]
	state := old
	state.Name = name
	updated := n
	updated.Name = name
	g.nodes[n.ID], g.machineStates[n.ID] = updated, state
	if err := g.commit(change{nodes: []string{n.ID}, machineStates: []string{n.ID}}); err != nil {
		g.nodes[n.ID], g.machineStates[n.ID] = n, old
		http.Error(w, "could not persist machine name", 500)
		return
	}
	for _, peer := range g.peers {
		if peer.userID != n.UserID {
			continue
		}
		select {
		case peer.out <- &protocol.Packet{Kind: "directory.changed", From: update.GatewaySender}:
		default:
			_ = peer.ws.CloseNow()
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(state)
}
