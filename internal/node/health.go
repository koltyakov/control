package node

import (
	"bytes"
	"context"
	"sync"
	"time"

	"github.com/koltyakov/control/internal/model"
)

func (n *Node) healthSnapshot() model.NodeHealth {
	health := model.NodeHealth{System: n.system.Snapshot()}
	// Kinds and times only: operation names, owners, and peers stay on the node.
	work := model.WorkCounts{}
	var active []model.Activity
	n.activityMu.Lock()
	health.ActiveCount = len(n.activities) + n.untrackedActivities
	work.Other = n.untrackedActivities
	for _, h := range n.activities {
		a := h.snapshot()
		active = append(active, a)
		health.LastActive = later(health.LastActive, a.Started)
		switch a.Kind {
		case "exec":
			work.Exec++
		case "transfer":
			work.Transfers++
		case "tunnel":
		default:
			work.Other++
		}
	}
	for _, a := range n.recentActivities {
		health.LastActive = later(health.LastActive, a.Finished)
	}
	n.activityMu.Unlock()
	n.mu.Lock()
	// Non-terminal tasks are exactly those with a cancel function.
	health.ActiveCount += len(n.cancels)
	work.Tasks = len(n.cancels)
	for id := range n.cancels {
		if task := n.tasks[id]; task != nil {
			health.LastActive = later(health.LastActive, task.Created)
		}
	}
	for _, a := range n.recentTasks {
		health.LastActive = later(health.LastActive, a.Finished)
	}
	health.Leased = n.lease != nil && (time.Now().Before(n.lease.Expires) || n.leaseBusy())
	if health.Leased {
		health.LeaseExpires = n.lease.Expires
	}
	n.mu.Unlock()
	if n.proxyChecks != nil {
		health.Proxies = n.proxyChecks.snapshot(time.Now())
	}
	routes := n.Peer.ConnectionRoutes()
	health.Work, health.Tunnels, health.ConnectionRoutes = &work, model.ActiveTunnelCounts(active), &routes
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

func later(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

// healthReport is the last health report accepted for sending.
type healthReport struct {
	mu      sync.Mutex
	data    []byte
	session uint64
	at      time.Time
}

// unchanged reports whether data repeats the last report on this gateway
// connection recently enough that the gateway still treats it as fresh.
func (r *healthReport) unchanged(data []byte, session uint64, now time.Time) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	// Ticks are not exact; resend a little early rather than one tick late.
	return session == r.session && bytes.Equal(data, r.data) && now.Sub(r.at) < model.UnchangedTelemetryInterval-model.NodeHealthInterval/2
}

func (r *healthReport) sent(data []byte, session uint64, now time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.data, r.session, r.at = data, session, now
}

func (n *Node) reportHealth(ctx context.Context) {
	if !n.Peer.SupportsNodeHealth() {
		return
	}
	data := model.JSON(n.healthSnapshot())
	if len(data) > model.MaxNodeHealthBytes {
		return
	}
	// Older gateways need every interval's report to keep health fresh.
	relaxed := n.Peer.TelemetryLiveness()
	session, now := n.Peer.GatewaySession(), time.Now()
	if relaxed && n.lastHealth.unchanged(data, session, now) {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, model.NodeHealthInterval)
	defer cancel()
	if n.Peer.SendControl(ctx, "node.health", data) == nil {
		n.lastHealth.sent(data, session, now)
	}
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
