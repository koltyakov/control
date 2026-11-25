package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/koltyakov/control/internal/client"
	"github.com/koltyakov/control/internal/gateway"
	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/node"
	"github.com/koltyakov/control/internal/transport"
	"github.com/koltyakov/control/internal/transport/transporttest"
)

// These tests exercise the fleet lock end to end: a real gateway, three real
// workers, standalone account clients, and the console API reached with an
// authenticated browser cookie. A policy change acknowledged by the console
// applies to the very next request, so every decision below is the first
// attempt after the change, made on a connection warmed before the lock.
// Every refusal is checked against the providers on all workers, so a denial
// that still dispatched work fails.

const (
	fleetLockSuperuser = "fleet-lock-operator-key-at-least-32-characters"
	fleetLockPassword  = "fleet lock console password"
)

var fleetLockNames = []string{"alpha", "bravo", "charlie"}

// fleetLockCounter counts provider runs on one worker.
type fleetLockCounter struct {
	calls *atomic.Int64
}

func (fleetLockCounter) Capability() model.Capability {
	return model.Capability{Name: "test.effect", InputSchema: json.RawMessage(`{"type":"object"}`)}
}

func (p fleetLockCounter) Run(context.Context, json.RawMessage, node.Execution) (any, error) {
	p.calls.Add(1)
	return "ran", nil
}

type fleetVaultEntry struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type fleetVaultAllowance struct {
	ID          string    `json:"id"`
	KeyID       string    `json:"keyId"`
	OwnerID     string    `json:"ownerId"`
	AllMachines bool      `json:"allMachines"`
	MachineIDs  []string  `json:"machineIds"`
	ExpiresAt   time.Time `json:"expiresAt"`
}

type fleetVaultPolicy struct {
	Enabled     bool                  `json:"enabled"`
	PausedUntil time.Time             `json:"pausedUntil"`
	Revision    int64                 `json:"revision"`
	Allowances  []fleetVaultAllowance `json:"allowances"`
	Keys        []fleetVaultEntry     `json:"keys"`
	Hosts       []fleetVaultEntry     `json:"hosts"`
	Machines    []fleetVaultEntry     `json:"machines"`
}

// consoleBrowser behaves like the console page: a cookie jar, same-origin
// requests, and the session CSRF token on mutations.
type consoleBrowser struct {
	origin string
	http   *http.Client
	csrf   string
}

func (b *consoleBrowser) do(ctx context.Context, method, path string, body any, csrf bool) (int, []byte, error) {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		reader = bytes.NewReader(raw)
	}
	r, err := http.NewRequestWithContext(ctx, method, b.origin+path, reader)
	if err != nil {
		return 0, nil, err
	}
	r.Header.Set("Origin", b.origin)
	if body != nil {
		r.Header.Set("Content-Type", "application/json")
	}
	if csrf {
		r.Header.Set("X-CSRF-Token", b.csrf)
	}
	resp, err := b.http.Do(r)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(resp.Body)
	return resp.StatusCode, data, err
}

type fleetLockFixture struct {
	t       *testing.T
	ctx     context.Context
	relay   bool
	url     string
	account string // Fleet owner (user role) account key.
	options transport.Options
	workers []*node.Node
	effects []*atomic.Int64
	browser *consoleBrowser
}

