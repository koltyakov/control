package client

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/koltyakov/control/internal/buildinfo"
	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/update"
)

const doctorTestKey = "doctor-test-account-key-0123456789abcdef"

// doctorGateway is a recording fake gateway. Every request is logged so tests
// can assert that doctor only reads.
type doctorGateway struct {
	mu        sync.Mutex
	requests  []string
	auth      map[string]any
	authCode  int
	status    model.PoolActivitySnapshot
	skew      time.Duration
	selection update.Selection
	nodes     []model.Node
	hang      map[string]bool
	server    *httptest.Server
}

func newDoctorGateway(t *testing.T) *doctorGateway {
	t.Helper()
	g := &doctorGateway{
		auth: map[string]any{"role": "user", "userId": "u1", "username": "alice", "delegation": true, "gatewayProtocol": 1},
		hang: map[string]bool{},
	}
	g.status.Gateway = &model.GatewaySnapshot{Software: buildinfo.Info{Version: "v1.4.0"}}
	g.server = httptest.NewServer(http.HandlerFunc(g.serve))
	t.Cleanup(g.server.Close)
	return g
}

func (g *doctorGateway) set(change func(*doctorGateway)) {
	g.mu.Lock()
	defer g.mu.Unlock()
	change(g)
}

func (g *doctorGateway) log() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.requests)
}

func (g *doctorGateway) serve(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	g.requests = append(g.requests, r.Method+" "+r.URL.Path)
	hang, authCode, auth, status, skew, selection, nodes := g.hang[r.URL.Path], g.authCode, g.auth, g.status, g.skew, g.selection, slices.Clone(g.nodes)
	g.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer "+doctorTestKey {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if hang {
		<-r.Context().Done()
		return
	}
	var value any
	switch r.URL.Path {
	case "/v1/auth":
		if authCode != 0 {
			http.Error(w, "unauthorized", authCode)
			return
		}
		value = auth
	case "/v1/status":
		status.ObservedAt = time.Now().Add(skew)
		value = status
	case "/v1/updates":
		value = selection
	case "/v1/nodes":
		value = nodes
	default:
		http.NotFound(w, r)
		return
	}
	_ = json.NewEncoder(w).Encode(value)
}

// fakeDoctorPeer records every peer read. It has no grant, task or lease
// surface at all; tests also assert the methods it saw.
type fakeDoctorPeer struct {
	mu          sync.Mutex
	calls       []string
	describe    doctorDescribe
	describeErr error
	diagnostics map[string]any
	diagErr     error
	sessions    SessionInfo
	hang        chan struct{}
}

func (p *fakeDoctorPeer) observe(_ context.Context, target, method string, _, result any) error {
	p.mu.Lock()
	p.calls = append(p.calls, target+" "+method)
	hang := p.hang
	p.mu.Unlock()
	if hang != nil {
		<-hang // deliberately ignores cancellation
	}
	var value any
	var err error
	switch method {
	case "node.describe":
		value, err = p.describe, p.describeErr
	case "diagnostics.get":
		value, err = p.diagnostics, p.diagErr
	}
	if err != nil {
		return err
	}
	b, _ := json.Marshal(value)
	return json.Unmarshal(b, result)
}

func (p *fakeDoctorPeer) session(context.Context) (SessionInfo, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, "session")
	return p.sessions, nil
}

func (p *fakeDoctorPeer) log() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.calls)
}

// doctorLocalAPI is a recording fake local node API.
func doctorLocalAPI(t *testing.T, methods *[]string, mu *sync.Mutex) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var call model.APICall
		_ = json.NewDecoder(r.Body).Decode(&call)
		mu.Lock()
		*methods = append(*methods, r.Method+" "+r.URL.Path+" "+call.Method)
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(model.Response{Result: json.RawMessage(`{"concurrency":{"execution":{"limit":4,"inUse":0}}}`)})
	}))
	t.Cleanup(server.Close)
	return server.URL
}

// closedLocalAPI returns a loopback URL that refuses connections.
func closedLocalAPI(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := l.Addr().String()
	_ = l.Close()
	return "http://" + address
}

