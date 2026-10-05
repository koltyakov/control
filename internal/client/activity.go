package client

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/koltyakov/control/internal/model"
)

func (c Client) Activities(ctx context.Context, query model.PoolActivityQuery) (model.PoolActivitySnapshot, error) {
	var snapshot model.PoolActivitySnapshot
	err := c.Call(ctx, "", "activities.pool", query, &snapshot)
	return snapshot, err
}

// Dashboard observes the gateway directly. Peer activity is optional enrichment;
// an unavailable local node does not hide a reachable gateway or its directory.
func (c Client) Dashboard(ctx context.Context, gateway Admin, query model.PoolActivityQuery) (model.PoolActivitySnapshot, error) {
	var pool model.PoolActivitySnapshot
	if err := gateway.JSON(ctx, http.MethodGet, "/v1/status", nil, &pool); err != nil {
		return pool, fmt.Errorf("gateway status: %w", err)
	}
	if pool.Gateway == nil {
		return pool, fmt.Errorf("gateway did not return status metadata; upgrade the gateway")
	}
	pool.Gateway.URL = strings.TrimRight(gateway.URL, "/")
	selected := make(map[string]bool, len(query.Nodes))
	for _, name := range query.Nodes {
		selected[name] = false
	}
	filtered := make([]model.NodeActivitySnapshot, 0, len(pool.Nodes))
	for _, n := range pool.Nodes {
		_, byID := selected[n.ID]
		_, byName := selected[n.Name]
		if len(selected) > 0 && !byID && !byName {
			continue
		}
		if byID {
			selected[n.ID] = true
		}
		if byName {
			selected[n.Name] = true
		}
		filtered = append(filtered, n)
	}
	pool.Nodes = filtered
	for name, found := range selected {
		if !found {
			return pool, fmt.Errorf("unknown node %q", name)
		}
	}
	if len(pool.Nodes) == 0 {
		return pool, nil
	}
	for _, n := range pool.Nodes {
		if n.Online && n.Status != "summary" {
			pool.Notice = "Live machine health unavailable; showing gateway presence and cached metrics."
			break
		}
	}
	if c.URL == "" || c.Token == "" {
		return pool, nil
	}
	peerCtx, cancel := context.WithTimeout(ctx, 11*time.Second)
	defer cancel()
	observed, err := c.Activities(peerCtx, query)
	if err != nil {
		return pool, nil
	}
	byID := make(map[string]model.NodeActivitySnapshot, len(observed.Nodes))
	for _, n := range observed.Nodes {
		byID[n.ID] = n
	}
	missing := false
	for i, n := range pool.Nodes {
		if live, ok := byID[n.ID]; ok && n.Online && live.Online && (live.Status == "ready" || n.Status != "summary") {
			// The directly authenticated directory defines fleet membership and
			// identity metadata, even if this CLI targets another node's API.
			live.Name, live.OS, live.Labels, live.LastSeen = n.Name, n.OS, n.Labels, n.LastSeen
			live.Software, live.Disabled, live.ControlPending = n.Software, n.Disabled, n.ControlPending
			pool.Nodes[i] = live
		} else if n.Online && n.Status != "summary" {
			missing = true
		}
	}
	if !missing {
		pool.Notice = ""
	}
	return pool, nil
}
