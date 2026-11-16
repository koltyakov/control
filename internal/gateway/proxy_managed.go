package gateway

import (
	"crypto/ed25519"
	"encoding/json"
	"net/http"
	"slices"
	"time"

	"github.com/koltyakov/control/internal/identity"
	"github.com/koltyakov/control/internal/model"
)

// proxyDependency durably records that an enrolled child reaches the gateway
// through a Control-managed listener. It is keyed by the child's immutable
// identity, written in the redemption transaction, and survives gateway
// restarts, invitation history clearing, disconnects, and re-registration.
// Only the child's signed acknowledgement of its own retirement, its signed
// re-enrollment through another route, or an explicit owner release removes it.
type proxyDependency struct {
	UserID     string    `json:"userId"`
	Worker     string    `json:"worker"`
	URL        string    `json:"url"`
	Invitation string    `json:"invitation,omitempty"`
	CreatedAt  time.Time `json:"createdAt"`
}

func (g *Gateway) managedProxyRoutes(mux *http.ServeMux) {
	for _, route := range []string{"GET /v1/fleet/nodes/{id}/proxy", "PUT /v1/fleet/nodes/{id}/proxy"} {
		mux.Handle(route, g.consoleHeaders(http.HandlerFunc(g.fleetAdmin(g.manageProxy))))
	}
	mux.HandleFunc("POST /v1/node/proxy", g.nodeProxyState)
	mux.HandleFunc("DELETE /v1/fleet/proxy-dependencies/{id}", g.fleetAdmin(g.releaseProxyDependency))
}

// Called under g.mu. A pending invitation keeps its listener while it can
// still be redeemed: unrevoked, unredeemed, and unexpired. Hybrid companions
// are separate invitation records and count independently.
func pendingManagedInvitation(i installation, now time.Time) bool {
	return i.GatewayProxy != nil && i.GatewayProxy.Managed && !i.Revoked && i.RedeemedID == "" && now.Before(i.ExpiresAt)
}

// Called under g.mu. hasCredential reports an unrevoked redemption that can
// still register, even before the child's first registration.
func (g *Gateway) hasCredentialLocked(id string) bool {
	for _, i := range g.installations {
		if i.RedeemedID == id && !i.Revoked {
			return true
		}
	}
	return false
}

// proxyChildHostLocked groups only contexts redeemed from the same Hybrid
// installation. Names and reported routes are not evidence of host pairing.
func (g *Gateway) proxyChildHostLocked(child string, d proxyDependency) string {
	for _, i := range g.installations {
		if i.ID == d.Invitation && i.UserID == d.UserID && i.RedeemedID == child && i.HybridID != "" {
			return "hybrid:" + i.HybridID
		}
	}
	return "node:" + child
}

// managedDependentsLocked counts dependencies of each managed endpoint for one
// fleet, keyed by worker and endpoint URL. Disconnected children count as
// enrolled; an unregistered child counts as retiring until it acknowledges.
// Caller holds g.mu.
func (g *Gateway) managedDependentsLocked(userID string, now time.Time) map[string]map[string]*model.ProxyDependents {
	out := map[string]map[string]*model.ProxyDependents{}
	hosts := map[[3]string]bool{}
	entry := func(worker, url string) *model.ProxyDependents {
		if out[worker] == nil {
			out[worker] = map[string]*model.ProxyDependents{}
		}
		if out[worker][url] == nil {
			out[worker][url] = &model.ProxyDependents{}
		}
		return out[worker][url]
	}
	for _, i := range g.installations {
		if i.UserID == userID && pendingManagedInvitation(i, now) && g.owners[i.GatewayProxy.Worker] == userID {
			entry(i.GatewayProxy.Worker, i.GatewayProxy.URL).Invitations++
		}
	}
	for child, d := range g.proxyDependencies {
		if d.UserID != userID || g.owners[d.Worker] != userID {
			continue
		}
		_, registered := g.nodes[child]
		switch {
		case g.machineStates[child].Unregistered:
			entry(d.Worker, d.URL).Retiring++
		case registered || g.hasCredentialLocked(child):
			count := entry(d.Worker, d.URL)
			count.Children++
			key := [3]string{d.Worker, d.URL, g.proxyChildHostLocked(child, d)}
			if !hosts[key] {
				hosts[key] = true
				count.Machines++
			}
		}
	}
	return out
}

