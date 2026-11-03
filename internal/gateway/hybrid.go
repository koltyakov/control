package gateway

import (
	"context"
	"log/slog"
	"maps"
	"time"

	"github.com/koltyakov/control/internal/model"
)

const hybridCleanupInterval = 30 * time.Second

func (i installation) pendingHybrid() bool {
	return i.HybridID != "" && !i.HybridComplete
}

// Called under g.mu after staging a registration. Completion commits with the
// second node, never with redemption alone. The returned originals allow rollback.
func (g *Gateway) completeHybrid(userID, hybridID string) map[string]installation {
	previous := map[string]installation{}
	if hybridID == "" {
		return previous
	}
	roles := map[string]bool{}
	for hash, i := range g.installations {
		if i.UserID != userID || i.HybridID != hybridID || i.Revoked {
			continue
		}
		n, ok := g.nodes[i.RedeemedID]
		if !ok || n.UserID != userID || n.HybridID != hybridID || n.HybridRole != i.HybridRole {
			return map[string]installation{}
		}
		roles[i.HybridRole] = true
		previous[hash] = i
	}
	if len(previous) != 2 || !roles["user"] || !roles["system"] {
		return map[string]installation{}
	}
	for hash, i := range previous {
		if i.HybridComplete {
			delete(previous, hash)
			continue
		}
		i.HybridComplete = true
		g.installations[hash] = i
	}
	return previous
}

// Expire the whole incomplete pair in one transaction. Permanent identity and
// retirement records remain so delayed installers cannot resurrect stale nodes.
func (g *Gateway) cleanupHybridInstallations(now time.Time) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	previous := map[string]installation{}
	nodes := map[string]model.Node{}
	states := map[string]model.MachineState{}
	persisted := change{}
	type pair struct{ userID, hybridID string }
	expired := map[pair]bool{}
	active := map[string]string{}
	for _, i := range g.installations {
		if i.pendingHybrid() && !now.Before(i.ExpiresAt) {
			expired[pair{i.UserID, i.HybridID}] = true
		}
		if i.RedeemedID != "" && !i.Revoked {
			active[i.RedeemedID] = i.HybridID
		}
	}
	for hash, i := range g.installations {
		if !i.pendingHybrid() || !expired[pair{i.UserID, i.HybridID}] {
			continue
		}
		previous[hash] = i
		delete(g.installations, hash)
		persisted.invitations = append(persisted.invitations, hash)
		id := i.RedeemedID
		if id == "" {
			continue
		}
		// A fresh enrollment superseding this attempt owns the identity now.
		if current, ok := active[id]; ok && current != i.HybridID {
			continue
		}
		if _, processed := states[id]; processed {
			continue
		}
		if n, ok := g.nodes[id]; ok {
			if n.UserID != i.UserID || (i.Revoked && n.HybridID != i.HybridID) {
				continue
			}
			nodes[id] = n
			delete(g.nodes, id)
		}
		states[id] = g.machineStates[id]
		g.machineStates[id] = model.MachineState{Revision: states[id].Revision + 1, Disabled: true, Unregistered: true}
		persisted.nodes = append(persisted.nodes, id)
		persisted.machineStates = append(persisted.machineStates, id)
		persisted.audit = append(persisted.audit, AuditEvent{At: now.UTC(), UserID: i.UserID, Actor: "gateway", Via: "timeout", Action: "machine.unregister", Target: i.Name, TargetID: id, Detail: "incomplete hybrid installation " + i.HybridID})
	}
	if len(previous) == 0 {
		return nil
	}
	if err := g.commit(persisted); err != nil {
		maps.Copy(g.installations, previous)
		maps.Copy(g.nodes, nodes)
		for id, state := range states {
			if state == (model.MachineState{}) {
				delete(g.machineStates, id)
			} else {
				g.machineStates[id] = state
			}
		}
		return err
	}
	for _, id := range persisted.nodes {
		delete(g.updateStatus, id)
		if peer := g.peers[id]; peer != nil {
			_ = peer.ws.CloseNow()
		}
	}
	return nil
}

func (g *Gateway) runHybridCleanup(ctx context.Context) {
	ticker := time.NewTicker(hybridCleanupInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			if err := g.cleanupHybridInstallations(now); err != nil {
				slog.Warn("clean up incomplete hybrid installations", "error", err)
			}
		}
	}
}
