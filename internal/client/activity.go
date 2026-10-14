package client

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/koltyakov/control/internal/model"
)

// poolActivityCache reuses one peer observation for a few seconds. The
// dashboard refreshes gateway status on every poll, but fanning out an RPC
// to every node on sub-five-second intervals only repeats work; direct
// Activities calls are never cached.
type poolActivityCache struct {
	mu       sync.Mutex
	key      string
	snapshot []byte // Encoded once; callers never share mutable snapshot storage.
	at       time.Time
	pending  chan struct{}
}

const poolActivityTTL = 5 * time.Second

func (c Client) observedActivities(ctx context.Context, query model.PoolActivityQuery, scope string) (model.PoolActivitySnapshot, error) {
	if c.pool == nil {
		return c.Activities(ctx, query)
	}
	key := scope + "\x00" + c.URL + "\x00" + c.Token + "\x00" + string(model.JSON(query))
	for {
		if err := ctx.Err(); err != nil {
			return model.PoolActivitySnapshot{}, err
		}
		c.pool.mu.Lock()
		if c.pool.key == key && time.Since(c.pool.at) < poolActivityTTL {
			data := c.pool.snapshot
			c.pool.mu.Unlock()
			var snapshot model.PoolActivitySnapshot
			err := json.Unmarshal(data, &snapshot)
			return snapshot, err
		}
		if pending := c.pool.pending; pending != nil {
			c.pool.mu.Unlock()
			select {
			case <-pending:
				continue
			case <-ctx.Done():
				return model.PoolActivitySnapshot{}, ctx.Err()
			}
		}
		c.pool.pending = make(chan struct{})
		c.pool.mu.Unlock()
		break
	}
	started := time.Now()
	snapshot, err := c.Activities(ctx, query)
	var data []byte
	if err == nil {
		data, err = json.Marshal(snapshot)
	}
	c.pool.mu.Lock()
	if err == nil {
		c.pool.key, c.pool.snapshot, c.pool.at = key, data, started
	}
	close(c.pool.pending)
	c.pool.pending = nil
	c.pool.mu.Unlock()
	return snapshot, err
}

func (c Client) Activities(ctx context.Context, query model.PoolActivityQuery) (model.PoolActivitySnapshot, error) {
	var snapshot model.PoolActivitySnapshot
	err := c.Call(ctx, "", "activities.pool", query, &snapshot)
	return snapshot, err
}

// DashboardFresh bypasses peer observation reuse for an explicit refresh and
// expires the old cache so the next automatic poll cannot restore old detail.
func (c Client) DashboardFresh(ctx context.Context, gateway Admin, query model.PoolActivityQuery) (model.PoolActivitySnapshot, error) {
	if c.pool != nil {
		c.pool.mu.Lock()
		c.pool.at = time.Time{}
		c.pool.mu.Unlock()
	}
	c.pool = nil
	return c.Dashboard(ctx, gateway, query)
}

// Dashboard observes the gateway directly. Peer activity is optional enrichment;
// an unavailable execution backend does not hide a reachable gateway or directory.
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
	c.addTunnelCounts(&pool)
	for name, found := range selected {
		if !found {
			return pool, fmt.Errorf("unknown node %q", name)
		}
	}
	if len(pool.Nodes) == 0 {
		return pool, nil
	}
	for _, n := range pool.Nodes {
		if n.Online && n.Status != "summary" && n.Status != "update" {
			pool.Notice = "Live machine health unavailable; showing gateway presence and cached metrics."
			break
		}
	}
	if c.routing == nil && (c.URL == "" || c.Token == "") {
		return pool, nil
	}
	peerCtx, cancel := context.WithTimeout(ctx, 11*time.Second)
	defer cancel()
	observed, err := c.observedActivities(peerCtx, query, gateway.URL+"\x00"+gateway.Key)
	if err != nil {
		return pool, nil
	}
	byID := make(map[string]model.NodeActivitySnapshot, len(observed.Nodes))
	for _, n := range observed.Nodes {
		byID[n.ID] = n
	}
	missing := false
	for i, n := range pool.Nodes {
		if n.Status == "update" {
			continue // Peer observations must not replace gateway update status.
		}
		if live, ok := byID[n.ID]; ok && n.Online && live.Online && (live.Status == "ready" || n.Status != "summary") {
			// The directly authenticated directory defines fleet membership and
			// identity metadata, even if this CLI targets another node's API.
			live.Name, live.OS, live.Labels, live.LastSeen = n.Name, n.OS, n.Labels, n.LastSeen
			live.Software, live.Disabled, live.ControlPending = n.Software, n.Disabled, n.ControlPending
			live.RetainedTunnels = n.RetainedTunnels
			if n.Status == "summary" && n.ObservedAt.After(live.ObservedAt) {
				// Cached detail must not replace a more recent health aggregate.
				live.ActiveCount, live.Leased, live.System = n.ActiveCount, n.Leased, n.System
			}
			if live.Status == "ready" && live.Tunnels == nil {
				live.Tunnels = model.ActiveTunnelCounts(live.Active)
			}
			pool.Nodes[i] = live
		} else if n.Online && n.Status != "summary" {
			missing = true
		} else if live, ok := byID[n.ID]; ok && n.Online && live.Error != "" {
			// Fresh health describes work, not reachability from this client.
			pool.Nodes[i].Error = live.Error
		}
	}
	if !missing {
		pool.Notice = ""
	}
	return pool, nil
}