var doctorSoftware = buildinfo.Info{Version: "v1.4.0", OS: "linux", Arch: "amd64", NodeGatewayMin: 0, NodeGatewayMax: 1}

func doctorMachine(name string) (model.Node, model.NodeActivitySnapshot) {
	software := buildinfo.Info{Version: "v1.4.0", OS: "linux", Arch: "amd64", NodeGatewayMax: 1}
	n := model.Node{ID: "id-" + name + "-0123456789abcdef", Name: name, OS: "linux", Online: true, InstructionDelegation: true, Software: software, LastSeen: time.Now().Add(-time.Hour), RunAs: "user"}
	return n, model.NodeActivitySnapshot{ID: n.ID, Name: name, OS: "linux", Online: true, Software: software, Status: "online", LastSeen: n.LastSeen}
}

func healthyDoctorPeer(id string) *fakeDoctorPeer {
	queued := 0
	return &fakeDoctorPeer{
		describe:    doctorDescribe{Concurrency: &model.NodeConcurrency{Execution: model.ConcurrencyPool{Limit: 4, InUse: 1, Queued: &queued}, Operations: model.ConcurrencyPool{Limit: 16}}},
		diagnostics: map[string]any{"tasks": map[string]any{"active": 1}, "artifacts": map[string]any{"usedBytes": 10, "maxBytes": 1000}, "mcp": map[string]any{"sessions": 0, "starting": 0}, "transport": map[string]any{}},
		sessions:    SessionInfo{Role: "client", Sessions: []model.PeerSession{{ID: "s1", Peer: id, Lane: "control", Mode: "webrtc", Route: &model.PeerRoute{Kind: "direct"}}}},
	}
}

func doctorOptions(g *doctorGateway, node string) DoctorOptions {
	software := doctorSoftware
	return DoctorOptions{
		Node: node, Gateway: Admin{URL: g.server.URL, Key: doctorTestKey}, LoginSource: "saved login",
		Timeout: 5 * time.Second, CheckTimeout: 2 * time.Second, Software: &software,
		Getenv: func(string) string { return "" },
	}
}

func doctorFind(t *testing.T, r DoctorReport, name string) DoctorCheck {
	t.Helper()
	for _, c := range r.Checks {
		if c.Check == name {
			return c
		}
	}
	t.Fatalf("report has no %q check: %+v", name, r.Checks)
	return DoctorCheck{}
}

func doctorExpect(t *testing.T, r DoctorReport, want map[string]string) {
	t.Helper()
	for name, status := range want {
		if c := doctorFind(t, r, name); c.Status != status {
			t.Errorf("%s = %s (%s; hint %q), want %s", name, c.Status, c.Detail, c.Hint, status)
		}
	}
}

// assertReadOnly fails when doctor sent anything but the allowed reads.
func assertReadOnly(t *testing.T, g *doctorGateway, p *fakeDoctorPeer) {
	t.Helper()
	allowed := map[string]bool{"GET /v1/auth": true, "GET /v1/status": true, "GET /v1/updates": true, "GET /v1/nodes": true}
	for _, request := range g.log() {
		if !allowed[request] {
			t.Errorf("doctor sent gateway request %q", request)
		}
	}
	if p == nil {
		return
	}
	for _, call := range p.log() {
		method := call[strings.LastIndex(call, " ")+1:]
		if method != "node.describe" && method != "diagnostics.get" && call != "session" {
			t.Errorf("doctor sent peer request %q", call)
		}
	}
}

