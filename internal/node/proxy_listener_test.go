package node

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/store"
)

const managedTestURL = "http://10.0.0.5:3128"

// managedFixture runs a managed listener on loopback, with dialing mapped to
// a local echo "gateway". The production address checks are covered by the
// config tests; this exercises admission, routing, and lifecycle.
func managedFixture(t *testing.T, sources ...string) (*managedProxy, *atomic.Int32, func() string) {
	t.Helper()
	echo, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = echo.Close() })
	go func() {
		for {
			c, err := echo.Accept()
			if err != nil {
				return
			}
			go func() { defer func() { _ = c.Close() }(); _, _ = io.Copy(c, c) }()
		}
	}()
	prefixes := []netip.Prefix{}
	for _, s := range sources {
		prefixes = append(prefixes, netip.MustParsePrefix(s))
	}
	dials := &atomic.Int32{}
	m := &managedProxy{
		target:    "gateway.test:443",
		endpoints: map[string]managedEndpoint{managedTestURL: {url: managedTestURL, addr: netip.MustParseAddrPort("10.0.0.5:3128"), sources: prefixes}},
		statePath: filepath.Join(t.TempDir(), managedProxyStateFile),
		listeners: map[string]net.Listener{}, failures: map[string]string{}, conns: map[net.Conn]struct{}{},
	}
	m.write = func(path string, value any) error { return store.Write(path, value) }
	m.listen = func(ctx context.Context, _ string) (net.Listener, error) {
		return (&net.ListenConfig{}).Listen(ctx, "tcp", "127.0.0.1:0")
	}
	m.dial = func(ctx context.Context, network, address string) (net.Conn, error) {
		if address != "gateway.test:443" {
			return nil, errors.New("unexpected dial " + address)
		}
		dials.Add(1)
		return (&net.Dialer{}).DialContext(ctx, network, echo.Addr().String())
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() { cancel(); m.closeAll() })
	m.start(ctx)
	if err := m.apply([]string{managedTestURL}, true); err != nil {
		t.Fatal(err)
	}
	addr := func() string {
		m.mu.Lock()
		defer m.mu.Unlock()
		if ln := m.listeners[managedTestURL]; ln != nil {
			return ln.Addr().String()
		}
		return ""
	}
	if addr() == "" {
		t.Fatal("listener did not start")
	}
	return m, dials, addr
}

func connectThrough(t *testing.T, address, request string) (net.Conn, *bufio.Reader, string) {
	t.Helper()
	c, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err = io.WriteString(c, request); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(c)
	line, _ := reader.ReadString('\n')
	return c, reader, line
}

