package gateway

import (
	"crypto/ed25519"
	"encoding/json"
	"maps"
	"net/http"
	"time"

	"github.com/koltyakov/control/internal/enrollment"
	"github.com/koltyakov/control/internal/identity"
	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/protocol"
	"github.com/koltyakov/control/internal/update"
)

func (g *Gateway) machineRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/node/state", g.nodeState)
	mux.HandleFunc("PATCH /v1/fleet/nodes/{id}", g.fleetAdmin(g.setMachineState))
	mux.HandleFunc("POST /v1/fleet/nodes/{id}/wake", g.fleetAdmin(g.wakeMachine))
	mux.HandleFunc("POST /v1/fleet/nodes/{id}/startup", g.fleetAdmin(g.changeStartup))
}

// Called under g.mu after persisting a policy change. The hint carries no state:
// the node reads and acknowledges it with a signed query. A full queue is safe
// to drop because the node's slower poll still finds the change.
func (g *Gateway) notifyMachineState(id string) {
	if c := g.peers[id]; c != nil && c.machineStatePush {
		c.enqueue(&protocol.Packet{Kind: "machine.state", From: update.GatewaySender})
	}
}

// Called under g.mu. An enabling machine remains ineligible until it has
// acknowledged the persisted policy, so selectors never race ahead of it.
func (g *Gateway) machinePending(id string) bool {
	state := g.machineStates[id]
	c := g.peers[id]
	return c != nil && c.lifecycleRevision != state.Revision
}

func (g *Gateway) nodeState(w http.ResponseWriter, r *http.Request) {
	defer g.trimIdentityCache()
	w.Header().Set("Cache-Control", "no-store")
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	var proof model.MachineStateProof
	if json.NewDecoder(r.Body).Decode(&proof) != nil || len(proof.PublicKey) != ed25519.PublicKeySize || time.Since(proof.SignedAt) > 5*time.Minute || time.Until(proof.SignedAt) > 5*time.Minute || !ed25519.Verify(proof.PublicKey, proof.Message(), proof.Signature) {
		http.Error(w, "invalid machine state proof", http.StatusUnauthorized)
		return
	}
	id := identity.ID(proof.PublicKey)
	g.mu.Lock()
	if err := g.loadIdentityLocked(id); err != nil {
		g.mu.Unlock()
		http.Error(w, "could not verify identity", http.StatusInternalServerError)
		return
	}
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
		g.publishObservations(id)
	}
	// An unregistered child has no gateway connection. Its signed read of the
	// exact retirement revision is the durable acknowledgement that releases a
	// managed proxy dependency.
	if err := g.acknowledgeRetirementLocked(id, proof, state); err != nil {
		g.mu.Unlock()
		http.Error(w, "could not persist retirement acknowledgement", http.StatusInternalServerError)
		return
	}
	g.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(state)
}

