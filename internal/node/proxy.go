package node

import (
	"context"
	"sync"
	"time"

	"github.com/koltyakov/control/internal/gatewayroute"
	"github.com/koltyakov/control/internal/model"
)

// proxyChecks owns this worker's bounded proxy-to-gateway checks. They run on
// their own interval, independently of dashboard polling. The gateway never
// probes advertised endpoints itself.
type proxyChecks struct {
	endpoints []model.ProxyEndpoint
	probe     func(context.Context, string, string) error
	interval  time.Duration
	mu        sync.Mutex
	results   map[string]model.ProxyReadiness
}

func newProxyChecks(endpoints []model.ProxyEndpoint) *proxyChecks {
	if len(endpoints) == 0 {
		return nil
	}
	return &proxyChecks{endpoints: endpoints, probe: gatewayroute.Probe, interval: model.ProxyCheckInterval, results: map[string]model.ProxyReadiness{}}
}

func (c *proxyChecks) run(ctx context.Context, gateway string) {
	ticker := time.NewTicker(c.interval)
	defer ticker.Stop()
	for {
		c.checkAll(ctx, gateway)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// checkAll runs sequentially: at most MaxProxyEndpoints bounded probes.
func (c *proxyChecks) checkAll(ctx context.Context, gateway string) {
	for _, endpoint := range c.endpoints {
		if ctx.Err() != nil {
			return
		}
		err := c.probe(ctx, endpoint.URL, gateway)
		if ctx.Err() != nil {
			return
		}
		result := model.ProxyReadiness{URL: endpoint.URL, State: model.ProxyReady, CheckedAt: time.Now().UTC()}
		if err != nil {
			result.State, result.Error = model.ProxyUnavailable, boundedProxyError(err.Error())
		}
		c.mu.Lock()
		c.results[endpoint.URL] = result
		c.mu.Unlock()
	}
}

func boundedProxyError(text string) string {
	if len(text) > model.MaxProxyErrorBytes {
		text = text[:model.MaxProxyErrorBytes]
	}
	return text
}

// snapshot reports checked endpoints with their age, so the gateway can date
// each check on its own clock. Unchecked endpoints are omitted.
func (c *proxyChecks) snapshot(now time.Time) []model.ProxyReadiness {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []model.ProxyReadiness
	for _, endpoint := range c.endpoints {
		r, ok := c.results[endpoint.URL]
		if !ok {
			continue
		}
		r.AgeSeconds = max(0, int(now.Sub(r.CheckedAt)/time.Second))
		out = append(out, r)
	}
	return out
}