// managedListenersLocked returns the sorted endpoint URLs whose listeners the
// worker must keep running. Caller holds g.mu.
func (g *Gateway) managedListenersLocked(worker string, now time.Time) []string {
	userID := g.owners[worker]
	listeners := []string{}
	if userID == "" || g.machineStates[worker].Unregistered {
		return listeners
	}
	for _, endpoint := range g.nodes[worker].ProxyEndpoints {
		if endpoint.Managed {
			listeners = append(listeners, endpoint.URL)
		}
	}
	slices.Sort(listeners)
	return listeners
}

// notifyProxyWorkers hints that managed listener dependencies changed. The
// worker reads its desired state with a signed query; its periodic poll also
// observes changes made only by the passage of time, such as expiry.
// Called under g.mu after commit.
func (g *Gateway) notifyProxyWorkers(workers ...string) {
	for _, worker := range workers {
		if worker != "" {
			g.notifyMachineState(worker)
		}
	}
}

// nodeProxyState returns only the signing worker's own desired listeners. It
// mutates nothing, so a captured proof cannot change another fleet's listener.
func (g *Gateway) nodeProxyState(w http.ResponseWriter, r *http.Request) {
	defer g.trimIdentityCache()
	w.Header().Set("Cache-Control", "no-store")
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	var proof model.ProxyStateProof
	if json.NewDecoder(r.Body).Decode(&proof) != nil || len(proof.PublicKey) != ed25519.PublicKeySize || time.Since(proof.SignedAt) > 5*time.Minute || time.Until(proof.SignedAt) > 5*time.Minute || !ed25519.Verify(proof.PublicKey, proof.Message(), proof.Signature) {
		http.Error(w, "invalid proxy state proof", http.StatusUnauthorized)
		return
	}
	id := identity.ID(proof.PublicKey)
	g.mu.Lock()
	if err := g.loadIdentityLocked(id); err != nil {
		g.mu.Unlock()
		http.Error(w, "could not verify identity", http.StatusInternalServerError)
		return
	}
	if g.clientOwners[id] || g.clientTransports[id] != "" || g.owners[id] == "" {
		g.mu.Unlock()
		http.NotFound(w, r)
		return
	}
	state := model.ProxyState{Listeners: g.managedListenersLocked(id, time.Now())}
	g.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(state)
}

// acknowledgeRetirementLocked removes an unregistered child's dependency after
// its signed policy read proves it durably applied that exact retirement
// revision. Directory removal, disconnects, and inspect-only reads never
// count. Caller holds g.mu.
func (g *Gateway) acknowledgeRetirementLocked(id string, proof model.MachineStateProof, state model.MachineState) error {
	d, ok := g.proxyDependencies[id]
	if !ok || !state.Unregistered || proof.InspectOnly || proof.Revision != state.Revision {
		return nil
	}
	delete(g.proxyDependencies, id)
	event := AuditEvent{At: time.Now().UTC(), UserID: d.UserID, Actor: "machine", Via: "retirement", Action: "proxy.dependency.retire", TargetID: id, Detail: "acknowledged retirement through " + d.URL}
	if err := g.commit(change{proxyDependencies: []string{id}, audit: []AuditEvent{event}}); err != nil {
		g.proxyDependencies[id] = d
		return err
	}
	g.notifyProxyWorkers(d.Worker)
	return nil
}

// releaseProxyDependency lets the fleet owner abandon waiting for an
// unregistered child that will never return to acknowledge retirement.
// Enrolled children and pending invitations cannot be released here.
func (g *Gateway) releaseProxyDependency(w http.ResponseWriter, r *http.Request) {
	p := g.requestPrincipal(r)
	id := r.PathValue("id")
	g.mu.Lock()
	defer g.mu.Unlock()
	d, ok := g.proxyDependencies[id]
	if !ok || d.UserID != p.UserID {
		http.NotFound(w, r)
		return
	}
	if !g.machineStates[id].Unregistered {
		http.Error(w, "only an unregistered child awaiting retirement acknowledgement can be released; unregister the machine first", http.StatusConflict)
		return
	}
	delete(g.proxyDependencies, id)
	if err := g.commit(change{proxyDependencies: []string{id}, audit: []AuditEvent{g.auditEventLocked(r, "", "proxy.dependency.release", "", id, "released unacknowledged retirement through "+d.URL)}}); err != nil {
		g.proxyDependencies[id] = d
		http.Error(w, "could not persist proxy dependency release", http.StatusInternalServerError)
		return
	}
	g.notifyProxyWorkers(d.Worker)
	w.WriteHeader(http.StatusNoContent)
}

