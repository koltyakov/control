package gateway

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/koltyakov/control/internal/buildinfo"
	"github.com/koltyakov/control/internal/model"
)

// status reads cached resource samples and only the authenticated user's fleet.
func (g *Gateway) status(w http.ResponseWriter, r *http.Request) {
	p := g.requestPrincipal(r)
	if p.Role == "" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	info := g.metrics.Snapshot()
	// Report capacity without exposing the gateway's private state path or
	// filesystem errors to fleet members.
	for i := range info.Disks {
		info.Disks[i].Path = "state"
		if info.Disks[i].Error != "" {
			info.Disks[i].Error = "disk sample unavailable"
		}
	}
	if len(info.Errors) > 0 {
		info.Errors = []string{"some gateway resource samples are unavailable"}
	}
	software := g.options.Software
	if software.Version == "" {
		software = buildinfo.Current()
	}
	result := model.PoolActivitySnapshot{
		ObservedAt: time.Now().UTC(),
		Gateway:    &model.GatewaySnapshot{URL: g.options.PublicURL, Software: software, StartedAt: g.startedAt, System: &info},
		Nodes:      []model.NodeActivitySnapshot{},
	}
	// Read committed observation data without waiting behind SQLite writes.
	deployment := g.updates.Current()
	entries := g.observations(p.UserID)
	fleet := make([]model.Node, 0, len(entries))
	for _, entry := range entries {
		fleet = append(fleet, entry.node)
	}
	for _, entry := range entries {
		n := entry.node
		state := "offline"
		if n.Online {
			state = "online" // Directory presence does not imply observed activity.
		}
		snapshot := model.NodeActivitySnapshot{
			ID: n.ID, Name: n.Name, OS: n.OS, Labels: n.Labels, Online: n.Online,
			LastSeen: n.LastSeen, Status: state, System: n.System,
			Software: n.Software, Disabled: n.Disabled, ControlPending: n.ControlPending, RunAs: n.RunAs,
			HybridID: n.HybridID, HybridRole: n.HybridRole,
			Active: []model.Activity{}, Recent: []model.Activity{},
		}
		if sender, ok := wakeSender(fleet, n); ok {
			snapshot.WakeVia = sender.Name
		}
		// Aggregate health is owner-only. Common peer keys still need the node's
		// activities.list/system.info permissions for live observation.
		if n.Online && entry.health != nil && (p.Role == "user" || p.Role == "superuser") && time.Since(entry.healthAt) < model.NodeHealthTTL {
			snapshot.Status, snapshot.ObservedAt = "summary", entry.healthAt
			snapshot.ActiveCount, snapshot.Leased = entry.health.ActiveCount, entry.health.Leased
			snapshot.System = &entry.health.System
			snapshot.Work, snapshot.Tunnels, snapshot.LeaseExpires = entry.health.Work, entry.health.Tunnels, entry.health.LeaseExpires
			snapshot.LastActive = entry.health.LastActive
			if routes := entry.health.ConnectionRoutes; routes != nil {
				snapshot.ConnectionRoutes = routes
				snapshot.DirectSessions = routes.Direct + routes.TURN + routes.Unknown
				snapshot.RelaySessions = routes.Relay
			}
		}
		if nodeUpdating(n, entry.status, result.ObservedAt, deployment) {
			snapshot.Status = "update"
		}
		result.Nodes = append(result.Nodes, snapshot)
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(result)
}