func newFleetLockFixture(t *testing.T, relay bool) *fleetLockFixture {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	t.Cleanup(cancel)
	g, err := gateway.New(t.TempDir(), "", gateway.Options{SuperuserKey: fleetLockSuperuser})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(g.Handler())
	t.Cleanup(func() { server.Close(); _ = g.Close() })
	f := &fleetLockFixture{t: t, ctx: ctx, relay: relay, url: server.URL, options: transporttest.Loopback()}
	operator := client.Admin{URL: server.URL, Key: fleetLockSuperuser}
	var user struct{ Token string }
	if err := operator.JSON(ctx, http.MethodPost, "/v1/admin/users", map[string]string{"name": "alice"}, &user); err != nil || user.Token == "" {
		t.Fatal("create fleet owner", err)
	}
	f.account = user.Token
	if err := (client.Admin{URL: server.URL, Key: f.account}).JSON(ctx, http.MethodPut, "/v1/console/password", map[string]string{"password": fleetLockPassword}, nil); err != nil {
		t.Fatal("set console password", err)
	}
	for _, name := range fleetLockNames {
		effects := &atomic.Int64{}
		w, err := node.New(node.Config{Name: name, Gateway: server.URL, Token: f.account, DataDir: t.TempDir(), WorkDir: t.TempDir(), RelayOnly: relay}, f.options)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = w.Close() })
		if err := w.Register(fleetLockCounter{effects}); err != nil {
			t.Fatal(err)
		}
		if err := w.Start(ctx); err != nil {
			t.Fatal(err)
		}
		f.workers, f.effects = append(f.workers, w), append(f.effects, effects)
	}
	f.browser = f.login("alice", fleetLockPassword)
	return f
}

func (f *fleetLockFixture) login(username, password string) *consoleBrowser {
	f.t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		f.t.Fatal(err)
	}
	b := &consoleBrowser{origin: f.url, http: &http.Client{Jar: jar, Timeout: 15 * time.Second}}
	status, body, err := b.do(f.ctx, http.MethodPost, "/console/api/login", map[string]string{"username": username, "password": password}, false)
	if err != nil || status != http.StatusOK {
		f.t.Fatalf("console login: %d %s %v", status, body, err)
	}
	var session struct{ CSRF string }
	status, body, err = b.do(f.ctx, http.MethodGet, "/console/api/session", nil, false)
	if err != nil || status != http.StatusOK || json.Unmarshal(body, &session) != nil || session.CSRF == "" {
		f.t.Fatalf("console session: %d %s %v", status, body, err)
	}
	b.csrf = session.CSRF
	return b
}

// orchestratorKey creates an execution-only fleet key and returns its ID and secret.
func (f *fleetLockFixture) orchestratorKey(name string) (string, string) {
	f.t.Helper()
	var created struct {
		Key   struct{ ID, Role string }
		Token string
	}
	if err := (client.Admin{URL: f.url, Key: f.account}).JSON(f.ctx, http.MethodPost, "/v1/fleet/keys", map[string]string{"name": name, "role": "orchestrator"}, &created); err != nil || created.Token == "" || created.Key.Role != "orchestrator" {
		f.t.Fatal("create orchestrator key", created.Key, err)
	}
	return created.Key.ID, created.Token
}

// client starts a standalone account client. Its stable host identity is
// derived from the gateway, the fleet, and stateDir, not from key.
func (f *fleetLockFixture) client(key, stateDir string) client.Client {
	f.t.Helper()
	c := (client.Client{}).WithStandalone(f.ctx, client.StandaloneConfig{Gateway: client.Admin{URL: f.url, Key: key}, StateDir: stateDir, RelayOnly: f.relay, AccountRouting: true, PeerOptions: []transport.Options{f.options}})
	f.t.Cleanup(func() { _ = c.Close() })
	return c
}

func (f *fleetLockFixture) owner(c client.Client) string {
	f.t.Helper()
	info, err := c.Session(f.ctx)
	if err != nil || info.OwnerID == "" {
		f.t.Fatal("client session", info, err)
	}
	return info.OwnerID
}

func (f *fleetLockFixture) machine(i int) string { return f.workers[i].Identity.ID }

func (f *fleetLockFixture) policy() fleetVaultPolicy {
	f.t.Helper()
	status, body, err := f.browser.do(f.ctx, http.MethodGet, "/console/api/fleet/vault", nil, false)
	var p fleetVaultPolicy
	if err != nil || status != http.StatusOK || json.Unmarshal(body, &p) != nil || p.Revision < 0 {
		f.t.Fatalf("read fleet lock: %d %s %v", status, body, err)
	}
	return p
}