type observedProxyInvitation struct {
	UserID, Worker, URL string
	ExpiresAt           time.Time
}

type observedProxyChild struct {
	UserID, Worker, URL string
	Retiring            bool
	Host                string
}

// managedProxyObservation is a committed projection protected by observationMu,
// never by the persistence lock during status reads.
type managedProxyObservation struct {
	invitations map[string]observedProxyInvitation
	children    map[string]observedProxyChild
}

// publishAllProxyObservations initializes the projection before serving requests.
func (g *Gateway) publishAllProxyObservations() {
	c := change{}
	for hash := range g.installations {
		c.invitations = append(c.invitations, hash)
	}
	for id := range g.proxyDependencies {
		c.proxyDependencies = append(c.proxyDependencies, id)
	}
	g.publishProxyObservations(c)
}

// publishProxyObservations publishes only changed records after a successful
// commit. Build records under mu, then hold observationMu only to replace them.
func (g *Gateway) publishProxyObservations(c change) {
	invitations := make(map[string]observedProxyInvitation, len(c.invitations))
	children := make(map[string]observedProxyChild)
	ids := append(slices.Clone(c.proxyDependencies), c.nodes...)
	ids = append(ids, c.machineStates...)
	for _, hash := range c.invitations {
		i, exists := g.installations[hash]
		if exists && i.RedeemedID != "" {
			ids = append(ids, i.RedeemedID)
		}
		if exists && i.GatewayProxy != nil && i.GatewayProxy.Managed && !i.Revoked && i.RedeemedID == "" && g.owners[i.GatewayProxy.Worker] == i.UserID {
			invitations[hash] = observedProxyInvitation{i.UserID, i.GatewayProxy.Worker, i.GatewayProxy.URL, i.ExpiresAt}
		}
	}
	for _, id := range ids {
		d, exists := g.proxyDependencies[id]
		if !exists || g.owners[d.Worker] != d.UserID {
			continue
		}
		retiring := g.machineStates[id].Unregistered
		_, registered := g.nodes[id]
		if retiring || registered || g.hasCredentialLocked(id) {
			children[id] = observedProxyChild{d.UserID, d.Worker, d.URL, retiring, g.proxyChildHostLocked(id, d)}
		}
	}
	if len(c.invitations) == 0 && len(ids) == 0 {
		return
	}
	g.observationMu.Lock()
	defer g.observationMu.Unlock()
	if g.proxyObservation.invitations == nil {
		g.proxyObservation.invitations = map[string]observedProxyInvitation{}
		g.proxyObservation.children = map[string]observedProxyChild{}
	}
	for _, hash := range c.invitations {
		delete(g.proxyObservation.invitations, hash)
		if i, ok := invitations[hash]; ok {
			g.proxyObservation.invitations[hash] = i
		}
	}
	for _, id := range ids {
		delete(g.proxyObservation.children, id)
		if d, ok := children[id]; ok {
			g.proxyObservation.children[id] = d
		}
	}
}

// proxyDependentsFor reads the committed projection without waiting for gateway
// persistence. Expiry is evaluated at read time, not by mutating the projection.
func (g *Gateway) proxyDependentsFor(userID string, entries []observation, now time.Time) map[string]map[string]*model.ProxyDependents {
	managed := false
	for _, entry := range entries {
		for _, endpoint := range entry.node.ProxyEndpoints {
			managed = managed || endpoint.Managed
		}
	}
	if !managed {
		return nil
	}
	out := map[string]map[string]*model.ProxyDependents{}
	hosts := map[[3]string]bool{}
	entry := func(worker, endpoint string) *model.ProxyDependents {
		if out[worker] == nil {
			out[worker] = map[string]*model.ProxyDependents{}
		}
		if out[worker][endpoint] == nil {
			out[worker][endpoint] = &model.ProxyDependents{}
		}
		return out[worker][endpoint]
	}
	g.observationMu.RLock()
	defer g.observationMu.RUnlock()
	for _, i := range g.proxyObservation.invitations {
		if i.UserID == userID && now.Before(i.ExpiresAt) {
			entry(i.Worker, i.URL).Invitations++
		}
	}
	for _, d := range g.proxyObservation.children {
		if d.UserID != userID {
			continue
		}
		if d.Retiring {
			entry(d.Worker, d.URL).Retiring++
		} else {
			count := entry(d.Worker, d.URL)
			count.Children++
			key := [3]string{d.Worker, d.URL, d.Host}
			if !hosts[key] {
				hosts[key] = true
				count.Machines++
			}
		}
	}
	return out
}
