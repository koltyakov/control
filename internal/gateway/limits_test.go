package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/koltyakov/control/internal/identity"
	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/protocol"
)

func TestGatewayLimitsDefaultsAndValidation(t *testing.T) {
	l, err := (Limits{}).defaults()
	if err != nil || l != DefaultLimits() {
		t.Fatal(l, err)
	}
	if _, err := (Limits{Connections: -1}).defaults(); err == nil {
		t.Fatal("negative limit accepted")
	}
	if err := (Limits{}).Validate(); err == nil {
		t.Fatal("explicit zero limits accepted")
	}
}

func TestGatewayAdmissionConcurrentReservations(t *testing.T) {
	l := DefaultLimits()
	l.Connections = 4
	l.ConnectionsPerAccount = 3
	l.ClientsPerAccount = 3
	a := newAdmission(l)
	reservations := make(chan *connectionReservation, 100)
	var wg sync.WaitGroup
	for i := range 100 {
		wg.Go(func() {
			user := "alice"
			if i%2 == 0 {
				user = "bob"
			}
			if r, ok := a.reserve(user, true); ok {
				r.finishHandshake()
				reservations <- r
			}
		})
	}
	wg.Wait()
	close(reservations)
	if a.connections != 4 || a.handshakes != 0 {
		t.Fatalf("admission overrun: %d %d", a.connections, a.handshakes)
	}
	for _, s := range a.accounts {
		if s.connections > 3 {
			t.Fatal("account overrun")
		}
	}
	for r := range reservations {
		r.release()
		r.release()
	}
	if a.connections != 0 || a.handshakes != 0 {
		t.Fatal("reservation leak")
	}
	r, ok := a.reserve("alice", true)
	if !ok {
		t.Fatal("capacity not reusable")
	}
	r.release()
}

func TestGatewayAdmissionClassAndHandshakeLimits(t *testing.T) {
	l := DefaultLimits()
	l.Handshakes = 2
	l.HandshakesPerAccount = 1
	l.ClientsPerAccount = 1
	l.NodesPerAccount = 1
	a := newAdmission(l)
	r, ok := a.reserve("alice", true)
	if !ok {
		t.Fatal("initial admission")
	}
	if _, ok := a.reserve("alice", false); ok {
		t.Fatal("handshake account bound ignored")
	}
	s, ok := a.reserve("bob", true)
	if !ok {
		t.Fatal("second account blocked")
	}
	if _, ok := a.reserve("charlie", true); ok {
		t.Fatal("global handshake bound ignored")
	}
	r.finishHandshake()
	if _, ok := a.reserve("alice", true); ok {
		t.Fatal("client bound ignored")
	}
	n, ok := a.reserve("alice", false)
	if !ok {
		t.Fatal("node was charged to client limit")
	}
	n.finishHandshake()
	if _, ok := a.reserve("alice", false); ok {
		t.Fatal("node bound ignored")
	}
	r.release()
	s.release()
	n.release()
	if a.connections != 0 || a.handshakes != 0 {
		t.Fatal("failed handshake reservation leak")
	}
}

func limitedGateway(t *testing.T, l Limits) (*Gateway, *httptest.Server) {
	t.Helper()
	g, err := New(t.TempDir(), commonKey, Options{SuperuserKey: superKey, Limits: l})
	if err != nil {
		t.Fatal(err)
	}
	s := httptest.NewServer(g.Handler())
	t.Cleanup(func() { s.Close(); _ = g.Close() })
	return g, s
}