// setLock changes the lock and returns the acknowledged policy.
func (f *fleetLockFixture) setLock(enabled bool, pauseSeconds int64) fleetVaultPolicy {
	f.t.Helper()
	before := f.policy()
	q := map[string]any{"enabled": enabled, "revision": before.Revision}
	if pauseSeconds != 0 {
		q["pauseSeconds"] = pauseSeconds
	}
	status, body, err := f.browser.do(f.ctx, http.MethodPut, "/console/api/fleet/vault", q, true)
	var after fleetVaultPolicy
	if err != nil || status != http.StatusOK || json.Unmarshal(body, &after) != nil {
		f.t.Fatalf("set fleet lock enabled=%v pause=%d: %d %s %v", enabled, pauseSeconds, status, body, err)
	}
	if after.Enabled != enabled || after.Revision <= before.Revision {
		f.t.Fatalf("fleet lock change was not acknowledged: before=%+v after=%+v", before, after)
	}
	return after
}

func allowanceRequest(revision int64, keyID, ownerID string, machines []string, ttlSeconds int64) map[string]any {
	q := map[string]any{"revision": revision, "allMachines": false, "machineIds": machines}
	if keyID != "" {
		q["keyId"] = keyID
	}
	if ownerID != "" {
		q["ownerId"] = ownerID
	}
	if ttlSeconds != 0 {
		q["ttlSeconds"] = ttlSeconds
	}
	return q
}

// allow adds one allowance through the browser API and returns it as the
// acknowledging response listed it.
func (f *fleetLockFixture) allow(keyID, ownerID string, machines []string, ttlSeconds int64) fleetVaultAllowance {
	f.t.Helper()
	before := f.policy()
	status, body, err := f.browser.do(f.ctx, http.MethodPost, "/console/api/fleet/vault/allowances", allowanceRequest(before.Revision, keyID, ownerID, machines, ttlSeconds), true)
	var after fleetVaultPolicy
	if err != nil || status < 200 || status > 299 || json.Unmarshal(body, &after) != nil {
		f.t.Fatalf("add allowance: %d %s %v", status, body, err)
	}
	if after.Revision <= before.Revision {
		f.t.Fatalf("allowance did not advance the revision: %d -> %d", before.Revision, after.Revision)
	}
	for _, a := range after.Allowances {
		if !slices.ContainsFunc(before.Allowances, func(b fleetVaultAllowance) bool { return b.ID == a.ID }) {
			if a.ID == "" || a.KeyID != keyID || a.OwnerID != ownerID || a.AllMachines || !sameSet(a.MachineIDs, machines) || (ttlSeconds == 0) != a.ExpiresAt.IsZero() {
				f.t.Fatalf("stored allowance differs from request: %+v", a)
			}
			return a
		}
	}
	f.t.Fatalf("new allowance is not listed: %s", body)
	return fleetVaultAllowance{}
}

func (f *fleetLockFixture) removeAllowance(id string) {
	f.t.Helper()
	before := f.policy()
	status, body, err := f.browser.do(f.ctx, http.MethodDelete, "/console/api/fleet/vault/allowances/"+id, map[string]any{"revision": before.Revision}, true)
	if err != nil || status < 200 || status > 299 {
		f.t.Fatalf("remove allowance: %d %s %v", status, body, err)
	}
	if after := f.policy(); slices.ContainsFunc(after.Allowances, func(a fleetVaultAllowance) bool { return a.ID == id }) {
		f.t.Fatal("removed allowance is still listed")
	}
}

func sameSet(a, b []string) bool {
	a, b = slices.Clone(a), slices.Clone(b)
	slices.Sort(a)
	slices.Sort(b)
	return slices.Equal(a, b)
}

// waitFor sleeps until a deadline the gateway returned, which must fall within
// limit of now. Natural expiry is checked by the first request afterwards.
func (f *fleetLockFixture) waitFor(deadline time.Time, limit time.Duration, what string) {
	f.t.Helper()
	remaining := time.Until(deadline)
	if deadline.IsZero() || remaining > limit {
		f.t.Fatalf("%s: returned deadline %v is not within %v", what, deadline, limit)
	}
	select {
	case <-f.ctx.Done():
		f.t.Fatalf("fixture expired waiting for %s", what)
	case <-time.After(remaining):
	}
}

