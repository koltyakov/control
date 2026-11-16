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
//
// A managed endpoint whose listener is running is checked through the
// listener like an external proxy. A listener that failed to bind reports
// unavailable. Disabled owner-managed endpoints are not advertised.
type proxyChecks struct {
	endpoints []model.ProxyEndpoint
	probe     func(context.Context, string, string) error
	managed   *managedProxy
	interval  time.Duration
	retry     time.Duration
	kick      chan struct{}
	reported  func()
	mu        sync.Mutex
	results   map[string]model.ProxyReadiness
}

func newProxyChecks(endpoints []model.ProxyEndpoint, managed *managedProxy) *proxyChecks {
	if len(endpoints) == 0 && managed == nil {
		return nil
	}
	c := &proxyChecks{endpoints: endpoints, probe: gatewayroute.Probe, managed: managed, interval: model.ProxyCheckInterval, retry: 5 * time.Second, kick: make(chan struct{}, 1), results: map[string]model.ProxyReadiness{}}
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

func (c *proxyChecks) run(ctx context.Context, gateway string, connected <-chan struct{}) {
	timer := time.NewTimer(c.interval)
	defer timer.Stop()
	retry := c.retry
	for {
		// Startup and listener restoration can queue the same initial check.
		// Coalesce signals already pending, but retain arrivals during a probe.
		select {
		case <-c.kick:
		default:
		}
		select {
		case <-connected:
		default:
		}
		healthy := c.checkAll(ctx, gateway)
		if c.reported != nil && ctx.Err() == nil {
			c.reported()
		}
		delay := c.interval
		if healthy {
			retry = c.retry
		} else {
			delay = min(retry, c.interval)
			retry = min(retry*2, c.interval)
		}
		timer.Reset(delay)
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		case <-c.kick:
			retry = c.retry
		case <-connected:
			retry = c.retry
		}
	}
}

// checkAll runs sequentially: at most MaxProxyEndpoints bounded probes.
func (c *proxyChecks) checkAll(ctx context.Context, gateway string) bool {
	if ctx.Err() != nil {
		return false
	}
	c.managed.retryListeners()
	c.mu.Lock()
	endpoints := append([]model.ProxyEndpoint(nil), c.endpoints...)
	c.mu.Unlock()
	healthy := true
	for _, endpoint := range endpoints {
		if ctx.Err() != nil {
			return false
		}
		generation := c.managed.generation()
		result, ok := c.check(ctx, endpoint, gateway)
		if ctx.Err() != nil {
			return false
		}
		// A listener change during the check makes its result stale; the
		// change already scheduled another check.
		if endpoint.Managed && c.managed.generation() != generation {
			continue
		}
		c.mu.Lock()
		if ok {
			c.results[endpoint.URL] = result
			healthy = healthy && result.State == model.ProxyReady
		} else {
			delete(c.results, endpoint.URL)
		}
		c.mu.Unlock()
	}
	return healthy
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
			result.State, result.Error = model.ProxyUnavailable, "managed listener is disabled"
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
