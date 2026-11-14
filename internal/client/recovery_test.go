package client

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/koltyakov/control/internal/gateway"
	"github.com/koltyakov/control/internal/transport"
	"github.com/koltyakov/control/internal/transport/transporttest"
)

func routingFixture(t *testing.T, intercept func(http.ResponseWriter, *http.Request) bool) Client {
	t.Helper()
	const key = "routing-recovery-superuser-1234567890"
	g, err := gateway.New(t.TempDir(), "", gateway.Options{SuperuserKey: key})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = g.Close() })
	handler := g.Handler()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/auth" && intercept(w, r) {
			return
		}
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	var issued struct{ Token string }
	if err := (Admin{URL: server.URL, Key: key}).JSON(t.Context(), http.MethodPost, "/v1/admin/users", map[string]string{"name": "recovery"}, &issued); err != nil {
		t.Fatal(err)
	}
	c := (Client{}).WithStandalone(t.Context(), StandaloneConfig{
		Gateway: Admin{URL: server.URL, Key: issued.Token}, StateDir: t.TempDir(),
		AccountRouting: true, PeerOptions: []transport.Options{transporttest.Loopback()},
	})
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func TestStandaloneRetriesFailedInitialization(t *testing.T) {
	var calls atomic.Int32
	c := routingFixture(t, func(w http.ResponseWriter, _ *http.Request) bool {
		if calls.Add(1) == 1 {
			http.Error(w, "temporarily unavailable", http.StatusServiceUnavailable)
			return true
		}
		return false
	})
	if _, err := c.backend(t.Context()); err == nil {
		t.Fatal("first initialization succeeded")
	}
	peer, err := c.backend(t.Context())
	if err != nil || peer == nil {
		t.Fatal("explicit retry did not recover", err)
	}
	initializedCalls := calls.Load() // Peer.Start also authenticates its connection.
	if again, err := c.backend(t.Context()); err != nil || again != peer || calls.Load() != initializedCalls {
		t.Fatal("successful initialization was not cached", err, calls.Load())
	}
}

func TestStandaloneInitializationWaiters(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	c := routingFixture(t, func(_ http.ResponseWriter, r *http.Request) bool {
		if calls.Add(1) == 1 {
			close(entered)
			select {
			case <-release:
			case <-r.Context().Done():
				return true
			}
		}
		return false
	})
	first := make(chan error, 1)
	go func() { _, err := c.backend(t.Context()); first <- err }()
	<-entered
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := c.backend(cancelled); !errors.Is(err, context.Canceled) {
		t.Fatal("cancelled waiter", err)
	}
	var wg sync.WaitGroup
	results := make(chan error, 8)
	for range 8 {
		wg.Go(func() { _, err := c.backend(t.Context()); results <- err })
	}
	close(release)
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != 2 { // Owner lookup followed by peer connection authentication.
		t.Fatal("concurrent setup was duplicated", calls.Load())
	}
}

type observedWaitContext struct {
	context.Context
	waiting chan struct{}
	once    sync.Once
}

func (c *observedWaitContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.waiting) })
	return c.Context.Done()
}

func TestStandaloneFailedAttemptShared(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	c := routingFixture(t, func(w http.ResponseWriter, r *http.Request) bool {
		calls.Add(1)
		close(entered)
		select {
		case <-release:
		case <-r.Context().Done():
		}
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
		return true
	})
	first, second := make(chan error, 1), make(chan error, 1)
	go func() { _, err := c.backend(t.Context()); first <- err }()
	<-entered
	waiter := &observedWaitContext{Context: t.Context(), waiting: make(chan struct{})}
	go func() { _, err := c.backend(waiter); second <- err }()
	<-waiter.waiting
	close(release)
	if firstErr, secondErr := <-first, <-second; firstErr == nil || firstErr != secondErr {
		t.Fatal("waiter did not receive the initiating attempt's failure", firstErr, secondErr)
	}
	if calls.Load() != 1 {
		t.Fatal("waiter retried failed initialization", calls.Load())
	}
}

func TestStandaloneLocalErrorKeepsBackend(t *testing.T) {
	var localCalls, gatewayCalls atomic.Int32
	local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if localCalls.Add(1) == 1 {
			http.Error(w, "local unavailable", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte(`{"result":{}}`))
	}))
	t.Cleanup(local.Close)
	remote := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { gatewayCalls.Add(1) }))
	t.Cleanup(remote.Close)
	c := (Client{URL: local.URL}).WithStandalone(t.Context(), StandaloneConfig{Gateway: Admin{URL: remote.URL, Key: "unused"}, StateDir: t.TempDir()})
	t.Cleanup(func() { _ = c.Close() })
	if err := c.Call(t.Context(), "", "node.describe", map[string]any{}, nil); err == nil {
		t.Fatal("local failure lost")
	}
	if err := c.Call(t.Context(), "", "node.describe", map[string]any{}, nil); err != nil {
		t.Fatal("local backend did not recover", err)
	}
	local.Close()
	if err := c.Call(t.Context(), "", "node.describe", map[string]any{}, nil); err == nil {
		t.Fatal("closed local backend succeeded")
	}
	if gatewayCalls.Load() != 0 {
		t.Fatal("selected local backend switched to gateway")
	}
}

