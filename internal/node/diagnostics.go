package node

import "context"

// diagnostics reports bounded operational counts, never owner records, task
// arguments, credentials, logs or local paths. It uses cached resource data.
func (n *Node) diagnostics(ctx context.Context) map[string]any {
	// A stale reading is refreshed within diskGuardProbeWait, outside n.mu.
	taskDisk := n.taskDiskDiagnostics(ctx)
	n.mu.Lock()
	tasks, pending := len(n.tasks), 0
	for _, t := range n.tombstones {
		if t.CleanupPending {
			pending++
		}
	}
	tombstones, active := len(n.tombstones), len(n.cancels)
	n.mu.Unlock()
	n.artifactBudgetMu.Lock()
	used, staging := n.artifactUsed, len(n.artifactStaging)
	n.artifactBudgetMu.Unlock()
	n.mcpMu.Lock()
	sessions, starting := len(n.mcpSessions), len(n.mcpPending)
	n.mcpMu.Unlock()
	return map[string]any{
		"tasks":     map[string]any{"retained": tasks, "active": active, "tombstones": tombstones, "cleanupPending": pending},
		"artifacts": map[string]any{"usedBytes": used, "maxBytes": n.Config.MaxArtifactBytes, "reserveBytes": n.Config.ArtifactReserveBytes, "retentionHours": n.Config.ArtifactRetentionHours, "imports": staging},
		"mcp":       map[string]int{"sessions": sessions, "starting": starting},
		"taskDisk":  taskDisk,
		"transport": n.Peer.Stats(), "sessions": n.Peer.SessionStats(), "system": n.healthSnapshot().System,
	}
}