func TestDoctorHealthyMachine(t *testing.T) {
	g := newDoctorGateway(t)
	n, s := doctorMachine("worker")
	g.set(func(g *doctorGateway) {
		g.nodes, g.status.Nodes = []model.Node{n}, []model.NodeActivitySnapshot{s}
		g.selection = update.Selection{ID: "d1", Manifest: update.Manifest{Version: "v1.4.0"}}
	})
	var mu sync.Mutex
	var local []string
	opts := doctorOptions(g, "worker")
	opts.LocalAPI, opts.LocalNode = doctorLocalAPI(t, &local, &mu), "laptop"
	p := healthyDoctorPeer(n.ID)
	r := runDoctor(t.Context(), opts, p)
	doctorExpect(t, r, map[string]string{
		"login": DoctorOK, "gateway": DoctorOK, "version": DoctorOK, "clock": DoctorOK, "local-node": DoctorOK, "environment": DoctorOK,
		"machine": DoctorOK, "machine-version": DoctorOK, "peer-session": DoctorOK, "concurrency": DoctorOK, "diagnostics": DoctorOK,
	})
	if r.Failed() || r.Summary.Status != DoctorOK || r.Summary.OK != len(r.Checks) || r.Node != "worker" {
		t.Fatalf("summary %+v", r.Summary)
	}
	if c := doctorFind(t, r, "peer-session"); !strings.Contains(c.Detail, "control: webrtc direct") {
		t.Fatal(c.Detail)
	}
	if c := doctorFind(t, r, "login"); !strings.Contains(c.Detail, "user alice") || !strings.Contains(c.Detail, "saved login") {
		t.Fatal(c.Detail)
	}
	assertReadOnly(t, g, p)
	if got := p.log(); !slices.Contains(got, n.ID+" node.describe") || !slices.Contains(got, n.ID+" diagnostics.get") {
		t.Fatal(got)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(local) != 1 || local[0] != "POST /v1/call node.describe" {
		t.Fatal("local API calls", local)
	}
	b, _ := json.Marshal(r)
	if strings.Contains(string(b), doctorTestKey) {
		t.Fatal("report contains the credential")
	}
}

func TestDoctorWithoutLogin(t *testing.T) {
	software := doctorSoftware
	p := &fakeDoctorPeer{}
	r := runDoctor(t.Context(), DoctorOptions{Node: "worker", LocalAPI: closedLocalAPI(t), Software: &software, Getenv: func(string) string { return "" }}, p)
	doctorExpect(t, r, map[string]string{
		"login": DoctorFailed, "gateway": DoctorSkipped, "version": DoctorSkipped, "clock": DoctorSkipped, "local-node": DoctorOK, "environment": DoctorSkipped,
		"machine": DoctorSkipped, "machine-version": DoctorSkipped, "peer-session": DoctorSkipped, "concurrency": DoctorSkipped, "diagnostics": DoctorSkipped,
	})
	if !r.Failed() || !strings.Contains(doctorFind(t, r, "login").Hint, "control login --gateway URL") {
		t.Fatal(r)
	}
	if len(p.log()) != 0 {
		t.Fatal("peer contacted without a login", p.log())
	}
	// A saved URL without a key is still a failed login, and nothing is sent.
	g := newDoctorGateway(t)
	r = runDoctor(t.Context(), DoctorOptions{Gateway: Admin{URL: g.server.URL}, Software: &software, Getenv: func(string) string { return "" }}, p)
	if c := doctorFind(t, r, "login"); c.Status != DoctorFailed || !strings.Contains(c.Detail, "no credential") {
		t.Fatal(c)
	}
	if len(g.log()) != 0 {
		t.Fatal("gateway contacted without a key", g.log())
	}
}

func TestDoctorRejectedCredentialAndUnreachableGateway(t *testing.T) {
	g := newDoctorGateway(t)
	g.set(func(g *doctorGateway) { g.authCode = http.StatusUnauthorized })
	r := runDoctor(t.Context(), doctorOptions(g, ""), nil)
	doctorExpect(t, r, map[string]string{"login": DoctorFailed, "gateway": DoctorFailed, "version": DoctorSkipped, "fleet": DoctorSkipped})
	if !strings.Contains(doctorFind(t, r, "gateway").Detail, "refused authentication") {
		t.Fatal(doctorFind(t, r, "gateway"))
	}
	assertReadOnly(t, g, nil)

	opts := doctorOptions(g, "")
	opts.Gateway.URL = closedLocalAPI(t)
	r = runDoctor(t.Context(), opts, nil)
	doctorExpect(t, r, map[string]string{"login": DoctorSkipped, "gateway": DoctorFailed, "clock": DoctorSkipped, "fleet": DoctorSkipped})
	if !strings.Contains(doctorFind(t, r, "gateway").Detail, "cannot reach") {
		t.Fatal(doctorFind(t, r, "gateway"))
	}
}

func TestDoctorClockSkew(t *testing.T) {
	for _, test := range []struct {
		skew   time.Duration
		status string
		words  string
	}{
		{0, DoctorOK, "matches"},
		{-3 * time.Second, DoctorOK, "ahead of"},
		{15 * time.Second, DoctorWarning, "behind"},
		{-45 * time.Second, DoctorFailed, "ahead of"},
	} {
		g := newDoctorGateway(t)
		g.set(func(g *doctorGateway) { g.skew = test.skew })
		c := doctorFind(t, runDoctor(t.Context(), doctorOptions(g, ""), nil), "clock")
		if c.Status != test.status || !strings.Contains(c.Detail, test.words) {
			t.Errorf("skew %s: %+v", test.skew, c)
		}
		if test.status != DoctorOK && !strings.Contains(c.Hint, "NTP") {
			t.Errorf("skew %s hint %q", test.skew, c.Hint)
		}
	}
}

func TestDoctorTimeoutsNeverHang(t *testing.T) {
	g := newDoctorGateway(t)
	n, s := doctorMachine("worker")
	g.set(func(g *doctorGateway) {
		g.nodes, g.status.Nodes = []model.Node{n}, []model.NodeActivitySnapshot{s}
		g.hang["/v1/status"] = true
	})
	p := healthyDoctorPeer(n.ID)
	p.hang = make(chan struct{})
	defer close(p.hang)
	opts := doctorOptions(g, "worker")
	opts.CheckTimeout = 300 * time.Millisecond
	started := time.Now()
	r := runDoctor(t.Context(), opts, p)
	if elapsed := time.Since(started); elapsed > 3*time.Second {
		t.Fatalf("doctor took %s", elapsed)
	}
	if c := doctorFind(t, r, "gateway"); c.Status != DoctorWarning || !strings.Contains(c.Detail, "timed out after 300ms") {
		t.Fatal(c)
	}
	doctorExpect(t, r, map[string]string{"clock": DoctorSkipped, "version": DoctorSkipped, "machine": DoctorOK, "peer-session": DoctorFailed, "diagnostics": DoctorSkipped})
	if c := doctorFind(t, r, "peer-session"); !strings.Contains(c.Detail, "timed out") {
		t.Fatal(c)
	}

	// The overall deadline bounds the run even when each check would wait longer.
	opts.Timeout, opts.CheckTimeout = 400*time.Millisecond, 400*time.Millisecond
	started = time.Now()
	r = runDoctor(t.Context(), opts, p)
	if elapsed := time.Since(started); elapsed > 3*time.Second {
		t.Fatalf("doctor took %s", elapsed)
	}
	if c := doctorFind(t, r, "peer-session"); c.Status != DoctorFailed || !strings.Contains(c.Detail, "deadline") {
		t.Fatal(c)
	}
}

// Hints follow the permission each command needs: fleet management for
// control machines enable, execution for control wake, superuser for rollout.
func TestDoctorHintsFollowRoleScopedHelp(t *testing.T) {
	fleetCommands := `machines (enable|disable|add|proxies|invites|revoke|rename|unregister)|\bkeys\b|\baudit\b|\bconsole\b`
	admin := `\busers\b|update (status|push|check|authorize)`
	hidden := map[string]*regexp.Regexp{
		"common":            regexp.MustCompile(fleetCommands + `|` + admin + `|control wake`),
		"orchestrator":      regexp.MustCompile(fleetCommands + `|` + admin),
		"fleet-manager":     regexp.MustCompile(admin + `|control wake`),
		"fleet-manager-run": regexp.MustCompile(admin),
		"user":              regexp.MustCompile(admin),
	}
	runs := map[string]bool{"orchestrator": true, "fleet-manager-run": true, "user": true, "superuser": true}
	manages := map[string]bool{"fleet-manager": true, "fleet-manager-run": true, "user": true, "superuser": true}
	offline, offlineStatus := doctorMachine("sleeper")
	offline.Online, offlineStatus.Online, offlineStatus.Status, offlineStatus.WakeVia = false, false, "offline", "worker"
	offline.Disabled, offlineStatus.Disabled = true, true
	old, oldStatus := doctorMachine("old")
	old.Software.Version, oldStatus.Software.Version = "v1.2.0", "v1.2.0"
	old.Disabled, oldStatus.Disabled = true, true
	for _, role := range []string{"common", "orchestrator", "fleet-manager", "fleet-manager-run", "user", "superuser"} {
		var text []string
		for _, target := range []string{"sleeper", "old"} {
			g := newDoctorGateway(t)
			g.set(func(g *doctorGateway) {
				g.auth["role"] = role
				g.nodes = []model.Node{offline, old}
				g.status.Nodes = []model.NodeActivitySnapshot{offlineStatus, oldStatus}
			})
			p := healthyDoctorPeer(old.ID)
			if !runs[role] {
				p.diagErr = errors.New("worker credentials permit discovery only; execution requires orchestrator delegation")
			}
			r := runDoctor(t.Context(), doctorOptions(g, target), p)
			assertReadOnly(t, g, p)
			for _, c := range r.Checks {
				text = append(text, c.Detail, c.Hint)
			}
			if target == "old" {
				if c := doctorFind(t, r, "machine-version"); c.Status != DoctorWarning {
					t.Errorf("%s machine-version %+v", role, c)
				}
				want := DoctorOK
				if !runs[role] {
					want = DoctorSkipped
					if c := doctorFind(t, r, "diagnostics"); !strings.HasPrefix(c.Detail, "not permitted") || !strings.Contains(c.Detail, "with run permission") {
						t.Errorf("%s diagnostics %+v", role, c)
					}
				}
				doctorExpect(t, r, map[string]string{"diagnostics": want})
			}
		}
		joined := strings.Join(text, "\n")
		if re := hidden[role]; re != nil {
			if m := re.FindString(joined); m != "" {
				t.Errorf("%s report reveals %q", role, m)
			}
		}
		if got := strings.Contains(joined, "control machines enable sleeper"); got != manages[role] {
			t.Errorf("%s enable hint shown = %v, want %v:\n%s", role, got, manages[role], joined)
		}
		if !manages[role] && !strings.Contains(joined, "a fleet-manager key can re-enable it") {
			t.Errorf("%s lacks the re-enable referral:\n%s", role, joined)
		}
		if got := strings.Contains(joined, "control wake sleeper"); got != runs[role] {
			t.Errorf("%s wake hint shown = %v, want %v:\n%s", role, got, runs[role], joined)
		}
		if !runs[role] && !strings.Contains(joined, "check the Control service on that machine") {
			t.Errorf("%s lacks the offline service hint:\n%s", role, joined)
		}
		if manages[role] && role != "superuser" && !strings.Contains(joined, "ask the gateway operator to publish") {
			t.Errorf("%s lacks the release publication hint:\n%s", role, joined)
		}
		switch role {
		case "superuser":
			if !strings.Contains(joined, "control update status") {
				t.Errorf("superuser hints lack rollout status:\n%s", joined)
			}
		}
	}
}

// Named fleet-manager keys are described from their permissions.
func TestDoctorLoginDescribesFleetManagerKeys(t *testing.T) {
	for role, want := range map[string]string{
		"fleet-manager":            "it can manage fleet machines and keys and discover peers, but not run work; it does not administer the gateway",
		"fleet-manager-enroll":     "it can manage fleet machines and keys, discover peers and enroll machines, but not run work;",
		"fleet-manager-run":        "it can manage fleet machines and keys, discover peers and run work; it does not administer the gateway",
		"fleet-manager-enroll-run": "it can manage fleet machines and keys, discover peers, enroll machines and run work;",
	} {
		g := newDoctorGateway(t)
		g.set(func(g *doctorGateway) { g.auth["role"] = role })
		c := doctorFind(t, runDoctor(t.Context(), doctorOptions(g, ""), nil), "login")
		if c.Status != DoctorOK || !strings.HasPrefix(c.Detail, "fleet-manager key from saved login") || !strings.Contains(c.Detail, want) {
			t.Errorf("%s login %+v", role, c)
		}
	}
}

func TestDoctorInstructionOnlyIsNotDiagnosed(t *testing.T) {
	g := newDoctorGateway(t)
	n, s := doctorMachine("host-system")
	n.InstructionOnly, n.HybridRole = true, "system"
	g.set(func(g *doctorGateway) { g.nodes, g.status.Nodes = []model.Node{n}, []model.NodeActivitySnapshot{s} })
	p := healthyDoctorPeer(n.ID)
	r := runDoctor(t.Context(), doctorOptions(g, "host-system"), p)
	c := doctorFind(t, r, "diagnostics")
	if c.Status != DoctorSkipped || !strings.Contains(c.Detail, "never requests") || c.Hint != "control diagnostics host-system authorizes that one read explicitly" {
		t.Fatal(c)
	}
	doctorExpect(t, r, map[string]string{"peer-session": DoctorOK, "concurrency": DoctorOK})
	for _, call := range p.log() {
		if strings.HasSuffix(call, "diagnostics.get") {
			t.Fatal("diagnosed an instruction-only destination")
		}
	}
	assertReadOnly(t, g, p)
}

func TestDoctorVersionAndDelegationGaps(t *testing.T) {
	g := newDoctorGateway(t)
	legacy, legacyStatus := doctorMachine("legacy")
	legacy.InstructionDelegation = false
	incompatible, incompatibleStatus := doctorMachine("incompatible")
	incompatible.Software.NodeGatewayMin, incompatible.Software.NodeGatewayMax = 2, 3
	g.set(func(g *doctorGateway) {
		g.nodes = []model.Node{legacy, incompatible}
		g.status.Nodes = []model.NodeActivitySnapshot{legacyStatus, incompatibleStatus}
		g.status.Gateway.Software.Version = "v1.5.0"
		g.selection.Manifest.Version = "v1.6.0"
	})
	p := healthyDoctorPeer(legacy.ID)
	r := runDoctor(t.Context(), doctorOptions(g, "legacy"), p)
	doctorExpect(t, r, map[string]string{"version": DoctorWarning, "machine-version": DoctorFailed, "peer-session": DoctorSkipped, "diagnostics": DoctorSkipped})
	if c := doctorFind(t, r, "version"); !strings.Contains(c.Hint, "control update --version v1.6.0") {
		t.Fatal(c)
	}
	if len(p.log()) != 0 {
		t.Fatal("sent requests to a worker without delegation support", p.log())
	}
	r = runDoctor(t.Context(), doctorOptions(g, "incompatible"), p)
	if c := doctorFind(t, r, "machine-version"); c.Status != DoctorFailed || !strings.Contains(c.Detail, "protocol 2..3") {
		t.Fatal(c)
	}
	r = runDoctor(t.Context(), doctorOptions(g, "missing"), p)
	if c := doctorFind(t, r, "machine"); c.Status != DoctorFailed || !strings.Contains(c.Hint, "control machines --compact") {
		t.Fatal(c)
	}
	doctorExpect(t, r, map[string]string{"peer-session": DoctorSkipped})
}

func TestDoctorRouteAndPressure(t *testing.T) {
	g := newDoctorGateway(t)
	n, s := doctorMachine("far")
	s.Leased, s.LeaseExpires = true, time.Now().Add(time.Hour)
	g.set(func(g *doctorGateway) { g.nodes, g.status.Nodes = []model.Node{n}, []model.NodeActivitySnapshot{s} })
	queued := 3
	p := healthyDoctorPeer(n.ID)
	p.sessions.Sessions = []model.PeerSession{{Peer: n.ID, Lane: "control", Mode: "relay", Route: &model.PeerRoute{Kind: "gateway-relay"}}}
	p.describe.Concurrency.Execution = model.ConcurrencyPool{Limit: 2, InUse: 2, Queued: &queued}
	p.diagnostics = map[string]any{"tasks": map[string]any{"cleanupPending": 2}, "artifacts": map[string]any{"usedBytes": 95, "maxBytes": 100}, "transport": map[string]any{"linkOverflows": 1}}
	r := runDoctor(t.Context(), doctorOptions(g, "far"), p)
	doctorExpect(t, r, map[string]string{"machine": DoctorWarning, "peer-session": DoctorWarning, "concurrency": DoctorWarning, "diagnostics": DoctorWarning})
	if c := doctorFind(t, r, "peer-session"); !strings.Contains(c.Hint, "control speedtest far") {
		t.Fatal(c)
	}
	if r.Failed() {
		t.Fatal("warnings failed the report", r.Summary)
	}
	opts := doctorOptions(g, "far")
	opts.RelayOnly = true
	if c := doctorFind(t, runDoctor(t.Context(), opts, p), "peer-session"); c.Status != DoctorOK || !strings.Contains(c.Detail, "relay-only") {
		t.Fatal(c)
	}
	p.describeErr = errors.New("peer connection failed")
	r = runDoctor(t.Context(), doctorOptions(g, "far"), p)
	doctorExpect(t, r, map[string]string{"peer-session": DoctorFailed, "concurrency": DoctorSkipped, "diagnostics": DoctorSkipped})
}

func TestDoctorFleetSummaryOpensNoPeerSession(t *testing.T) {
	g := newDoctorGateway(t)
	_, up := doctorMachine("alpha")
	_, down := doctorMachine("beta")
	down.Online, down.Status = false, "offline"
	g.set(func(g *doctorGateway) { g.status.Nodes = []model.NodeActivitySnapshot{down, up} })
	p := &fakeDoctorPeer{}
	r := runDoctor(t.Context(), doctorOptions(g, ""), p)
	doctorExpect(t, r, map[string]string{"fleet": DoctorOK, "machine:alpha": DoctorOK, "machine:beta": DoctorWarning})
	if c := doctorFind(t, r, "machine:beta"); c.Hint != "control doctor beta" {
		t.Fatal(c)
	}
	if r.Failed() || len(p.log()) != 0 || slices.Contains(g.log(), "GET /v1/nodes") {
		t.Fatal(r.Summary, p.log(), g.log())
	}
	if r.Checks[len(r.Checks)-2].Check != "machine:alpha" {
		t.Fatal("machine lines are not ordered by name")
	}
	assertReadOnly(t, g, p)
}

func TestDoctorEnvironment(t *testing.T) {
	env := func(values map[string]string) func(string) string { return func(k string) string { return values[k] } }
	for _, test := range []struct {
		url    string
		env    map[string]string
		status string
		words  string
	}{
		{"https://gw.example", nil, DoctorOK, "no proxy variables"},
		{"https://gw.example", map[string]string{"HTTPS_PROXY": "http://user:secret@proxy.corp:3128"}, DoctorOK, "proxy.corp:3128 from HTTPS_PROXY"},
		{"https://gw.example", map[string]string{"https_proxy": "proxy.corp:3128", "NO_PROXY": ".example"}, DoctorOK, "NO_PROXY excludes"},
		{"http://127.0.0.1:7330", map[string]string{"HTTP_PROXY": "proxy.corp:3128"}, DoctorOK, "no proxy variables"},
		{"http://gw.example", nil, DoctorWarning, "plain HTTP"},
	} {
		d := &doctorRun{opts: DoctorOptions{Gateway: Admin{URL: test.url, Key: "k"}, Getenv: env(test.env)}}
		c := d.environmentCheck()
		if c.Status != test.status || !strings.Contains(c.Detail, test.words) || strings.Contains(c.Detail, "secret") {
			t.Errorf("%s %v: %+v", test.url, test.env, c)
		}
	}
	// An unreachable gateway behind an environment proxy is a warning with a hint.
	d := &doctorRun{opts: DoctorOptions{Gateway: Admin{URL: "https://gw.example", Key: "k"}, Getenv: env(map[string]string{"HTTPS_PROXY": "proxy:8080"})}, authErr: errors.New("dial tcp: i/o timeout")}
	if c := d.environmentCheck(); c.Status != DoctorWarning || !strings.Contains(c.Hint, "NO_PROXY") {
		t.Fatal(c)
	}
	for _, test := range []struct {
		list, host, port string
		want             bool
	}{
		{"*", "a.b", "443", true},
		{"b.example", "a.b.example", "443", true},
		{"b.example", "xb.example", "443", false},
		{".b.example", "b.example", "443", false},
		{"10.0.0.0/8", "10.1.2.3", "443", true},
		{"gw.example:8443", "gw.example", "443", false},
		{"gw.example:443", "gw.example", "443", true},
		{"", "gw.example", "443", false},
	} {
		if got := doctorNoProxy(test.list, test.host, test.port); got != test.want {
			t.Errorf("NO_PROXY %q host %s:%s = %v", test.list, test.host, test.port, got)
		}
	}
}

// The JSON shape is a documented contract: top-level node, checks and summary,
// and every check carries all four fields even when empty.
func TestDoctorJSONShape(t *testing.T) {
	g := newDoctorGateway(t)
	b, err := json.Marshal(runDoctor(t.Context(), doctorOptions(g, ""), nil))
	if err != nil {
		t.Fatal(err)
	}
	var top map[string]json.RawMessage
	if err = json.Unmarshal(b, &top); err != nil {
		t.Fatal(err)
	}
	keys := func(m map[string]json.RawMessage) []string {
		var out []string
		for k := range m {
			out = append(out, k)
		}
		slices.Sort(out)
		return out
	}
	if got := keys(top); !slices.Equal(got, []string{"checks", "node", "summary"}) {
		t.Fatal(got)
	}
	var checks []map[string]json.RawMessage
	if err = json.Unmarshal(top["checks"], &checks); err != nil || len(checks) == 0 {
		t.Fatal(err, len(checks))
	}
	for _, c := range checks {
		if got := keys(c); !slices.Equal(got, []string{"check", "detail", "hint", "status"}) {
			t.Fatal(got)
		}
	}
	var summary map[string]json.RawMessage
	if err = json.Unmarshal(top["summary"], &summary); err != nil {
		t.Fatal(err)
	}
	if got := keys(summary); !slices.Equal(got, []string{"failed", "ok", "skipped", "status", "warning"}) {
		t.Fatal(got)
	}
}

// Against a real gateway and nodes, doctor reads through the account client
// session and leaves no grant or task behind, including at an instruction-only
// destination.
func TestDoctorAgainstPeerCluster(t *testing.T) {
	t.Parallel()
	ctx, c, _, admin := delegationClusterWithTimeout(t, true, 60*time.Second, true)
	opts := DoctorOptions{Node: "first", Gateway: admin, LoginSource: "test", RelayOnly: true, LocalAPI: closedLocalAPI(t), Timeout: 40 * time.Second, CheckTimeout: 20 * time.Second}
	r := c.Doctor(ctx, opts)
	var text strings.Builder
	_ = r.WriteText(&text)
	t.Log("\n" + text.String())
	doctorExpect(t, r, map[string]string{"login": DoctorOK, "gateway": DoctorOK, "machine": DoctorOK, "machine-version": DoctorOK, "peer-session": DoctorOK, "concurrency": DoctorOK, "diagnostics": DoctorOK})
	if c := doctorFind(t, r, "peer-session"); !strings.Contains(c.Detail, "gateway relay") {
		t.Fatal(c)
	}
	opts.Node = "second"
	r = c.Doctor(ctx, opts)
	doctorExpect(t, r, map[string]string{"peer-session": DoctorOK, "diagnostics": DoctorSkipped})
	var grants []model.Delegation
	if err := c.Call(ctx, "second", "access.list", nil, &grants); err != nil || len(grants) != 0 {
		t.Fatal("doctor left grants", grants, err)
	}
	var tasks []model.Task
	if err := c.Call(ctx, "first", "tasks.list", map[string]any{}, &tasks); err != nil || len(tasks) != 0 {
		t.Fatal("doctor started tasks", tasks, err)
	}
}

func TestDoctorTextReport(t *testing.T) {
	r := DoctorReport{Checks: []DoctorCheck{{Check: "login", Status: DoctorFailed, Detail: "no gateway login is configured", Hint: doctorLoginHint}, {Check: "clock", Status: DoctorSkipped, Detail: "no gateway login"}}}
	r.summarize()
	var b strings.Builder
	if err := r.WriteText(&b); err != nil {
		t.Fatal(err)
	}
	want := "failed   login              no gateway login is configured\n" +
		"                            hint: " + doctorLoginHint + "\n" +
		"skipped  clock              no gateway login\n" +
		"\nfailed: 0 ok, 0 warning, 1 failed, 1 skipped\n"
	if b.String() != want {
		t.Fatalf("%q", b.String())
	}
}
