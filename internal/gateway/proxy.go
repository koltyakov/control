package gateway

import (
	"net/http"
	"time"

	"github.com/koltyakov/control/internal/model"
)

// proxySelectionLocked validates an invitation's proxy worker and endpoint
// against the requesting fleet's live registrations. The gateway never probes
// the endpoint: only the worker's own fresh readiness report qualifies it.
// An empty endpoint selects the worker's only advertised endpoint. On success
// the string is the endpoint's current readiness state. Caller holds g.mu.
func (g *Gateway) proxySelectionLocked(userID, worker, endpoint string, now time.Time) (*model.GatewayProxy, string, int) {
	n, exists := g.nodes[worker]
	c := g.peers[worker]
	if !exists || n.UserID != userID || g.owners[worker] != userID {
		return nil, "proxy worker is not a machine in this fleet", http.StatusNotFound
	}
	if c == nil || c.client || !n.Online {
		return nil, "proxy worker is offline", http.StatusConflict
	}
	if n.Disabled || g.machineStates[worker].Disabled || g.machineStates[worker].Unregistered {
		return nil, "proxy worker is disabled or unregistered", http.StatusConflict
	}
	if len(n.ProxyEndpoints) == 0 {
		return nil, "proxy worker advertises no proxy endpoints; configure proxyEndpoints on that worker", http.StatusConflict
	}
	if endpoint == "" {
		if len(n.ProxyEndpoints) != 1 {
			return nil, "proxy worker advertises several endpoints; select one explicitly", http.StatusBadRequest
		}
		endpoint = n.ProxyEndpoints[0].URL
	}
	var readiness []model.ProxyReadiness
	if c.health != nil {
		readiness = c.health.Proxies
	}
	for _, status := range model.ProxyStatuses(n.ProxyEndpoints, true, readiness, c.healthAt, now) {
		if status.URL != endpoint {
			continue
		}
		// A managed listener on standby has freshly verified its direct gateway
		// path; this invitation becomes the dependency that activates it.
		if !status.Selectable() {
			text := "proxy endpoint is " + status.State
			if status.Error != "" {
				text += ": " + status.Error
			}
			return nil, text + "; wait for a fresh ready check on the proxy worker", http.StatusConflict
		}
		return &model.GatewayProxy{URL: endpoint, Worker: worker, Managed: status.Managed}, status.State, 0
	}
	return nil, "endpoint is not advertised by the proxy worker", http.StatusBadRequest
}

// proxyStatuses projects a node's advertised endpoints for the fleet owner.
// Managed endpoints include their durable dependency counts.
func proxyStatuses(entry observation, now time.Time, dependents map[string]*model.ProxyDependents) []model.ProxyEndpointStatus {
	var readiness []model.ProxyReadiness
	if entry.health != nil {
		readiness = entry.health.Proxies
	}
	statuses := model.ProxyStatuses(entry.node.ProxyEndpoints, entry.node.Online, readiness, entry.healthAt, now)
	for i := range statuses {
		if statuses[i].Managed {
			count := model.ProxyDependents{}
			if d := dependents[statuses[i].URL]; d != nil {
				count = *d
			}
			statuses[i].Dependents = &count
		}
	}
	return statuses
}

// advertisedReadiness keeps only reports for endpoints in the registration.
func advertisedReadiness(n model.Node, readiness []model.ProxyReadiness) []model.ProxyReadiness {
	var kept []model.ProxyReadiness
	for _, r := range readiness {
		for _, endpoint := range n.ProxyEndpoints {
			if endpoint.URL == r.URL {
				kept = append(kept, r)
				break
			}
		}
	}
	return kept
}
