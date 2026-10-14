package gateway

import (
	"sort"
	"time"

	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/update"
)

// Observation reads use a separately locked, committed view. Never take mu
// while holding observationMu, and never perform persistence under this lock.
type observation struct {
	node     model.Node
	health   *model.NodeHealth
	healthAt time.Time
	status   update.Status
}

type observedFleet struct {
	nodes map[string]observation
	order []string
}

// publishObservations updates only changed records after a successful commit,
// or after a volatile presence/health/policy acknowledgement. Caller holds mu.
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
		next := observation{node: n, status: g.updateStatus[id]}
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
	g.observationMu.RLock()
	defer g.observationMu.RUnlock()
	fleet := g.observed[user]
	if fleet == nil {
		return []model.Node{}
	}
	nodes := make([]model.Node, 0, len(fleet.order))
	for _, id := range fleet.order {
		nodes = append(nodes, fleet.nodes[id].node)
	}
	return nodes
}
