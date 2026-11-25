package gateway

import (
	"encoding/json"
	"maps"
	"net/http"
	"slices"
	"strings"

	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/protocol"
	"github.com/koltyakov/control/internal/update"
)

// machineLabelSchema stores fleet owners' label overlays by machine identity.
// The node's own registration, including its signed labels, stays in nodes.
const machineLabelSchema = `
CREATE TABLE machine_labels (id TEXT PRIMARY KEY REFERENCES identities(id), user_id TEXT NOT NULL REFERENCES users(id), data BLOB NOT NULL);
`

// labelOverlay is one machine's owner overlay and the fleet that set it.
type labelOverlay struct {
	UserID string `json:"userId"`
	model.LabelOverlay
}

// stripOwnerLabels removes gateway-owned label fields from a registration.
// A node cannot assert that its labels were set by the fleet owner.
func stripOwnerLabels(n *model.Node) {
	n.OwnerLabels, n.RemovedLabels, n.NodeLabels = nil, nil, nil
}

// effectiveNodeLocked applies the owner's overlay to a directory copy of a
// registration. g.nodes keeps exactly the node-reported labels. Caller holds mu.
func (g *Gateway) effectiveNodeLocked(n model.Node) model.Node {
	stripOwnerLabels(&n)
	overlay, ok := g.labelOverlays[n.ID]
	if !ok || overlay.UserID != n.UserID || overlay.Empty() {
		return n
	}
	n.NodeLabels = n.Labels
	n.Labels = overlay.Apply(n.Labels)
	n.OwnerLabels = maps.Clone(overlay.Set)
	n.RemovedLabels = slices.Clone(overlay.Remove)
	return n
}

// setMachineLabels applies an owner's validated label patch to one machine.
// Labels are discovery and selection metadata; they grant no authority and
// need no node acknowledgement. Hybrid contexts keep separate labels.
// Called under g.mu after the fleet ownership check.
func (g *Gateway) setMachineLabels(w http.ResponseWriter, r *http.Request, n model.Node, patch model.LabelPatch) {
	stored, existed := g.labelOverlays[n.ID]
	current := stored.LabelOverlay
	if existed && stored.UserID != n.UserID {
		// Identity ownership is immutable; never carry another fleet's entry.
		current = model.LabelOverlay{}
	}
	next, err := current.Patch(patch)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	unchanged := !existed && next.Empty() || existed && stored.UserID == n.UserID && sameOverlay(current, next)
	if unchanged {
		writeLabelView(w, g.effectiveNodeLocked(n))
		return
	}
	if next.Empty() {
		delete(g.labelOverlays, n.ID)
	} else {
		g.labelOverlays[n.ID] = labelOverlay{UserID: n.UserID, LabelOverlay: next}
	}
	event := g.auditEventLocked(r, "", "machine.labels", n.Name, n.ID, labelPatchDetail(patch))
	// The commit publishes the effective record to the observation view only
	// after the overlay and its audit event are durable.
	if err := g.commit(change{labels: []string{n.ID}, audit: []AuditEvent{event}}); err != nil {
		if existed {
			g.labelOverlays[n.ID] = stored
		} else {
			delete(g.labelOverlays, n.ID)
		}
		http.Error(w, "could not persist machine labels", http.StatusInternalServerError)
		return
	}
	g.notifyDirectoryChangedLocked(n.UserID)
	writeLabelView(w, g.effectiveNodeLocked(n))
}

// notifyDirectoryChangedLocked asks connected fleet peers to drop cached
// lookups, as a rename does. Caller holds g.mu, after commit.
func (g *Gateway) notifyDirectoryChangedLocked(user string) {
	for _, peer := range g.peers {
		if peer.userID != user {
			continue
		}
		// The writer releases every queued packet's reservation, including
		// metadata-only notifications. Never publish directly to its channel.
		if !peer.enqueue(&protocol.Packet{Kind: "directory.changed", From: update.GatewaySender}) {
			_ = peer.ws.CloseNow()
		}
	}
}

// countSet counts present fields of a request that accepts exactly one.
func countSet(present ...bool) int {
	count := 0
	for _, p := range present {
		if p {
			count++
		}
	}
	return count
}

func sameOverlay(a, b model.LabelOverlay) bool {
	return maps.Equal(a.Set, b.Set) && slices.Equal(a.Remove, b.Remove)
}

func writeLabelView(w http.ResponseWriter, n model.Node) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(model.LabelView(n))
}

// labelPatchDetail summarizes a patch for the audit log. Labels are not
// secrets, and values are bounded by validation.
func labelPatchDetail(p model.LabelPatch) string {
	parts := []string{}
	for _, key := range slices.Sorted(maps.Keys(p.Set)) {
		parts = append(parts, "set "+key+"="+p.Set[key])
	}
	for _, key := range slices.Sorted(slices.Values(p.Remove)) {
		parts = append(parts, "remove "+key)
	}
	for _, key := range slices.Sorted(slices.Values(p.Reset)) {
		parts = append(parts, "reset "+key)
	}
	return strings.Join(parts, ", ")
}
