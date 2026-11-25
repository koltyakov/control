package gatewayroute

import (
	"context"
	"errors"
	"io"
	"net/http"
	"sync"
	"time"
)

// routeMaintenance belongs to one configured route, not the process-wide
// transport cache. Application/provider HTTP and other profiles are unaffected.
type routeMaintenance struct {
	mu      sync.Mutex
	active  int
	until   time.Time
	changed chan struct{}
}

type maintenanceTransport struct {
	base  http.RoundTripper
	state *routeMaintenance
}

func (t maintenanceTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	s := t.state
	s.mu.Lock()
	if time.Now().Before(s.until) {
		s.mu.Unlock()
		return nil, errors.New("gateway route is parked for proxy maintenance")
	}
	s.active++
	s.mu.Unlock()
	var once sync.Once
	release := func() { once.Do(func() { s.mu.Lock(); s.active--; s.mu.Unlock() }) }
	resp, err := t.base.RoundTrip(req)
	if err != nil || resp.StatusCode == http.StatusSwitchingProtocols {
		// Upgraded WebSockets are owned and checked by the peer transport.
		release()
		return resp, err
	}
	resp.Body = &maintenanceBody{ReadCloser: resp.Body, release: release}
	return resp, nil
}

type maintenanceBody struct {
	io.ReadCloser
	release func()
}

func (b *maintenanceBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err != nil {
		b.release()
	}
	return n, err
}

func (b *maintenanceBody) Close() error {
	err := b.ReadCloser.Close()
	b.release()
	return err
}

// ParkIfIdle excludes new requests only after every admitted response body has
// drained. Idle HTTP/1 and HTTP/2 pools are then closed without cancelling work.
func (r Route) ParkIfIdle(until time.Time) bool {
	if r.maintenance == nil || !time.Now().Before(until) {
		return false
	}
	s := r.maintenance
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.active != 0 {
		return false
	}
	s.until = until
	r.CloseIdleConnections()
	return true
}

// Resume releases a park early. Its deadline also expires without a coordinator.
func (r Route) Resume() {
	if s := r.maintenance; s != nil {
		s.mu.Lock()
		s.until = time.Time{}
		close(s.changed)
		s.changed = make(chan struct{})
		s.mu.Unlock()
	}
}

// Wait prevents the gateway reconnect loop from accumulating backoff while a
// deliberately parked route is unavailable.
func (r Route) Wait(ctx context.Context) error {
	if r.maintenance == nil {
		return ctx.Err()
	}
	for {
		s := r.maintenance
		s.mu.Lock()
		until, changed := s.until, s.changed
		s.mu.Unlock()
		if !time.Now().Before(until) {
			return ctx.Err()
		}
		timer := time.NewTimer(time.Until(until))
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-changed:
			timer.Stop()
		case <-timer.C:
		}
	}
}
