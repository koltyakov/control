package node

import (
	"context"
	"time"

	"github.com/koltyakov/control/internal/model"
)

func (n *Node) healthSnapshot() model.NodeHealth {
	health := model.NodeHealth{System: n.system.Snapshot()}
	n.activityMu.Lock()
	health.ActiveCount = len(n.activities) + n.untrackedActivities
	n.activityMu.Unlock()
	n.mu.Lock()
	for _, task := range n.tasks {
		if !task.Terminal() {
			health.ActiveCount++
		}
	}
	health.Leased = n.lease != nil && (time.Now().Before(n.lease.Expires) || n.leaseBusy())
	n.mu.Unlock()
	for i := range health.System.Disks {
		health.System.Disks[i].Path = "work"
		if i > 0 {
			health.System.Disks[i].Path = "data"
		}
		if health.System.Disks[i].Error != "" {
			health.System.Disks[i].Error = "disk sample unavailable"
		}
	}
	if len(health.System.Errors) > 0 {
		health.System.Errors = []string{"some machine resource samples are unavailable"}
	}
	return health
}

func (n *Node) reportHealth(ctx context.Context) {
	if !n.Peer.SupportsNodeHealth() {
		return
	}
	data := model.JSON(n.healthSnapshot())
	if len(data) > model.MaxNodeHealthBytes {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, model.NodeHealthInterval)
	defer cancel()
	_ = n.Peer.SendControl(ctx, "node.health", data)
}

func (n *Node) runHealth(ctx context.Context) {
	ticker := time.NewTicker(model.NodeHealthInterval)
	defer ticker.Stop()
	for {
		n.reportHealth(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
