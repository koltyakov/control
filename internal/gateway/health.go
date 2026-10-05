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
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&health) != nil || decoder.Decode(new(any)) != io.EOF || health.ActiveCount < 0 || health.System.OS != n.OS || health.System.Arch != arch || len(health.System.Disks) > 2 {
		return false
	}
	if cpu := health.System.CPUUsagePercent; cpu != nil && (*cpu < 0 || *cpu > 100) {
		return false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.peers[n.ID] != c {
		return false
	}
	c.health, c.healthAt = &health, time.Now().UTC()
	return true
}
