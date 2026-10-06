package gateway

import (
	"crypto/ed25519"
	"encoding/json"
	"net/http"
	"time"

	"github.com/koltyakov/control/internal/identity"
	"github.com/koltyakov/control/internal/model"
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
			if err := g.persist(); err != nil {
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
		Disabled *bool `json:"disabled"`
	}
	if json.NewDecoder(r.Body).Decode(&q) != nil || q.Disabled == nil {
		http.Error(w, "disabled is required", 400)
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
		if err := g.persist(); err != nil {
			g.nodes[id] = n
			g.machineStates[id] = old
			http.Error(w, "could not persist machine state", 500)
			return
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(state)
}
