package node

import (
	"context"
	"net"
	"sync"
	"time"

	"github.com/koltyakov/control/internal/gatewayroute"
	"github.com/koltyakov/control/internal/model"
)

// proxyChecks owns this worker's bounded proxy-to-gateway checks. They run on
// their own interval, independently of dashboard polling. The gateway never
// probes advertised endpoints itself.
//
// A managed endpoint whose listener is running is checked through the
// listener like an external proxy. A managed endpoint that is intentionally
// off reports standby after a fresh direct check of the path its listener
// would use, and a listener that failed to bind reports unavailable. Managed
// results are omitted until the gateway confirms managed proxy support.
type proxyChecks struct {
	endpoints []model.ProxyEndpoint
	probe     func(context.Context, string, string) error
	direct    func(context.Context, string) error
	managed   *managedProxy
	interval  time.Duration
	kick      chan struct{}
	reported  func()
	mu        sync.Mutex
	results   map[string]model.ProxyReadiness
}

func newProxyChecks(endpoints []model.ProxyEndpoint, managed *managedProxy) *proxyChecks {
	if len(endpoints) == 0 {
		return nil
	}
	c := &proxyChecks{endpoints: endpoints, probe: gatewayroute.Probe, managed: managed, interval: model.ProxyCheckInterval, kick: make(chan struct{}, 1), results: map[string]model.ProxyReadiness{}}
	c.direct = func(ctx context.Context, gateway string) error {
		dial := (&net.Dialer{Timeout: managedDialTimeout}).DialContext
		if managed != nil {
			dial = managed.dial
		}
		return gatewayroute.ProbeDirect(ctx, gateway, dial)
	}
	return c
}

// recheck discards managed observations that no longer describe the listener
// and schedules an immediate check, so a stopped listener is never reported
// ready from an older observation.
func (c *proxyChecks) recheck() {
	if c == nil {
		return
	}
	c.mu.Lock()
	for _, endpoint := range c.endpoints {
		if endpoint.Managed {
			delete(c.results, endpoint.URL)
		}
	}
	c.mu.Unlock()
	select {
	case c.kick <- struct{}{}:
	default:
	}
}

func (c *proxyChecks) run(ctx context.Context, gateway string) {
	ticker := time.NewTicker(c.interval)
	defer ticker.Stop()
	for {
		c.checkAll(ctx, gateway)
		if c.reported != nil && ctx.Err() == nil {
			c.reported()
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-c.kick:
		}
	}
}

// checkAll runs sequentially: at most MaxProxyEndpoints bounded probes.
func (c *proxyChecks) checkAll(ctx context.Context, gateway string) {
	for _, endpoint := range c.endpoints {
		if ctx.Err() != nil {
			return
		}
		generation := c.managed.generation()
		result, ok := c.check(ctx, endpoint, gateway)
		if ctx.Err() != nil {
			return
		}
		// A listener change during the check makes its result stale; the
		// change already scheduled another check.
		if endpoint.Managed && c.managed.generation() != generation {
			continue
		}
		c.mu.Lock()
		if ok {
			c.results[endpoint.URL] = result
		} else {
			delete(c.results, endpoint.URL)
		}
		c.mu.Unlock()
	}
}

func (c *proxyChecks) check(ctx context.Context, endpoint model.ProxyEndpoint, gateway string) (model.ProxyReadiness, bool) {
	result := model.ProxyReadiness{URL: endpoint.URL, State: model.ProxyReady}
	var err error
	switch {
	case !endpoint.Managed:
		err = c.probe(ctx, endpoint.URL, gateway)
	case c.managed == nil:
		return result, false
	default:
		supported, listening, failure := c.managed.endpointState(endpoint.URL)
		switch {
		case !supported:
			return result, false
		case failure != "":
			result.State, result.Error = model.ProxyUnavailable, failure
		case listening:
			err = c.probe(ctx, endpoint.URL, gateway)
		default:
			result.State = model.ProxyStandby
			err = c.direct(ctx, gateway)
		}
	}
	if err != nil {
		result.State, result.Error = model.ProxyUnavailable, boundedProxyError(err.Error())
	}
	result.CheckedAt = time.Now().UTC()
	return result, true
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