func eventuallyTrue(t *testing.T, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !check() {
		if time.Now().After(deadline) {
			t.Fatal("condition not reached")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestManagedListenerAllowsOnlyTheGatewayTarget(t *testing.T) {
	m, dials, addr := managedFixture(t, "127.0.0.0/8")
	for _, request := range []string{
		"CONNECT other.test:443 HTTP/1.1\r\nHost: other.test:443\r\n\r\n",
		"CONNECT gateway.test:8443 HTTP/1.1\r\nHost: gateway.test:8443\r\n\r\n",
		"CONNECT 127.0.0.1:443 HTTP/1.1\r\nHost: 127.0.0.1:443\r\n\r\n",
		"CONNECT http://gateway.test:443 HTTP/1.1\r\nHost: gateway.test:443\r\n\r\n",
	} {
		if _, _, line := connectThrough(t, addr(), request); !strings.Contains(line, "403") && !strings.Contains(line, "400") {
			t.Fatalf("arbitrary target %q answered %q", request, line)
		}
	}
	if _, _, line := connectThrough(t, addr(), "GET http://gateway.test/ HTTP/1.1\r\nHost: gateway.test\r\n\r\n"); !strings.Contains(line, "405") {
		t.Fatalf("plain request answered %q", line)
	}
	if dials.Load() != 0 {
		t.Fatal("refused request dialed upstream")
	}
	// Bytes sent with the request, such as a TLS hello, are forwarded.
	c, reader, line := connectThrough(t, addr(), "CONNECT Gateway.Test.:443 HTTP/1.1\r\nHost: gateway.test:443\r\n\r\nhello")
	if !strings.Contains(line, "200") {
		t.Fatalf("gateway CONNECT answered %q", line)
	}
	if blank, _ := reader.ReadString('\n'); blank != "\r\n" {
		t.Fatalf("response headers %q", blank)
	}
	buf := make([]byte, 5)
	if _, err := io.ReadFull(reader, buf); err != nil || string(buf) != "hello" {
		t.Fatalf("tunnel bytes %q %v", buf, err)
	}
	if !m.busy() {
		t.Fatal("open tunnel not counted")
	}
	_ = c.Close()
	eventuallyTrue(t, func() bool { return !m.busy() })
}

func TestManagedListenerRefusesUnlistedSources(t *testing.T) {
	m, dials, addr := managedFixture(t, "10.20.0.0/24")
	c, err := net.Dial("tcp", addr())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	_, _ = io.WriteString(c, "CONNECT gateway.test:443 HTTP/1.1\r\n\r\n")
	_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
	if n, err := c.Read(make([]byte, 64)); n != 0 || err == nil {
		t.Fatal("unlisted source received a response")
	}
	if dials.Load() != 0 || m.busy() {
		t.Fatal("unlisted source was admitted")
	}
}

func TestManagedListenerUpdateFenceAndDeactivation(t *testing.T) {
	m, _, addr := managedFixture(t, "127.0.0.0/8")
	address := addr()
	// A pending handshake is owned work: no maintenance fence while it exists.
	pending, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	eventuallyTrue(t, m.busy)
	if m.fenceIfIdle() {
		t.Fatal("fenced over a pending CONNECT handshake")
	}
	_ = pending.Close()
	eventuallyTrue(t, func() bool { return !m.busy() })
	tunnel, reader, line := connectThrough(t, address, "CONNECT gateway.test:443 HTTP/1.1\r\n\r\n")
	if !strings.Contains(line, "200") {
		t.Fatal(line)
	}
	_, _ = reader.ReadString('\n')
	if m.fenceIfIdle() {
		t.Fatal("fenced over an active tunnel")
	}
	// Losing every dependency stops accepting without cutting the tunnel.
	if err := m.apply(nil, true); err != nil {
		t.Fatal(err)
	}
	if _, err = net.DialTimeout("tcp", address, time.Second); err == nil {
		t.Fatal("deactivated listener still accepts")
	}
	if _, err = io.WriteString(tunnel, "ping"); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4)
	if _, err = io.ReadFull(reader, buf); err != nil || string(buf) != "ping" {
		t.Fatal("deactivation closed an admitted tunnel", err)
	}
	_ = tunnel.Close()
	eventuallyTrue(t, func() bool { return !m.busy() })
	var saved model.ProxyState
	if err = store.Read(m.statePath, &saved); err != nil || len(saved.Listeners) != 0 {
		t.Fatalf("deactivation not persisted: %+v %v", saved, err)
	}
	// Reactivate, fence while idle: new connections are refused until resume.
	if err := m.apply([]string{managedTestURL}, true); err != nil {
		t.Fatal(err)
	}
	address = addr()
	if !m.fenceIfIdle() {
		t.Fatal("idle listener could not be fenced")
	}
	c, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
	if n, _ := c.Read(make([]byte, 16)); n != 0 || m.busy() {
		t.Fatal("fenced listener admitted a connection")
	}
	_ = c.Close()
	m.unfence()
	if _, _, line = connectThrough(t, address, "CONNECT gateway.test:443 HTTP/1.1\r\n\r\n"); !strings.Contains(line, "200") {
		t.Fatalf("resumed listener answered %q", line)
	}
}

func TestManagedListenerShutdownClosesTunnelsAndJoins(t *testing.T) {
	m, _, addr := managedFixture(t, "127.0.0.0/8")
	c, reader, line := connectThrough(t, addr(), "CONNECT gateway.test:443 HTTP/1.1\r\n\r\n")
	if !strings.Contains(line, "200") {
		t.Fatal(line)
	}
	_, _ = reader.ReadString('\n')
	done := make(chan struct{})
	go func() { m.closeAll(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown did not join tunnel goroutines")
	}
	_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := reader.ReadByte(); err == nil {
		t.Fatal("tunnel survived shutdown")
	}
	if m.busy() {
		t.Fatal("shutdown left admitted connections")
	}
}

func TestManagedListenerRestoresPersistedDecisionAndReportsBindFailure(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{Gateway: "https://Gateway.Example", DataDir: dir, ProxyEndpoints: []model.ProxyEndpoint{{URL: managedTestURL, Managed: true, Sources: []string{"10.0.0.0/24"}}}}
	if err := store.Write(filepath.Join(dir, managedProxyStateFile), model.ProxyState{Listeners: []string{managedTestURL}}); err != nil {
		t.Fatal(err)
	}
	m, err := newManagedProxy(cfg)
	if err != nil || m == nil {
		t.Fatal(err)
	}
	if m.target != "gateway.example:443" || !slices.Equal(m.desired, []string{managedTestURL}) {
		t.Fatalf("restored %q %v", m.target, m.desired)
	}
	m.listen = func(context.Context, string) (net.Listener, error) { return nil, errors.New("bind: address in use") }
	ctx, cancel := context.WithCancel(context.Background())
	defer func() { cancel(); m.closeAll() }()
	m.setSupported(true)
	m.start(ctx)
	if _, listening, failure := m.endpointState(managedTestURL); listening || failure == "" {
		t.Fatal("bind failure reported as running")
	}
	checks := newProxyChecks(cfg.ProxyEndpoints, m)
	checks.probe = func(context.Context, string, string) error {
		t.Fatal("probed a listener that is not running")
		return nil
	}
	checks.checkAll(ctx, cfg.Gateway)
	if got := checks.snapshot(time.Now()); len(got) != 1 || got[0].State != model.ProxyUnavailable {
		t.Fatalf("readiness %+v", got)
	}
	// A stopped listener is unavailable, never selectable as standby.
	if err := m.apply(nil, true); err != nil {
		t.Fatal(err)
	}
	checks.checkAll(ctx, cfg.Gateway)
	if got := checks.snapshot(time.Now()); len(got) != 1 || got[0].State != model.ProxyUnavailable {
		t.Fatalf("readiness %+v", got)
	}
	// Advertisement omits the local source allowlist.
	if advertised := advertisedProxyEndpoints(cfg.ProxyEndpoints); advertised[0].Sources != nil || !advertised[0].Managed {
		t.Fatalf("advertised %+v", advertised)
	}
}

func TestManagedProxyConfigRequiresPrivateListenAndSources(t *testing.T) {
	base := func(endpoint model.ProxyEndpoint) Config {
		return Config{Name: "bridge", Gateway: "https://gateway.example", Token: testToken, DataDir: t.TempDir(), WorkDir: t.TempDir(), ProxyEndpoints: []model.ProxyEndpoint{endpoint}}
	}
	for _, endpoint := range []model.ProxyEndpoint{
		{URL: "http://bridge.lan:3128", Managed: true, Sources: []string{"10.0.0.0/24"}},
		{URL: "http://0.0.0.0:3128", Managed: true, Sources: []string{"10.0.0.0/24"}},
		{URL: "http://127.0.0.1:3128", Managed: true, Sources: []string{"127.0.0.0/8"}},
		{URL: "http://203.0.113.5:3128", Managed: true, Sources: []string{"10.0.0.0/24"}},
		{URL: "http://10.0.0.5:3128", Managed: true},
		{URL: "http://10.0.0.5:3128", Managed: true, Sources: []string{"0.0.0.0/0"}},
		{URL: "http://10.0.0.5:3128", Managed: true, Sources: []string{"8.0.0.0/4"}},
		{URL: "http://10.0.0.5:3128", Sources: []string{"10.0.0.0/24"}},
	} {
		cfg := base(endpoint)
		if err := cfg.defaults(); err == nil {
			t.Fatalf("accepted %+v", endpoint)
		}
	}
	for _, endpoint := range []model.ProxyEndpoint{
		{URL: "http://10.0.0.5:3128", Managed: true, Sources: []string{"10.0.0.0/24", "192.168.1.7"}},
		{URL: "http://100.100.1.2:3128", Managed: true, Sources: []string{"100.64.0.0/10"}},
		{URL: "http://[fd00::5]:3128", Managed: true, Sources: []string{"fd00::/64"}},
	} {
		cfg := base(endpoint)
		if err := cfg.defaults(); err != nil {
			t.Fatalf("rejected %+v: %v", endpoint, err)
		}
	}
	cfg := base(model.ProxyEndpoint{URL: "http://10.0.0.5:3128", Managed: true, Sources: []string{"10.0.0.0/24"}})
	cfg.Listen = "10.0.0.5:3128"
	if err := cfg.defaults(); err == nil {
		t.Fatal("managed listener shared the local API address")
	}
}

func TestRetirementAcknowledgesExactRevisionAfterPersistence(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var acks []model.MachineStateProof
	failFirst := true
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/node/state":
			var proof model.MachineStateProof
			_ = json.NewDecoder(r.Body).Decode(&proof)
			mu.Lock()
			if proof.Revision == 4 {
				acks = append(acks, proof)
				if failFirst {
					failFirst = false
					mu.Unlock()
					http.Error(w, "transient", http.StatusBadGateway)
					return
				}
			}
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(model.MachineState{Revision: 4, Disabled: true, Unregistered: true})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	n, err := New(Config{Name: "retiring", Gateway: server.URL, Token: testToken, DataDir: t.TempDir(), WorkDir: t.TempDir(), MetricsIntervalSeconds: -1})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	if err = n.Start(ctx); !errors.Is(err, ErrUnregistered) {
		t.Fatal(err)
	}
	mu.Lock()
	count := len(acks)
	mu.Unlock()
	// A failed acknowledgement is retried within its bound.
	if count != 2 {
		t.Fatalf("acknowledgements %d, want a retry after the failed first", count)
	}
	if err = n.Close(); err != nil {
		t.Fatal(err)
	}
	// A restart of the retired node repeats the acknowledgement from its
	// durable state before stopping.
	restarted, err := New(n.Config)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = restarted.Close() }()
	if err = restarted.Start(ctx); !errors.Is(err, ErrUnregistered) {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(acks) != 3 || acks[2].InspectOnly {
		t.Fatalf("restart acknowledgements %+v", acks)
	}
}

func TestManagedTunnelsBlockNodeUpdateReservation(t *testing.T) {
	n, err := New(Config{Name: "bridge", Gateway: "https://gateway.example", Token: testToken, DataDir: t.TempDir(), WorkDir: t.TempDir(), MetricsIntervalSeconds: -1})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = n.Close() }()
	m, _, addr := managedFixture(t, "127.0.0.0/8")
	n.managedProxy = m
	tunnel, reader, line := connectThrough(t, addr(), "CONNECT gateway.test:443 HTTP/1.1\r\n\r\n")
	if !strings.Contains(line, "200") {
		t.Fatal(line)
	}
	_, _ = reader.ReadString('\n')
	if !n.updateBusy() || n.pauseForUpdate() {
		t.Fatal("update reserved over an active managed tunnel")
	}
	if _, paused := n.work.State(); paused {
		t.Fatal("failed reservation left work admission paused")
	}
	if _, err = io.WriteString(tunnel, "x"); err != nil {
		t.Fatal("update check disturbed the tunnel", err)
	}
	_ = tunnel.Close()
	eventuallyTrue(t, func() bool { return !m.busy() })
	if !n.pauseForUpdate() {
		t.Fatal("idle node could not reserve maintenance")
	}
	c, err := net.Dial("tcp", addr())
	if err != nil {
		t.Fatal(err)
	}
	_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
	if k, _ := c.Read(make([]byte, 8)); k != 0 || m.busy() {
		t.Fatal("reserved node admitted a new tunnel")
	}
	_ = c.Close()
	n.resumeAfterUpdate()
	if _, _, line = connectThrough(t, addr(), "CONNECT gateway.test:443 HTTP/1.1\r\n\r\n"); !strings.Contains(line, "200") {
		t.Fatalf("resumed node answered %q", line)
	}
}

func TestManagedListenerRetriesFailedDecisionPersistence(t *testing.T) {
	m, _, addr := managedFixture(t, "127.0.0.0/8")
	address := addr()
	write := m.write
	m.write = func(string, any) error { return errors.New("disk unavailable") }
	if err := m.apply(nil, true); err == nil {
		t.Fatal("failed persistence was ignored")
	}
	if addr() != address || !slices.Equal(m.desired, []string{managedTestURL}) {
		t.Fatal("failed deactivation changed running or desired listeners")
	}
	m.write = write
	if err := m.apply(nil, true); err != nil {
		t.Fatal(err)
	}
	if addr() != "" {
		t.Fatal("deactivation was not retried")
	}
	m.write = func(string, any) error { return errors.New("disk unavailable") }
	if err := m.apply([]string{managedTestURL}, true); err == nil {
		t.Fatal("failed activation persistence was ignored")
	}
	if addr() != "" || len(m.desired) != 0 {
		t.Fatal("activation ran without a durable decision")
	}
	m.write = write
	if err := m.apply([]string{managedTestURL}, true); err != nil {
		t.Fatal(err)
	}
	if addr() == "" {
		t.Fatal("activation was not retried")
	}
	var saved model.ProxyState
	if err := store.Read(m.statePath, &saved); err != nil || !slices.Equal(saved.Listeners, m.desired) {
		t.Fatalf("restart state differs: %+v %v", saved, err)
	}
}

func TestManagedListenerRejectsCorruptSavedDecision(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, managedProxyStateFile), []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	_, err := newManagedProxy(Config{Gateway: "https://gateway.example", DataDir: dir, ProxyEndpoints: []model.ProxyEndpoint{{URL: managedTestURL, Managed: true, Sources: []string{"10.0.0.0/24"}}}})
	if err == nil {
		t.Fatal("corrupt saved decision was treated as no dependencies")
	}
}