func (f *fleetLockFixture) effectCounts() []int64 {
	counts := make([]int64, len(f.effects))
	for i, e := range f.effects {
		counts[i] = e.Load()
	}
	return counts
}

// attempt makes one execution request to worker i. It is never retried.
func (f *fleetLockFixture) attempt(c client.Client, i int) error {
	ctx, cancel := context.WithTimeout(f.ctx, 10*time.Second)
	defer cancel()
	var out json.RawMessage
	return c.Call(ctx, fleetLockNames[i], "test.effect", map[string]any{}, &out)
}

// allowed requires one first attempt on worker i to run exactly one effect,
// on that worker only.
func (f *fleetLockFixture) allowed(c client.Client, i int, what string) {
	f.t.Helper()
	before := f.effectCounts()
	if err := f.attempt(c, i); err != nil {
		f.t.Fatalf("%s: %s refused: %v", what, fleetLockNames[i], err)
	}
	f.ran(before, i, what)
}

// ran requires exactly one effect, on worker i, since before.
func (f *fleetLockFixture) ran(before []int64, i int, what string) {
	f.t.Helper()
	want := slices.Clone(before)
	want[i]++
	if after := f.effectCounts(); !slices.Equal(after, want) {
		f.t.Fatalf("%s: effects %v -> %v, want one on %s", what, before, after, fleetLockNames[i])
	}
}

// denied requires one first attempt on worker i to be refused by the lock.
func (f *fleetLockFixture) denied(c client.Client, i int, what string) {
	f.t.Helper()
	f.refused(what+" on "+fleetLockNames[i], func() error { return f.attempt(c, i) })
}

// refused requires call to fail with a fleet lock refusal before admission,
// and no worker in the fleet to have run an effect.
func (f *fleetLockFixture) refused(what string, call func() error) {
	f.t.Helper()
	before := f.effectCounts()
	err := call()
	var rpc *model.RPCError
	if err == nil || !errors.As(err, &rpc) || rpc.Code != model.FleetVaultDenied || rpc.Outcome != model.OutcomeNotAccepted {
		f.t.Fatalf("%s: want %s/%s refusal, got %v", what, model.FleetVaultDenied, model.OutcomeNotAccepted, err)
	}
	if after := f.effectCounts(); !slices.Equal(before, after) {
		f.t.Fatalf("%s: refused call dispatched effects %v -> %v", what, before, after)
	}
}

// delegated calls worker dest from worker src with pre-issued grants.
func (f *fleetLockFixture) delegated(src, dest int, grants ...model.Delegation) error {
	ctx, cancel := context.WithTimeout(model.WithDelegations(f.ctx, grants), 10*time.Second)
	defer cancel()
	var out json.RawMessage
	return f.workers[src].Call(ctx, fleetLockNames[dest], "test.effect", map[string]any{}, &out)
}

// grant issues a synchronous test.effect grant on dest for subject src.
func (f *fleetLockFixture) grant(c client.Client, src, dest int) model.Delegation {
	f.t.Helper()
	ctx, cancel := context.WithTimeout(f.ctx, 10*time.Second)
	defer cancel()
	var g model.Delegation
	request := model.Delegation{Subject: fleetLockNames[src], Method: "test.effect", Params: model.JSON(map[string]any{})}
	if err := c.Call(ctx, fleetLockNames[dest], "access.grant", request, &g); err != nil || g.ID == "" {
		f.t.Fatal("pre-issue grant", g, err)
	}
	return g
}

// peersCall instructs worker via to run test.effect on worker target.
func (f *fleetLockFixture) peersCall(c client.Client, via, target int) error {
	ctx, cancel := context.WithTimeout(f.ctx, 10*time.Second)
	defer cancel()
	var out json.RawMessage
	return c.Call(ctx, fleetLockNames[via], "peers.call", map[string]any{"target": fleetLockNames[target], "method": "test.effect", "params": map[string]any{}}, &out)
}

