package gatewayroute

import (
	"context"
	"io"
	"testing"
	"time"
)

func TestProxyRouteParkDrainsHTTPAndKeepsOtherRoutesIndependent(t *testing.T) {
	gateway, proxy := gatewayFixture(t)
	route, err := New(proxy.URL)
	if err != nil {
		t.Fatal(err)
	}
	other, err := New(proxy.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := route.Client(time.Second, false).Get(gateway + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	until := time.Now().Add(time.Minute)
	if route.ParkIfIdle(until) {
		t.Fatal("park cut off an admitted response body")
	}
	if _, err := io.ReadAll(resp.Body); err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if !route.ParkIfIdle(until) {
		t.Fatal("drained route could not park")
	}
	if _, err := route.Client(time.Second, false).Get(gateway + "/healthz"); err == nil {
		t.Fatal("parked route opened a new connection")
	}
	resp2, err := other.Client(time.Second, false).Get(gateway + "/healthz")
	if err != nil {
		t.Fatal("park affected a separate configured route", err)
	}
	_, _ = io.Copy(io.Discard, resp2.Body)
	_ = resp2.Body.Close()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := route.Wait(ctx); err != context.Canceled {
		t.Fatal("park wait ignored cancellation", err)
	}
	route.Resume()
	if err := route.Wait(t.Context()); err != nil {
		t.Fatal(err)
	}
	resp3, err := route.Client(time.Second, false).Get(gateway + "/healthz")
	if err != nil {
		t.Fatal("resume did not restore route", err)
	}
	_ = resp3.Body.Close()
	route.maintenance.mu.Lock()
	route.maintenance.until = time.Now().Add(-time.Second)
	route.maintenance.mu.Unlock()
	if err := route.Wait(t.Context()); err != nil {
		t.Fatal("deadline did not recover route", err)
	}
}