func waitAdmission(t *testing.T, a *admission, connections int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		a.mu.Lock()
		count := a.connections
		a.mu.Unlock()
		if count == connections {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("admitted %d connections, want %d", count, connections)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestGatewayRejectsHandshakeBeforeUpgradeAndReleases(t *testing.T) {
	l := DefaultLimits()
	l.Handshakes = 1
	g, s := limitedGateway(t, l)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	url := "ws" + strings.TrimPrefix(s.URL, "http") + "/v1/client/connect"
	options := &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer " + superKey}}}
	ws, _, err := websocket.Dial(ctx, url, options)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ws.CloseNow() }()
	if _, _, err := ws.Read(ctx); err != nil {
		t.Fatal(err)
	}
	second, resp, err := websocket.Dial(ctx, url, options)
	if second != nil {
		_ = second.CloseNow()
	}
	if err == nil || resp == nil || resp.StatusCode != http.StatusServiceUnavailable || resp.Header.Get("Retry-After") != "1" {
		t.Fatal("handshake was not bounded before upgrade", resp, err)
	}
	if resp.Body != nil {
		_ = resp.Body.Close()
	}
	_ = ws.CloseNow()
	waitAdmission(t, g.admission, 0)
	ws, _, err = websocket.Dial(ctx, url, options)
	if err != nil {
		t.Fatal("capacity not recovered", err)
	}
	_ = ws.CloseNow()
	waitAdmission(t, g.admission, 0)
}

func TestGatewayNewIdentityRatePreservesKnownReconnect(t *testing.T) {
	l := DefaultLimits()
	l.NewIdentitiesPerMinute = 1
	g, s := limitedGateway(t, l)
	owner, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	id, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	n := model.Node{Name: "cli-" + owner.ID[:16]}
	ws := rawOwnedClientSession(t, s.URL, superKey, id, n, true, owner, nil)
	if ws == nil {
		t.Fatal("initial identity rejected")
	}
	_ = ws.CloseNow()
	waitSessionClosed(t, g, id.ID)
	waitAdmission(t, g.admission, 0)
	other, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	if ws := rawOwnedClientSession(t, s.URL, superKey, other, n, true, owner, nil); ws != nil {
		t.Fatal("new transport escaped identity rate")
	}
	var count int
	if err := g.db.QueryRow("SELECT count(*) FROM identities WHERE id=?", other.ID).Scan(&count); err != nil || count != 0 {
		t.Fatal("rejected identity was persisted", count, err)
	}
	ws = rawOwnedClientSession(t, s.URL, superKey, id, n, true, owner, nil)
	if ws == nil {
		t.Fatal("known reconnect charged as a new identity")
	}
	_ = ws.CloseNow()
	waitAdmission(t, g.admission, 0)
}

func TestGatewayAdmissionRejectionDoesNotWaitForMutation(t *testing.T) {
	l := DefaultLimits()
	l.ClientsPerAccount = 1
	g, _ := limitedGateway(t, l)
	r, ok := g.admission.reserve(legacyUser, true)
	if !ok {
		t.Fatal("reserve")
	}
	defer r.release()
	g.mu.Lock()
	defer g.mu.Unlock()
	done := make(chan struct{})
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "http://gateway/v1/client/connect", nil)
	req.Header.Set("Authorization", "Bearer "+superKey)
	go func() { defer close(done); g.Handler().ServeHTTP(w, req) }()
	select {
	case <-done:
		if w.Code != http.StatusServiceUnavailable {
			t.Fatalf("admission status=%d", w.Code)
		}
	case <-time.After(time.Second):
		t.Fatal("rejected admission waited behind mutation lock")
	}
}

func TestGatewayClientAdmissionIsAccountScoped(t *testing.T) {
	l := DefaultLimits()
	l.ClientsPerAccount = 1
	g, s := limitedGateway(t, l)
	_, alice := createTestUser(t, s, "alice")
	_, bob := createTestUser(t, s, "bob")
	open := func(key string) *websocket.Conn {
		t.Helper()
		id, err := identity.Generate()
		if err != nil {
			t.Fatal(err)
		}
		return rawClientSession(t, s.URL, key, id, model.Node{Name: "cli-" + id.ID[:16]}, true)
	}
	first := open(alice.Key)
	if first == nil {
		t.Fatal("initial client refused")
	}
	if open(alice.Key) != nil {
		t.Fatal("account limit escaped")
	}
	if open(bob.Key) == nil {
		t.Fatal("one account consumed another's allowance")
	}
	_ = first.CloseNow()
	// Bob remains connected while Alice's released slot becomes reusable.
	waitAdmission(t, g.admission, 1)
	if open(alice.Key) == nil {
		t.Fatal("account capacity did not recover")
	}
}

func budgetedConnection(a *admission, user string) *connection {
	a.mu.Lock()
	s := a.accounts[user]
	if s == nil {
		s = &accountAdmission{packets: packetBudget{limit: a.limits.QueuedBytesPerAccount}}
		a.accounts[user] = s
	}
	a.mu.Unlock()
	return &connection{out: make(chan *protocol.Packet, 2), relay: newRelayScheduler(), queues: &packetQueues{
		local: packetBudget{limit: relayQueueBytes}, global: &a.packets, account: &s.packets, rejected: a}}
}