// warm establishes c's sessions to the listed workers before the lock, runs
// one effect on each, and checks the sessions use this run's transport.
func (f *fleetLockFixture) warm(c client.Client, workers ...int) {
	f.t.Helper()
	for _, i := range workers {
		f.allowed(c, i, "before lock")
	}
	mode := "webrtc"
	if f.relay {
		mode = "relay"
	}
	deadline := time.Now().Add(20 * time.Second)
	for {
		info, err := c.Session(f.ctx)
		ready := err == nil
		for _, i := range workers {
			ready = ready && info.Connections[f.machine(i)] == mode
		}
		if ready {
			return
		}
		if time.Now().After(deadline) {
			f.t.Fatalf("sessions did not reach %s: %v %v", mode, info.Connections, err)
		}
		select {
		case <-f.ctx.Done():
			f.t.Fatal("fixture expired warming sessions")
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// warmWorkers opens worker-to-worker sessions with discovery, which needs no grant.
func (f *fleetLockFixture) warmWorkers(pairs ...[2]int) {
	f.t.Helper()
	for _, pair := range pairs {
		if err := f.workers[pair[0]].Call(f.ctx, fleetLockNames[pair[1]], "capabilities.list", map[string]any{}, nil); err != nil {
			f.t.Fatal("warm worker session", pair, err)
		}
	}
}

func forTransports(t *testing.T, run func(t *testing.T, relay bool)) {
	for _, relay := range []bool{true, false} {
		t.Run(fmt.Sprintf("relay=%v", relay), func(t *testing.T) {
			t.Parallel()
			run(t, relay)
		})
	}
}

func containsID(list []fleetVaultEntry, id string) bool {
	return slices.ContainsFunc(list, func(e fleetVaultEntry) bool { return e.ID == id })
}

func TestFleetLockScopesExecutionToAllowedKeyHostAndMachines(t *testing.T) {
	t.Parallel()
	forTransports(t, func(t *testing.T, relay bool) {
		f := newFleetLockFixture(t, relay)
		const A, B, C = 0, 1, 2
		keyID, key := f.orchestratorKey("scoped")
		otherKeyID, otherKey := f.orchestratorKey("other")
		hostDir := t.TempDir()
		c := f.client(key, hostDir)
		host := f.owner(c)
		// The same host with another execution key, and the same key on a
		// second host. Both differ from the allowance in exactly one selector.
		sameHost := f.client(otherKey, hostDir)
		if f.owner(sameHost) != host {
			t.Fatal("another key on the same state directory changed the stable host")
		}
		secondHost := f.client(key, t.TempDir())
		otherHost := f.owner(secondHost)
		if otherHost == host {
			t.Fatal("a second state directory reused the stable host")
		}
		account := f.client(f.account, t.TempDir())
		f.warm(c, A, B, C)
		f.warm(sameHost, A, C)
		f.warm(secondHost, A)
		f.warm(account, A)
		f.warmWorkers([2]int{A, B}, [2]int{A, C}, [2]int{C, B})

		// Grants issued before the lock by the principal that is allowed later.
		lockedAB := f.grant(c, A, B)
		allowedAB := f.grant(c, A, B)
		preAC := f.grant(c, A, C)
		preCB := f.grant(c, C, B)

		policy := f.setLock(true, 0)
		if !containsID(policy.Keys, keyID) || !containsID(policy.Keys, otherKeyID) {
			t.Fatalf("policy omits fleet keys: %+v", policy.Keys)
		}
		if !containsID(policy.Hosts, host) || !containsID(policy.Hosts, otherHost) {
			t.Fatalf("policy omits stable hosts %s, %s: %+v", host, otherHost, policy.Hosts)
		}
		for i := range f.workers {
			if !slices.Contains(policy.Machines, fleetVaultEntry{ID: f.machine(i), Name: fleetLockNames[i]}) {
				t.Fatalf("policy omits machine %s: %+v", fleetLockNames[i], policy.Machines)
			}
		}

		// Warmed sessions cannot execute anywhere once the lock is acknowledged,
		// and a pre-issued grant does not bypass it.
		for i := range f.workers {
			f.denied(c, i, "warmed session under lock")
		}
		f.denied(account, A, "account key without allowance")
		f.refused("pre-issued A->B grant without allowance", func() error { return f.delegated(A, B, lockedAB) })

		allowance := f.allow(keyID, host, []string{f.machine(A), f.machine(B)}, 0)
		f.allowed(c, A, "allowed key and host")
		f.allowed(c, B, "allowed key and host")
		f.denied(c, C, "machine outside the allowance")

		// Instruction-bound delegation stays inside the allowance.
		before := f.effectCounts()
		if err := f.peersCall(c, A, B); err != nil {
			t.Fatal("instruction A->B refused", err)
		}
		f.ran(before, B, "instruction A->B")
		before = f.effectCounts()
		if err := f.delegated(A, B, allowedAB); err != nil {
			t.Fatal("pre-issued A->B grant inside the allowance refused", err)
		}
		f.ran(before, B, "pre-issued A->B grant")
		f.refused("instruction A->C", func() error { return f.peersCall(c, A, C) })
		f.refused("pre-issued A->C grant", func() error { return f.delegated(A, C, preAC) })
		f.refused("pre-issued C->B grant", func() error { return f.delegated(C, B, preCB) })
		f.refused("instruction C->B", func() error { return f.peersCall(c, C, B) })

		// A key and host rule needs both selectors.
		f.denied(sameHost, A, "another key on the allowed host")
		f.denied(secondHost, A, "allowed key on a second host")
		f.denied(account, A, "account key while another credential is allowed")

		// Another rule for the same host's other key admits only that key and
		// its machine, and leaves the scoped allowance in force.
		extra := f.allow(otherKeyID, host, []string{f.machine(C)}, 0)
		f.allowed(sameHost, C, "second key with its own rule")
		f.denied(sameHost, A, "second key outside its own rule")
		f.denied(c, C, "first key outside its rule")
		f.allowed(c, A, "scoped allowance after an unrelated rule")
		f.removeAllowance(extra.ID)
		f.denied(sameHost, C, "removed second-key rule")
		f.allowed(c, A, "scoped allowance after an unrelated removal")

		// The allowance continues for a new process on the same host.
		if err := c.Close(); err != nil {
			t.Fatal(err)
		}
		restarted := f.client(key, hostDir)
		if f.owner(restarted) != host {
			t.Fatal("restart changed the stable host")
		}
		f.allowed(restarted, A, "restarted client on the allowed host")
		f.denied(restarted, C, "restarted client outside the allowance")

		f.removeAllowance(allowance.ID)
		f.denied(restarted, A, "removed allowance")
		f.denied(restarted, B, "removed allowance")

		f.setLock(false, 0)
		f.allowed(restarted, C, "unlocked fleet")
		f.allowed(account, A, "account key in unlocked fleet")
		f.allowed(secondHost, A, "second host in unlocked fleet")
	})
}

func TestFleetLockDisjointAllowancesDoNotCombine(t *testing.T) {
	t.Parallel()
	forTransports(t, func(t *testing.T, relay bool) {
		f := newFleetLockFixture(t, relay)
		const A, B, C = 0, 1, 2
		keyID, key := f.orchestratorKey("split")
		_, otherKey := f.orchestratorKey("other")
		hostDir := t.TempDir()
		c := f.client(key, hostDir)
		host := f.owner(c)
		sameHost := f.client(otherKey, hostDir)
		secondHost := f.client(key, t.TempDir())
		f.warm(c, A, B, C)
		f.warm(sameHost, A, B)
		f.warm(secondHost, A, B)
		f.warmWorkers([2]int{A, B}, [2]int{B, A})
		splitAB := f.grant(c, A, B)
		selectorAB := f.grant(c, A, B)

		// Rules split by machine: each hop is covered, the pair is not.
		f.setLock(true, 0)
		first := f.allow(keyID, host, []string{f.machine(A)}, 0)
		second := f.allow(keyID, host, []string{f.machine(B)}, 0)
		f.allowed(c, A, "first rule")
		f.allowed(c, B, "second rule")
		f.denied(c, C, "machine in neither rule")
		f.refused("instruction A->B across machine rules", func() error { return f.peersCall(c, A, B) })
		f.refused("pre-issued A->B grant across machine rules", func() error { return f.delegated(A, B, splitAB) })
		f.refused("instruction B->A across machine rules", func() error { return f.peersCall(c, B, A) })
		f.removeAllowance(first.ID)
		f.removeAllowance(second.ID)

		// Rules split by selector: a key-only rule and a host-only rule each
		// admit their own selector, but never combine into one principal's pair.
		f.allow(keyID, "", []string{f.machine(A)}, 0)
		f.allow("", host, []string{f.machine(B)}, 0)
		f.allowed(c, A, "key-only rule")
		f.allowed(secondHost, A, "key-only rule on a second host")
		f.denied(sameHost, A, "key-only rule for another key")
		f.allowed(c, B, "host-only rule")
		f.allowed(sameHost, B, "host-only rule for another key")
		f.denied(secondHost, B, "host-only rule on a second host")
		f.refused("instruction A->B across selector rules", func() error { return f.peersCall(c, A, B) })
		f.refused("pre-issued A->B grant across selector rules", func() error { return f.delegated(A, B, selectorAB) })
	})
}

func TestFleetLockExpiryAndPause(t *testing.T) {
	t.Parallel()
	forTransports(t, func(t *testing.T, relay bool) {
		f := newFleetLockFixture(t, relay)
		const A, C = 0, 2
		keyID, key := f.orchestratorKey("temporary")
		c := f.client(key, t.TempDir())
		host := f.owner(c)
		f.warm(c, A, C)

		// A pause lifts the lock until its returned deadline, then the lock
		// applies again without another operator action.
		f.setLock(true, 0)
		f.denied(c, C, "locked before pause")
		const pause = 2
		paused := f.setLock(true, pause)
		f.allowed(c, C, "paused lock")
		f.waitFor(paused.PausedUntil, pause*time.Second, "pause deadline")
		f.denied(c, C, "pause deadline elapsed")
		if p := f.policy(); !p.Enabled || p.Revision != paused.Revision || !p.PausedUntil.IsZero() {
			t.Fatalf("pause expiry changed stored policy: %+v", p)
		}

		// A time-limited allowance stops authorizing at its returned deadline.
		const lifetime = 2
		temporary := f.allow(keyID, host, []string{f.machine(A)}, lifetime)
		f.allowed(c, A, "temporary allowance")
		f.waitFor(temporary.ExpiresAt, lifetime*time.Second, "allowance deadline")
		f.denied(c, A, "temporary allowance expired")
		f.denied(c, C, "outside temporary allowance")
		if p := f.policy(); slices.ContainsFunc(p.Allowances, func(a fleetVaultAllowance) bool { return a.ID == temporary.ID }) {
			t.Fatal("expired allowance is still listed")
		}

		f.setLock(false, 0)
		f.allowed(c, A, "unlocked after expiry")
	})
}

// The console contract is cookie-bound, CSRF-protected, revisioned, and
// scoped to the signed-in fleet. Another fleet's execution is unaffected.
func TestFleetLockConsoleContract(t *testing.T) {
	t.Parallel()
	f := newFleetLockFixture(t, true)
	const A = 0
	keyID, key := f.orchestratorKey("contract")
	c := f.client(key, t.TempDir())
	host := f.owner(c)
	f.warm(c, A)

	anonymous := &consoleBrowser{origin: f.url, http: &http.Client{Timeout: 10 * time.Second}}
	if status, _, err := anonymous.do(f.ctx, http.MethodGet, "/console/api/fleet/vault", nil, false); err != nil || status != http.StatusUnauthorized {
		t.Fatal("anonymous read", status, err)
	}
	// Bearer credentials never reach the cookie-only console API.
	bearer, err := http.NewRequestWithContext(f.ctx, http.MethodGet, f.url+"/console/api/fleet/vault", nil)
	if err != nil {
		t.Fatal(err)
	}
	bearer.Header.Set("Authorization", "Bearer "+f.account)
	resp, err := http.DefaultClient.Do(bearer)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatal("bearer read", resp.StatusCode)
	}

	initial := f.policy()
	if initial.Enabled || len(initial.Allowances) != 0 {
		t.Fatalf("new fleet starts locked: %+v", initial)
	}
	put := map[string]any{"enabled": true, "revision": initial.Revision}
	if status, _, _ := f.browser.do(f.ctx, http.MethodPut, "/console/api/fleet/vault", put, false); status != http.StatusForbidden {
		t.Fatal("mutation without CSRF token", status)
	}
	if status, _, _ := f.browser.do(f.ctx, http.MethodPut, "/console/api/fleet/vault", map[string]any{"enabled": true, "revision": initial.Revision + 7}, true); status != http.StatusConflict {
		t.Fatal("stale revision accepted", status)
	}
	if p := f.policy(); p.Enabled || p.Revision != initial.Revision {
		t.Fatalf("refused mutation changed policy: %+v", p)
	}
	f.allowed(c, A, "refused mutations")

	// A second fleet's machine and owner are not addressable from this one.
	operator := client.Admin{URL: f.url, Key: fleetLockSuperuser}
	var bob struct{ Token string }
	if err := operator.JSON(f.ctx, http.MethodPost, "/v1/admin/users", map[string]string{"name": "bob"}, &bob); err != nil {
		t.Fatal(err)
	}
	bobEffects := &atomic.Int64{}
	foreign, err := node.New(node.Config{Name: "bravo", Gateway: f.url, Token: bob.Token, DataDir: t.TempDir(), WorkDir: t.TempDir(), RelayOnly: true}, f.options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = foreign.Close() })
	if err := foreign.Register(fleetLockCounter{bobEffects}); err != nil {
		t.Fatal(err)
	}
	if err := foreign.Start(f.ctx); err != nil {
		t.Fatal(err)
	}
	bobClient := f.client(bob.Token, t.TempDir())
	bobHost := f.owner(bobClient)

	locked := f.setLock(true, 0)
	if containsID(locked.Machines, foreign.Identity.ID) || containsID(locked.Hosts, bobHost) {
		t.Fatalf("policy lists another fleet's machines or hosts: %+v", locked)
	}
	for _, bad := range []map[string]any{
		allowanceRequest(locked.Revision, keyID, host, []string{foreign.Identity.ID}, 0),
		allowanceRequest(locked.Revision, keyID, bobHost, []string{f.machine(A)}, 0),
		allowanceRequest(locked.Revision, "missing-key", host, []string{f.machine(A)}, 0),
		allowanceRequest(locked.Revision, keyID, "missing-host", []string{f.machine(A)}, 0),
		allowanceRequest(locked.Revision, "", "", []string{f.machine(A)}, 0),
		allowanceRequest(locked.Revision, keyID, host, []string{}, 0),
		allowanceRequest(locked.Revision-1, keyID, host, []string{f.machine(A)}, 0),
	} {
		status, body, _ := f.browser.do(f.ctx, http.MethodPost, "/console/api/fleet/vault/allowances", bad, true)
		if status < 400 || status > 499 {
			t.Fatalf("accepted invalid allowance %v: %d %s", bad, status, body)
		}
	}
	if p := f.policy(); len(p.Allowances) != 0 || p.Revision != locked.Revision {
		t.Fatalf("refused allowances changed policy: %+v", p)
	}
	f.denied(c, A, "locked without allowance")

	// The other fleet keeps executing while this one is locked.
	var out json.RawMessage
	if err := bobClient.Call(f.ctx, "bravo", "test.effect", map[string]any{}, &out); err != nil || bobEffects.Load() != 1 {
		t.Fatal("another fleet's execution was blocked", bobEffects.Load(), err)
	}
	allowance := f.allow(keyID, host, []string{f.machine(A)}, 0)
	f.allowed(c, A, "allowance")
	stale := map[string]any{"revision": locked.Revision}
	if status, _, _ := f.browser.do(f.ctx, http.MethodDelete, "/console/api/fleet/vault/allowances/"+allowance.ID, stale, true); status != http.StatusConflict {
		t.Fatal("stale allowance removal", status)
	}
	f.allowed(c, A, "refused removal")
}