func TestStandaloneInitializationCancellation(t *testing.T) {
	for _, closing := range []bool{false, true} {
		t.Run(map[bool]string{false: "initiator", true: "close"}[closing], func(t *testing.T) {
			entered := make(chan struct{})
			var calls atomic.Int32
			c := routingFixture(t, func(_ http.ResponseWriter, r *http.Request) bool {
				if calls.Add(1) != 1 {
					return false
				}
				close(entered)
				<-r.Context().Done()
				return true
			})
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			done := make(chan error, 1)
			go func() { _, err := c.backend(ctx); done <- err }()
			<-entered
			if closing {
				if err := c.Close(); err != nil {
					t.Fatal(err)
				}
				c.routing.mu.Lock()
				pending := c.routing.attempt != nil
				c.routing.mu.Unlock()
				if pending {
					t.Fatal("Close returned before initialization cleanup")
				}
			} else {
				cancel()
			}
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("cancelled initialization succeeded")
				}
			case <-time.After(5 * time.Second):
				t.Fatal("setup cancellation blocked")
			}
			peer, err := c.backend(t.Context())
			if closing {
				if err == nil || peer != nil || calls.Load() != 1 {
					t.Fatal("closed client restarted", err)
				}
			} else if err != nil || peer == nil {
				t.Fatal("later explicit retry failed", err)
			}
		})
	}
}

func TestCompletedForwardsReleaseCapacity(t *testing.T) {
	c := (Client{}).WithLifetime(t.Context())
	t.Cleanup(func() { _ = c.Close() })
	for i := range maxForwards + 2 {
		f, err := c.StartForward(t.Context(), ForwardSpec{Node: "unused", Address: "127.0.0.1:1"})
		if err != nil {
			t.Fatal(i, err)
		}
		if i%2 == 0 {
			_ = f.listener.Close() // Exercise automatic listener failure cleanup.
		} else {
			f.Close()
		}
		select {
		case <-f.Done():
		case <-time.After(5 * time.Second):
			t.Fatal("forward did not finish")
		}
		if len(c.Forwards()) != 0 {
			t.Fatal("completed forward still active")
		}
		if err := c.StopForward(f.Info().ID); err != nil {
			t.Fatal("recent completed ID was forgotten", err)
		}
	}
	if err := c.StopForward("unknown"); err == nil {
		t.Fatal("unknown ID accepted")
	}
}

func TestForwardConcurrentStopCloseAndStart(t *testing.T) {
	c := (Client{}).WithLifetime(t.Context())
	t.Cleanup(func() { _ = c.Close() })
	f, err := c.StartForward(t.Context(), ForwardSpec{Node: "unused", Address: "127.0.0.1:1"})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	wg.Go(f.Close)
	wg.Go(func() { _ = c.StopForward(f.Info().ID) })
	wg.Go(func() {
		next, err := c.StartForward(t.Context(), ForwardSpec{Node: "unused", Address: "127.0.0.1:1"})
		if err != nil {
			t.Error(err)
			return
		}
		next.Close()
	})
	wg.Wait()
	if len(c.Forwards()) != 0 {
		t.Fatal("completed forwards retained capacity")
	}
}

func TestCompletedForwardAcknowledgmentsAreBounded(t *testing.T) {
	c := (Client{}).WithLifetime(t.Context())
	t.Cleanup(func() { _ = c.Close() })
	var first, last string
	for i := range maxForwards + 2 {
		forward, err := c.StartForward(t.Context(), ForwardSpec{Node: "unused", Address: "127.0.0.1:1"})
		if err != nil {
			t.Fatal(err)
		}
		last = forward.Info().ID
		if i == 0 {
			first = last
		}
		forward.Close()
	}
	if len(c.resources.finished) != maxForwards || len(c.Forwards()) != 0 {
		t.Fatal("completed forward history is unbounded or consumes active capacity")
	}
	if err := c.StopForward(first); err == nil {
		t.Fatal("evicted completion ID was still recognized")
	}
	if err := c.StopForward(last); err != nil {
		t.Fatal("recent completion ID was not acknowledged", err)
	}
	if err := c.StopForward(last); err == nil {
		t.Fatal("acknowledged completion ID was not forgotten")
	}
}