func (g *Gateway) setMachineState(w http.ResponseWriter, r *http.Request) {
	p := g.requestPrincipal(r)
	r.Body = http.MaxBytesReader(w, r.Body, 1024)
	var q struct {
		Disabled *bool   `json:"disabled"`
		Name     *string `json:"name"`
		Force    bool    `json:"force,omitempty"`
	}
	if json.NewDecoder(r.Body).Decode(&q) != nil || (q.Disabled == nil) == (q.Name == nil) {
		http.Error(w, "provide either disabled or name", 400)
		return
	}
	if q.Name != nil && !validName.MatchString(*q.Name) {
		http.Error(w, "name must start with a letter or digit and contain 1..63 letters, digits, dots, hyphens or underscores", 400)
		return
	}
	if q.Force && q.Name == nil {
		http.Error(w, "force requires a name", http.StatusBadRequest)
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
		g.renameMachine(w, r, n, *q.Name, q.Force)
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
		action := actionVerb(state.Disabled, "machine.disable", "machine.enable")
		if err := g.commit(change{nodes: []string{id}, machineStates: []string{id}, audit: []AuditEvent{g.auditEventLocked(r, "", action, n.Name, id, "")}}); err != nil {
			g.nodes[id] = n
			g.machineStates[id] = old
			http.Error(w, "could not persist machine state", 500)
			return
		}
		g.notifyMachineState(id)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(state)
}

// Called under g.mu. Renaming changes only gateway routing metadata, not work
// admission or the signed installation proof retained for response recovery.
func (g *Gateway) renameMachine(w http.ResponseWriter, r *http.Request, n model.Node, name string, force bool) {
	targets := []model.Node{n}
	names := map[string]string{n.ID: name}
	// Pairing is gateway-owned enrollment metadata, never a name suffix.
	// Renaming System directly remains a context-only operation.
	if n.HybridID != "" && n.HybridRole == "user" {
		for _, other := range g.nodes {
			if other.ID == n.ID || other.UserID != n.UserID || other.HybridID != n.HybridID {
				continue
			}
			if len(targets) != 1 || other.HybridRole != "system" {
				http.Error(w, "host rename requires one User and one System context", http.StatusConflict)
				return
			}
			targets = append(targets, other)
			names[other.ID] = enrollment.SystemName(name)
		}
		if len(targets) == 2 && len(enrollment.SystemName(name)) > 63 {
			http.Error(w, "hybrid names must be at most 56 characters to leave room for -system", http.StatusBadRequest)
			return
		}
	}
	retracted := map[string]installation{}
	for _, target := range targets {
		if !g.checkRename(w, target, names[target.ID], retracted) {
			return
		}
	}
	if len(retracted) > 0 && !force {
		http.Error(w, enrollment.ErrPendingInvitation.Error(), http.StatusConflict)
		return
	}
	oldStates := map[string]model.MachineState{}
	persisted := change{}
	for _, target := range targets {
		if old, ok := g.machineStates[target.ID]; ok {
			oldStates[target.ID] = old
		}
		state := g.machineStates[target.ID]
		state.Name = names[target.ID]
		updated := target
		updated.Name = state.Name
		g.nodes[target.ID], g.machineStates[target.ID] = updated, state
		persisted.nodes = append(persisted.nodes, target.ID)
		persisted.machineStates = append(persisted.machineStates, target.ID)
		persisted.audit = append(persisted.audit, g.auditEventLocked(r, "", "machine.rename", state.Name, target.ID, "was "+target.Name))
	}
	for hash, i := range retracted {
		i.Revoked = true
		g.installations[hash] = i
		persisted.invitations = append(persisted.invitations, hash)
	}
	if err := g.commit(persisted); err != nil {
		for _, target := range targets {
			g.nodes[target.ID] = target
			delete(g.machineStates, target.ID)
		}
		maps.Copy(g.machineStates, oldStates)
		maps.Copy(g.installations, retracted)
		http.Error(w, "could not persist machine name", 500)
		return
	}
	for _, target := range targets {
		g.notifyMachineState(target.ID)
	}
	for _, peer := range g.peers {
		if peer.userID != n.UserID {
			continue
		}
		// The writer releases every queued packet's reservation, including
		// metadata-only notifications. Never publish directly to its channel.
		if !peer.enqueue(&protocol.Packet{Kind: "directory.changed", From: update.GatewaySender}) {
			_ = peer.ws.CloseNow()
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(g.machineStates[n.ID])
}

// Called under g.mu before any rename is applied. Collect pending reservations
// separately so a hard conflict on either name cannot retract an invitation.
func (g *Gateway) checkRename(w http.ResponseWriter, n model.Node, name string, retracted map[string]installation) bool {
	if g.clientNameReserved(n.UserID, name) {
		http.Error(w, "name is reserved for a client identity", http.StatusConflict)
		return false
	}
	for id, other := range g.nodes {
		if id != n.ID && other.UserID == n.UserID && other.Name == name {
			http.Error(w, "name is already registered", http.StatusConflict)
			return false
		}
	}
	for hash, i := range g.installations {
		if i.UserID == n.UserID && g.installationName(i) == name && !i.Revoked && i.RedeemedID != n.ID && (i.RedeemedID != "" || time.Now().Before(i.ExpiresAt)) {
			if i.RedeemedID != "" {
				http.Error(w, "name has an existing enrollment", http.StatusConflict)
				return false
			}
			retracted[hash] = i
		}
	}
	return true
}
