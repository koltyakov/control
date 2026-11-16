package node

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/koltyakov/control/internal/model"
)

func TestProxyChecksRetryFailureWithoutWaitingHealthyInterval(t *testing.T) {
	checks := newProxyChecks([]model.ProxyEndpoint{{URL: "http://10.0.0.5:3128"}}, nil)
	checks.interval, checks.retry = time.Hour, 10*time.Millisecond
	var calls atomic.Int32
	checks.probe = func(context.Context, string, string) error {
		if calls.Add(1) == 1 {
			return errors.New("gateway restarting")
		}
		return nil
	}
	reports := make(chan []model.ProxyReadiness, 4)
	checks.reported = func() { reports <- checks.snapshot(time.Now()) }
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	done := make(chan struct{})
	go func() { defer close(done); checks.run(ctx, "https://gateway.example", nil) }()
	t.Cleanup(func() { cancel(); <-done })
	for _, state := range []string{model.ProxyUnavailable, model.ProxyReady} {
		select {
		case report := <-reports:
			if len(report) != 1 || report[0].State != state {
				t.Fatalf("expected %s, got %+v", state, report)
			}
		case <-ctx.Done():
			t.Fatal("failed probe did not recover promptly")
		}
	}
	select {
	case <-reports:
		t.Fatal("healthy proxy kept the fast failure cadence")
	case <-time.After(40 * time.Millisecond):
	}
	if calls.Load() != 2 {
		t.Fatal("unexpected repeated probes", calls.Load())
	}
}

func TestProxyChecksReconnectWakesProbeAndJoinsCancellation(t *testing.T) {
	checks := newProxyChecks([]model.ProxyEndpoint{{URL: "http://10.0.0.5:3128"}}, nil)
	checks.interval, checks.retry = time.Hour, time.Hour
	connected := make(chan struct{}, 1)
	connected <- struct{}{} // Initial registration coalesces with startup.
	blocked := make(chan struct{})
	var calls atomic.Int32
	checks.probe = func(ctx context.Context, _, _ string) error {
		switch calls.Add(1) {
		case 1:
			return errors.New("gateway update interrupted the check")
		case 2:
			return nil
		default:
			close(blocked)
			<-ctx.Done()
			return ctx.Err()
		}
	}
	reports := make(chan []model.ProxyReadiness, 3)
	checks.reported = func() { reports <- checks.snapshot(time.Now()) }
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	done := make(chan struct{})
	go func() { defer close(done); checks.run(ctx, "https://gateway.example", connected) }()
	t.Cleanup(func() { cancel(); <-done })
	wait := func(state string) {
		t.Helper()
		select {
		case report := <-reports:
			if len(report) != 1 || report[0].State != state {
				t.Fatalf("expected %s, got %+v", state, report)
			}
		case <-ctx.Done():
			t.Fatal("reconnect did not trigger a readiness check")
		}
	}
	wait(model.ProxyUnavailable)
	connected <- struct{}{}
	wait(model.ProxyReady)
	if calls.Load() != 2 {
		t.Fatal("startup signal caused duplicate checks", calls.Load())
	}
	connected <- struct{}{}
	select {
	case <-blocked:
	case <-ctx.Done():
		t.Fatal("probe did not start before cancellation")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("cancelled readiness check did not join")
	}
}

func TestManagedProxyMaintenanceInvalidatesAndRechecks(t *testing.T) {
	m, err := newManagedProxy(Config{Gateway: "https://gateway.example", DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	checks := newProxyChecks([]model.ProxyEndpoint{{URL: managedTestURL, Managed: true}}, m)
	m.changed = checks.recheck
	checks.results[managedTestURL] = model.ProxyReadiness{URL: managedTestURL, State: model.ProxyReady}
	if !m.fenceIfIdle() {
		t.Fatal("idle proxy did not enter maintenance")
	}
	if got := checks.snapshot(time.Now()); len(got) != 0 {
		t.Fatal("maintenance kept old ready observation", got)
	}
	<-checks.kick
	if !checks.checkAll(t.Context(), "https://gateway.example") {
		// A maintenance refusal must publish unavailable, not attempt CONNECT.
		report := checks.snapshot(time.Now())
		if len(report) != 1 || report[0].State != model.ProxyUnavailable {
			t.Fatal("maintenance did not report unavailable", report)
		}
	} else {
		t.Fatal("maintenance was reported as ready")
	}
	m.unfence()
	select {
	case <-checks.kick:
	default:
		t.Fatal("leaving maintenance waited for the periodic check")
	}
	if got := checks.snapshot(time.Now()); len(got) != 0 {
		t.Fatal("leaving maintenance retained old failure", got)
	}
}