func TestGatewayPacketBudgetsAcrossAccountsAndDisconnect(t *testing.T) {
	l := DefaultLimits()
	l.QueuedBytes = 20 << 10
	l.QueuedBytesPerAccount = 10 << 10
	a := newAdmission(l)
	first, second, bob, charlie := budgetedConnection(a, "alice"), budgetedConnection(a, "alice"), budgetedConnection(a, "bob"), budgetedConnection(a, "charlie")
	for _, c := range []*connection{first, second, bob, charlie} {
		defer c.closeQueues()
	}
	p := &protocol.Packet{Kind: "data", From: "source", Session: "bulk", Data: make([]byte, 8<<10)}
	if !first.enqueue(p) || second.enqueue(p) {
		t.Fatal("account aggregate budget failed")
	}
	if !bob.enqueue(p) || charlie.enqueue(p) {
		t.Fatal("global aggregate budget failed")
	}
	if !first.enqueue(&protocol.Packet{Kind: "close", From: "source", Session: "bulk"}) {
		t.Fatal("bulk traffic left no control headroom")
	}
	first.closeQueues()
	if first.enqueue(p) || first.queued.Load() != 0 {
		t.Fatal("closed queue accepted data or retained payload")
	}
	if !charlie.enqueue(p) {
		t.Fatal("disconnect did not recover aggregate budget")
	}
	bob.closeQueues()
	charlie.closeQueues()
	second.closeQueues()
	if a.packets.used.Load() != 0 || first.queues.account.used.Load() != 0 {
		t.Fatal("packet reservations leaked")
	}
}

func TestGatewayAdmissionMetricsAreOperatorOnly(t *testing.T) {
	l := DefaultLimits()
	g, s := limitedGateway(t, l)
	r, ok := g.admission.reserve("test-account", true)
	if !ok {
		t.Fatal("reserve")
	}
	defer r.release()
	var metrics GatewayMetrics
	if err := (testAdmin{URL: s.URL, Key: superKey}).JSON(t.Context(), http.MethodGet, "/v1/admin/metrics", nil, &metrics); err != nil {
		t.Fatal(err)
	}
	if metrics.Limits != l || metrics.AdmittedConnections != 1 || metrics.ActiveHandshakes != 1 {
		t.Fatal("missing admission diagnostics", metrics)
	}
	if err := (testAdmin{URL: s.URL, Key: commonKey}).JSON(t.Context(), http.MethodGet, "/v1/admin/metrics", nil, &metrics); err == nil {
		t.Fatal("common credential read global admission metrics")
	}
}

func TestGatewayPacketBudgetCountsCapacityAndSignaling(t *testing.T) {
	l := DefaultLimits()
	l.QueuedBytesPerAccount = 4096
	a := newAdmission(l)
	c := budgetedConnection(a, "alice")
	defer c.closeQueues()
	large := make([]byte, 1, 8192)
	if c.enqueue(&protocol.Packet{Kind: "close", Data: large}) {
		t.Fatal("non-data capacity escaped byte budget")
	}
	p := &protocol.Packet{Kind: "open", Data: []byte("offer")}
	for range 2 {
		if !c.enqueue(p) {
			t.Fatal("signaling packet rejected below capacity")
		}
	}
	if c.enqueue(p) {
		t.Fatal("signaling packet count limit")
	}
	if got, want := a.packets.used.Load(), 2*packetRetainedBytes(p); got != want {
		t.Fatalf("rollback = %d, want %d", got, want)
	}
	unknown := &protocol.Packet{Kind: "open"}
	unknown.ProtoReflect().SetUnknown(make([]byte, 8192))
	if c.enqueue(unknown) {
		t.Fatal("unknown fields escaped byte budget")
	}
	c.closeQueues()
	if a.packets.used.Load() != 0 {
		t.Fatal("signaling reservation leak")
	}
}

func TestGatewayPacketBudgetConcurrentClose(t *testing.T) {
	a := newAdmission(DefaultLimits())
	c := budgetedConnection(a, "alice")
	var wg sync.WaitGroup
	for range 64 {
		wg.Go(func() {
			for range 32 {
				c.enqueue(&protocol.Packet{Kind: "data", From: "peer", Session: "session", Data: make([]byte, 4096)})
			}
		})
	}
	wg.Go(c.closeQueues)
	wg.Wait()
	c.closeQueues()
	if a.packets.used.Load() != 0 || c.queued.Load() != 0 {
		t.Fatal("concurrent disconnect leaked packet budgets")
	}
}

func TestGatewayFailedPacketWriteReleasesBudget(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = ws.CloseNow() }()
		for {
			if _, _, err := ws.Read(r.Context()); err != nil {
				return
			}
		}
	}))
	defer server.Close()
	ws, _, err := websocket.Dial(t.Context(), "ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ws.CloseNow() }()
	a := newAdmission(DefaultLimits())
	c := budgetedConnection(a, "alice")
	c.ws = ws
	defer c.closeQueues()
	p := &protocol.Packet{Kind: "data", From: "peer", Session: "session", Data: []byte("payload")}
	if !c.enqueue(p) {
		t.Fatal("enqueue")
	}
	p = c.relay.pop()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if c.writeQueuedPacket(ctx, p) == nil {
		t.Fatal("canceled write succeeded")
	}
	if a.packets.used.Load() != 0 || c.queued.Load() != 0 || a.writeFailures.Load() != 1 {
		t.Fatal("write failure leaked reservation")
	}
}
