package gateway

import (
	"bytes"
	"encoding/json"
	"io"
	"time"

	"github.com/koltyakov/control/internal/model"
)

func (g *Gateway) receiveHealth(n model.Node, c *connection, data []byte) bool {
	if len(data) > model.MaxNodeHealthBytes {
		return false
	}
	var health model.NodeHealth
	arch := n.Software.Arch
	if arch == "" && n.System != nil {
		arch = n.System.Arch
	}
	// Unknown fields from newer nodes are ignored rather than rejected: only
	// the known aggregate fields are decoded and retained, and rejecting them
	// would disconnect an upgraded node from an older gateway every interval.
	decoder := json.NewDecoder(bytes.NewReader(data))
	if decoder.Decode(&health) != nil || decoder.Decode(new(any)) != io.EOF || !health.Valid() || health.System.OS != n.OS || health.System.Arch != arch || len(health.System.Disks) > 2 {
		return false
	}
	if !model.ValidProxyReadiness(health.Proxies) {
		return false
	}
	if cpu := health.System.CPUUsagePercent; cpu != nil && (*cpu < 0 || *cpu > 100) {
		return false
	}
	if len(health.System.GPUs) > model.MaxGPUs {
		return false
	}
	for _, gpu := range health.System.GPUs {
		if !gpu.Valid() {
			return false
		}
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.peers[n.ID] != c {
		return false
	}
	health.Proxies = advertisedReadiness(g.nodes[n.ID], health.Proxies)
	c.health, c.healthAt = &health, time.Now().UTC()
	c.healthReceived = c.healthAt
	g.publishTelemetry(n.ID)
	return true
}

// telemetryReportLimit bounds how long heartbeat echoes may confirm one
// report. Nodes negotiating telemetryLiveness resend unchanged reports every
// model.UnchangedTelemetryInterval; a node that stops reporting entirely, for
// example with a stalled updater, still goes stale after this limit.
const telemetryReportLimit = model.UnchangedTelemetryInterval + 15*time.Second

// refreshTelemetry runs when a node echoes the outstanding heartbeat
// challenge. The echo proves that this registered connection is alive, and
// the node sends any change to health or update status as soon as it observes
// it, in order on the same socket. Reports received on this connection within
// telemetryReportLimit therefore remain current, so nodes negotiating
// telemetryLiveness can resend unchanged reports rarely. A report from a
// previous connection is never confirmed.
func (g *Gateway) refreshTelemetry(id string, c *connection) {
	now := time.Now().UTC()
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.peers[id] != c || c.client {
		return
	}
	changed := false
	if c.health != nil && now.Sub(c.healthReceived) < telemetryReportLimit {
		c.healthAt, changed = now, true
	}
	if status, ok := g.updateStatus[id]; ok && !c.statusAt.IsZero() && now.Sub(c.statusAt) < telemetryReportLimit {
		status.SeenAt = now
		g.updateStatus[id], changed = status, true
	}
	if changed {
		g.publishTelemetry(id)
	}
}
