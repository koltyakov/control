package gateway

import (
	"encoding/json"
	"net/http"
	"sort"
	"time"

	"github.com/koltyakov/control/internal/buildinfo"
	"github.com/koltyakov/control/internal/model"
)

// status reads cached resource samples and only the authenticated user's fleet.
func (g *Gateway) status(w http.ResponseWriter, r *http.Request) {
	p := g.authenticate(bearer(r))
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
	g.mu.Lock()
	for _, n := range g.nodes {
		if n.UserID != p.UserID {
			continue
		}
		state := "offline"
		if n.Online {
			state = "online" // Directory presence does not imply observed activity.
		}
		snapshot := model.NodeActivitySnapshot{
			ID: n.ID, Name: n.Name, OS: n.OS, Labels: n.Labels, Online: n.Online,
			LastSeen: n.LastSeen, Status: state, System: n.System,
			Software: n.Software, Disabled: n.Disabled, ControlPending: g.machinePending(n.ID),
			Active: []model.Activity{}, Recent: []model.Activity{},
		}
		// Aggregate health is owner-only. Common peer keys still need the node's
		// activities.list/system.info permissions for live observation.
		if c := g.peers[n.ID]; n.Online && c != nil && c.health != nil && (p.Role == "user" || p.Role == "superuser") && time.Since(c.healthAt) < model.NodeHealthTTL {
			snapshot.Status, snapshot.ObservedAt = "summary", c.healthAt
			snapshot.ActiveCount, snapshot.Leased = c.health.ActiveCount, c.health.Leased
			snapshot.System = &c.health.System
		}
		if g.nodeUpdating(n, result.ObservedAt) {
			snapshot.Status = "update"
		}
		result.Nodes = append(result.Nodes, snapshot)
	}
	g.mu.Unlock()
	sort.Slice(result.Nodes, func(i, j int) bool { return nodeNameLess(result.Nodes[i].Name, result.Nodes[j].Name) })
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(result)
}
