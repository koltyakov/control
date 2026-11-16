package gateway

import (
	"crypto/sha256"
	"sort"
	"strings"
	"time"

	"github.com/koltyakov/control/internal/identity"
	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/update"
)

// Observation reads use a separately locked, committed view. Never take mu
// while holding observationMu, and never perform persistence under this lock.
type observation struct {
	node     model.Node
	nodeHash [sha256.Size]byte
	health   *model.NodeHealth
	healthAt time.Time
	status   update.Status
}

type observedFleet struct {
	nodes map[string]observation
	order []string
	etag  string
}

// publishObservations updates only changed records after a successful commit,
// or after a volatile presence/policy acknowledgement. Caller holds mu.
func (g *Gateway) publishObservations(ids ...string) {
	g.observationMu.Lock()
	defer g.observationMu.Unlock()
	reordered := make(map[*observedFleet]bool)
	for _, id := range ids {
		n, exists := g.nodes[id]
		user := g.owners[id] // Immutable, including after registration deletion.
		fleet := g.observed[user]
		if fleet == nil {
			if !exists {
				continue
			}
			fleet = &observedFleet{nodes: make(map[string]observation)}
			g.observed[user] = fleet
		}
		previous, had := fleet.nodes[id]
		if !exists {
			if had {
				fleet.etag = `"` + identity.NewID() + `"`
			}
			delete(fleet.nodes, id)
			for i, entry := range fleet.order {
				if entry == id {
					fleet.order = append(fleet.order[:i], fleet.order[i+1:]...)
					break
				}
			}
			continue
		}
		n.ControlPending = g.machinePending(id)
		// Hash the serialized directory record, not health or rollout status.
		// This also detects edits to nested maps/slices shared by model values.
		next := observation{node: n, nodeHash: sha256.Sum256(model.JSON(n)), status: g.updateStatus[id]}
		if !had || previous.nodeHash != next.nodeHash {
			fleet.etag = `"` + identity.NewID() + `"`
		}
		if c := g.peers[id]; c != nil {
			next.health, next.healthAt = c.health, c.healthAt
		}
		fleet.nodes[id] = next
		if !had {
			fleet.order = append(fleet.order, id)
		}
		if !had || previous.node.Name != n.Name {
			reordered[fleet] = true
		}
	}
	for fleet := range reordered {
		sort.Slice(fleet.order, func(i, j int) bool {
			return nodeNameLess(fleet.nodes[fleet.order[i]].node.Name, fleet.nodes[fleet.order[j]].node.Name)
		})
	}
}

// publishTelemetry updates health and rollout status without re-reading or
// hashing directory records. Caller holds mu. Directory changes, including
// UpdateUntil and ControlPending, must use the full publication path first.
func (g *Gateway) publishTelemetry(id string) {
	g.observationMu.Lock()
	defer g.observationMu.Unlock()
	fleet := g.observed[g.owners[id]]
	if fleet == nil {
		return
	}
	entry, exists := fleet.nodes[id]
	if !exists {
		return
	}
	entry.status = g.updateStatus[id]
	entry.health, entry.healthAt = nil, time.Time{}
	if c := g.peers[id]; c != nil {
		entry.health, entry.healthAt = c.health, c.healthAt
	}
	fleet.nodes[id] = entry
}

func (g *Gateway) observations(user string) []observation {
	g.observationMu.RLock()
	defer g.observationMu.RUnlock()
	fleet := g.observed[user]
	if fleet == nil {
		return []observation{}
	}
	result := make([]observation, 0, len(fleet.order))
	for _, id := range fleet.order {
		result = append(result, fleet.nodes[id])
	}
	return result
}

func (g *Gateway) directory(user string) []model.Node {
	nodes, _, _ := g.conditionalDirectory(user, "")
	return nodes
}

// conditionalDirectory checks the validator and copies the same committed view
// under one lock. It retains no per-request or per-client directory bodies.
func (g *Gateway) conditionalDirectory(user, condition string) ([]model.Node, string, bool) {
	g.observationMu.RLock()
	defer g.observationMu.RUnlock()
	fleet := g.observed[user]
	if fleet == nil {
		// The empty representation exists even before any node is published,
		// but no per-account validator needs to be retained for it.
		return []model.Node{}, "", strings.TrimSpace(condition) == "*"
	}
	if matchesDirectoryETag(condition, fleet.etag) {
		return nil, fleet.etag, true
	}
	nodes := make([]model.Node, 0, len(fleet.order))
	for _, id := range fleet.order {
		n := fleet.nodes[id].node
		n.GatewayProxy = nil // Configured gateway routes are owner-only metadata.
		nodes = append(nodes, n)
	}
	return nodes, fleet.etag, false
}

func matchesDirectoryETag(condition, etag string) bool {
	if etag == "" {
		return false
	}
	for value := range strings.SplitSeq(condition, ",") {
		value = strings.TrimSpace(value)
		if value == "*" || strings.TrimPrefix(value, "W/") == etag {
			return true
		}
	}
	return false
}
