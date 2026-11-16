package gateway

import (
	"context"
	"crypto/ed25519"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/koltyakov/control/internal/buildinfo"
	"github.com/koltyakov/control/internal/enrollment"
	"github.com/koltyakov/control/internal/identity"
	"github.com/koltyakov/control/internal/model"
)

// registerAs registers a specific identity, as an installed child does.
func registerAs(t *testing.T, serverURL, token string, id *identity.Identity, n model.Node) *websocket.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(ctx, serverURL+"/v1/connect", &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer " + token}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ws.CloseNow() })
	_, challenge, err := ws.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	n.ID, n.PublicKey = id.ID, id.Public
	hello := Hello{Version: model.Version, NodeBytes: model.JSON(n)}
	hello.Signature = ed25519.Sign(id.Private, hello.Message(challenge))
	if err = ws.Write(ctx, websocket.MessageBinary, model.JSON(hello)); err != nil {
		t.Fatal(err)
	}
	if _, ready, err := ws.Read(ctx); err != nil || string(ready) != "ready" {
		t.Fatalf("registration rejected: %v", err)
	}
	return ws
}

func proxyListeners(t *testing.T, serverURL string, id *identity.Identity) []string {
	t.Helper()
	proof := model.ProxyStateProof{PublicKey: id.Public, SignedAt: time.Now().UTC()}
	proof.Signature = ed25519.Sign(id.Private, proof.Message())
	var state model.ProxyState
	if err := proxyRequest(t, serverURL+"/v1/node/proxy", "", proof, &state); err != nil {
		t.Fatal(err)
	}
	return state.Listeners
}

func readState(t *testing.T, serverURL string, id *identity.Identity, revision uint64, inspect bool) model.MachineState {
	t.Helper()
	proof := model.MachineStateProof{PublicKey: id.Public, SignedAt: time.Now().UTC(), Revision: revision, InspectOnly: inspect}
	proof.Signature = ed25519.Sign(id.Private, proof.Message())
	var state model.MachineState
	if err := proxyRequest(t, serverURL+"/v1/node/state", "", proof, &state); err != nil {
		t.Fatal(err)
	}
	return state
}

func redeemProxied(t *testing.T, serverURL string, link enrollment.Link) (*identity.Identity, string) {
	t.Helper()
	id, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	credential := "credential-" + identity.NewID()
	q := enrollment.Redemption{PublicKey: id.Public, CredentialHash: enrollment.Hash(credential), GatewayProxy: link.GatewayProxy.URL}
	ticket := link.URL[strings.LastIndex(link.URL, "/")+1:]
	q.Signature = ed25519.Sign(id.Private, enrollment.Message(ticket, q))
	if err = proxyRequest(t, serverURL+"/install/"+ticket+"/redeem", "", q, nil); err != nil {
		t.Fatal(err)
	}
	return id, credential
}

func waitProxyState(t *testing.T, admin testAdmin, worker, want string) model.ProxyEndpointStatus {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		states := proxyStatus(t, admin, worker)
		if len(states) == 1 && states[0].State == want {
			return states[0]
		}
		if time.Now().After(deadline) {
			t.Fatalf("proxy state %+v, want %s", states, want)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestManagedProxyDependenciesSurviveAndRequireRetirementAcknowledgement(t *testing.T) {
	g, server := installationFixture(t)
	alice, a := createTestUser(t, server, "alice")
	bob, b := createTestUser(t, server, "bob")
	office := "http://10.0.0.5:3128"
	// Managed endpoints must name an explicit private address.
	if _, _, ok := proxyWorkerPeer(t, server.URL, a.Key, "public-bridge", alice.ID, []model.ProxyEndpoint{{URL: "http://203.0.113.5:3128", Managed: true}}); ok {
		t.Fatal("public managed listener accepted")
	}
	ws, worker, ok := proxyWorkerPeer(t, server.URL, a.Key, "office-bridge", alice.ID, []model.ProxyEndpoint{{URL: office, Managed: true}})
	if !ok {
		t.Fatal("managed proxy worker rejected")
	}
	_, bobWorker, ok := proxyWorkerPeer(t, server.URL, b.Key, "bob-bridge", bob.ID, []model.ProxyEndpoint{{URL: office, Managed: true}})
	if !ok {
		t.Fatal("second fleet worker rejected")
	}
	if got := proxyListeners(t, server.URL, worker); len(got) != 1 {
		t.Fatalf("configured listener not enabled without dependencies: %v", got)
	}
	// Bootstrap: a standby listener is selectable before any child exists.
	sendProxyHealth(t, ws, []model.ProxyReadiness{{URL: office, State: model.ProxyStandby, CheckedAt: time.Now().UTC()}})
	if status := waitProxyState(t, a, worker.ID, model.ProxyStandby); !status.Managed || status.Dependents == nil || status.Dependents.Total() != 0 {
		t.Fatalf("standby status %+v", status)
	}
	create := func(name string) enrollment.Link {
		t.Helper()
		var link enrollment.Link
		q := enrollment.Request{Name: name, OS: "linux", Arch: "amd64", Gateway: "https://gateway.example", ProxyWorker: worker.ID}
		if err := proxyRequest(t, server.URL+"/v1/fleet/installations", a.Key, q, &link); err != nil {
			t.Fatal(err)
		}
		if link.GatewayProxy == nil || !link.GatewayProxy.Managed || link.ProxyActivation != enrollment.ProxyActivationPending {
			t.Fatalf("managed selection not confirmed as pending: %+v %q", link.GatewayProxy, link.ProxyActivation)
		}
		return link
	}
	want := []string{office}
	pending := create("render-01")
	if got := proxyListeners(t, server.URL, worker); !slices.Equal(got, want) {
		t.Fatalf("pending invitation did not activate listener: %v", got)
	}
	if got := proxyListeners(t, server.URL, bobWorker); len(got) != 1 {
		t.Fatalf("another fleet's configured listener changed: %v", got)
	}
	// Revoking an unused invitation releases its dependency.
	if err := a.JSON(context.Background(), "DELETE", "/v1/fleet/installations/"+pending.ID, nil, nil); err != nil {
		t.Fatal(err)
	}
	if got := proxyListeners(t, server.URL, worker); len(got) != 1 {
		t.Fatalf("revoked invitation stopped enabled listener: %v", got)
	}
	// Expiry releases it without any mutation.
	expiring := create("render-02")
	g.mu.Lock()
	for hash, i := range g.installations {
		if i.ID == expiring.ID {
			i.ExpiresAt = time.Now().Add(-time.Second)
			g.installations[hash] = i
		}
	}
	g.mu.Unlock()
	if got := proxyListeners(t, server.URL, worker); len(got) != 1 {
		t.Fatalf("expired invitation stopped enabled listener: %v", got)
	}
	// A redeemed child is a durable dependency independent of invitation
	// history, connection presence, and gateway restarts.
	child, credential := redeemProxied(t, server.URL, create("render-03"))
	second, _ := redeemProxied(t, server.URL, create("render-04"))
	if err := a.JSON(context.Background(), "DELETE", "/v1/fleet/installations", nil, nil); err != nil {
		t.Fatal(err)
	}
	childWS := registerAs(t, server.URL, credential, child, model.Node{Name: "render-03", UserID: alice.ID, OS: "linux", Software: buildinfo.Info{Arch: "amd64"}})
	_ = childWS.CloseNow()
	server.Close()
	if err := g.Close(); err != nil {
		t.Fatal(err)
	}
	g, err := New(filepath.Dir(g.path), commonKey, Options{SuperuserKey: superKey})
	if err != nil {
		t.Fatal(err)
	}
	restarted := httptest.NewServer(g.Handler())
	t.Cleanup(func() { restarted.Close(); _ = g.Close() })
	a.URL, b.URL = restarted.URL, restarted.URL
	if got := proxyListeners(t, restarted.URL, worker); !slices.Equal(got, want) {
		t.Fatalf("children lost dependency after clear and restart: %v", got)
	}
	g.mu.Lock()
	counts := g.managedDependentsLocked(alice.ID, time.Now())[worker.ID][office]
	g.mu.Unlock()
	if counts == nil || counts.Children != 2 || counts.Machines != 2 || counts.Invitations != 0 {
		t.Fatalf("dependency counts %+v", counts)
	}
	// Enrolled children cannot be released, and another fleet cannot release.
	if err = a.JSON(context.Background(), "DELETE", "/v1/fleet/proxy-dependencies/"+second.ID, nil, nil); err == nil {
		t.Fatal("enrolled child dependency released")
	}
	// Unregistering an offline child keeps the listener until the child
	// durably acknowledges the exact retirement revision.
	if err = a.JSON(context.Background(), "DELETE", "/v1/fleet/nodes/"+child.ID, nil, nil); err != nil {
		t.Fatal(err)
	}
	if got := proxyListeners(t, restarted.URL, worker); !slices.Equal(got, want) {
		t.Fatalf("unregistration cut off the retiring child: %v", got)
	}
	retired := readState(t, restarted.URL, child, 0, false)
	if !retired.Unregistered {
		t.Fatal("child could not read retirement after revocation")
	}
	readState(t, restarted.URL, child, retired.Revision, true)
	if got := proxyListeners(t, restarted.URL, worker); !slices.Equal(got, want) {
		t.Fatalf("delivery or inspection counted as acknowledgement: %v", got)
	}
	// Another identity's acknowledgement cannot release this dependency.
	readState(t, restarted.URL, bobWorker, retired.Revision, false)
	readState(t, restarted.URL, child, retired.Revision, false)
	g.mu.Lock()
	_, kept := g.proxyDependencies[child.ID]
	g.mu.Unlock()
	if kept {
		t.Fatal("retirement acknowledgement did not release the dependency")
	}
	if got := proxyListeners(t, restarted.URL, worker); !slices.Equal(got, want) {
		t.Fatalf("shared listener stopped while another child depends on it: %v", got)
	}
	// The second child never returns: only the owner can release its wait.
	g.mu.Lock()
	previous := g.machineStates[second.ID]
	g.machineStates[second.ID] = model.MachineState{Revision: previous.Revision + 1, Disabled: true, Unregistered: true}
	g.mu.Unlock()
	if err = b.JSON(context.Background(), "DELETE", "/v1/fleet/proxy-dependencies/"+second.ID, nil, nil); err == nil {
		t.Fatal("another fleet released a dependency")
	}
	if err = a.JSON(context.Background(), "DELETE", "/v1/fleet/proxy-dependencies/"+second.ID, nil, nil); err != nil {
		t.Fatal(err)
	}
	if got := proxyListeners(t, restarted.URL, worker); len(got) != 1 {
		t.Fatalf("enabled listener stopped after final dependency: %v", got)
	}
}

func TestManagedProxyRedemptionThroughAnotherRouteReplacesDependency(t *testing.T) {
	g, server := installationFixture(t)
	alice, a := createTestUser(t, server, "alice")
	office := "http://10.0.0.5:3128"
	ws, worker, ok := proxyWorkerPeer(t, server.URL, a.Key, "office-bridge", alice.ID, []model.ProxyEndpoint{{URL: office, Managed: true}})
	if !ok {
		t.Fatal("worker rejected")
	}
	sendProxyHealth(t, ws, []model.ProxyReadiness{{URL: office, State: model.ProxyStandby, CheckedAt: time.Now().UTC()}})
	waitProxyState(t, a, worker.ID, model.ProxyStandby)
	var link enrollment.Link
	if err := proxyRequest(t, server.URL+"/v1/fleet/installations", a.Key, enrollment.Request{Name: "render-01", OS: "linux", Arch: "amd64", Gateway: "https://gateway.example", ProxyWorker: worker.ID}, &link); err != nil {
		t.Fatal(err)
	}
	child, _ := redeemProxied(t, server.URL, link)
	if got := proxyListeners(t, server.URL, worker); len(got) != 1 {
		t.Fatalf("listeners %v", got)
	}
	// The same identity re-enrolls directly: its signed redemption replaces
	// the route, so the managed dependency is released in that transaction.
	if err := proxyRequest(t, server.URL+"/v1/fleet/installations", a.Key, enrollment.Request{Name: "render-01", OS: "linux", Arch: "amd64", Gateway: "https://gateway.example"}, &link); err != nil {
		t.Fatal(err)
	}
	q := enrollment.Redemption{PublicKey: child.Public, CredentialHash: enrollment.Hash("direct-credential")}
	ticket := link.URL[strings.LastIndex(link.URL, "/")+1:]
	q.Signature = ed25519.Sign(child.Private, enrollment.Message(ticket, q))
	if err := proxyRequest(t, server.URL+"/install/"+ticket+"/redeem", "", q, nil); err != nil {
		t.Fatal(err)
	}
	if got := proxyListeners(t, server.URL, worker); len(got) != 1 {
		t.Fatalf("direct re-enrollment stopped enabled listener: %v", got)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.proxyDependencies) != 0 {
		t.Fatal("dependency record retained")
	}
}

func TestExternalProxyEndpointsNeverReportStandby(t *testing.T) {
	_, server := installationFixture(t)
	alice, a := createTestUser(t, server, "alice")
	office := "http://10.0.0.5:3128"
	ws, worker, ok := proxyWorkerPeer(t, server.URL, a.Key, "office-bridge", alice.ID, []model.ProxyEndpoint{{URL: office}})
	if !ok {
		t.Fatal("worker rejected")
	}
	sendProxyHealth(t, ws, []model.ProxyReadiness{{URL: office, State: model.ProxyStandby, CheckedAt: time.Now().UTC()}})
	time.Sleep(100 * time.Millisecond)
	if states := proxyStatus(t, a, worker.ID); len(states) != 1 || states[0].State != model.ProxyUnchecked || states[0].Managed || states[0].Dependents != nil {
		t.Fatalf("external endpoint accepted standby: %+v", states)
	}
	var link enrollment.Link
	if err := proxyRequest(t, server.URL+"/v1/fleet/installations", a.Key, enrollment.Request{Name: "render-01", OS: "linux", Arch: "amd64", Gateway: "https://gateway.example", ProxyWorker: worker.ID}, &link); err == nil {
		t.Fatal("external endpoint selected without a ready check")
	}
}

func TestManagedProxyCountsPendingHybridCompanion(t *testing.T) {
	g, server := installationFixture(t)
	alice, a := createTestUser(t, server, "alice")
	office := "http://10.0.0.5:3128"
	ws, worker, ok := proxyWorkerPeer(t, server.URL, a.Key, "office-bridge", alice.ID, []model.ProxyEndpoint{{URL: office, Managed: true}})
	if !ok {
		t.Fatal("worker rejected")
	}
	sendProxyHealth(t, ws, []model.ProxyReadiness{{URL: office, State: model.ProxyStandby, CheckedAt: time.Now().UTC()}})
	waitProxyState(t, a, worker.ID, model.ProxyStandby)
	var link enrollment.Link
	if err := proxyRequest(t, server.URL+"/v1/fleet/installations", a.Key, enrollment.Request{Name: "render-01", OS: "linux", Arch: "amd64", ServiceMode: "hybrid", Gateway: "https://gateway.example", ProxyWorker: worker.ID}, &link); err != nil {
		t.Fatal(err)
	}
	// Redeeming the User context leaves the System companion pending.
	user, userKey := redeemProxied(t, server.URL, link)
	g.mu.Lock()
	counts := g.managedDependentsLocked(alice.ID, time.Now())[worker.ID][office]
	g.mu.Unlock()
	if counts == nil || counts.Invitations != 1 || counts.Children != 1 || counts.Machines != 1 {
		t.Fatalf("hybrid dependency counts %+v", counts)
	}
	if status := waitProxyState(t, a, worker.ID, model.ProxyStandby); status.Dependents == nil || status.Dependents.Total() != 2 {
		t.Fatalf("owner projection %+v", status.Dependents)
	}
	ticket := enrollment.SystemTicket(link.URL[strings.LastIndex(link.URL, "/")+1:])
	g.mu.Lock()
	companion := g.installations[enrollment.Hash(ticket)]
	g.mu.Unlock()
	system, systemKey := redeemProxied(t, server.URL, enrollment.Link{Invitation: companion.Invitation, URL: server.URL + "/install/" + ticket})
	registerAs(t, server.URL, userKey, user, model.Node{Name: link.Name, OS: "linux", RunAs: "user", Software: buildinfo.Info{Arch: "amd64"}})
	registerAs(t, server.URL, systemKey, system, model.Node{Name: companion.Name, OS: "linux", RunAs: "system", InstructionOnly: true, Software: buildinfo.Info{Arch: "amd64"}})

	check := func(children, machines, retiring int) {
		t.Helper()
		g.mu.Lock()
		locked := g.managedDependentsLocked(alice.ID, time.Now())[worker.ID][office]
		management := g.proxyCountsLocked(alice.ID, worker.ID)
		g.mu.Unlock()
		projected := proxyStatus(t, a, worker.ID)[0].Dependents
		for _, got := range []*model.ProxyDependents{locked, &management, projected} {
			if got == nil || got.Children != children || got.Machines != machines || got.Retiring != retiring || got.Invitations != 0 || got.Total() != children+retiring {
				t.Fatalf("dependency counts %+v, want children=%d machines=%d retiring=%d", got, children, machines, retiring)
			}
		}
	}
	check(2, 1, 0)
	if err := a.JSON(t.Context(), "DELETE", "/v1/fleet/installations", nil, nil); err != nil {
		t.Fatal(err)
	}
	check(2, 1, 0)
	server.Close()
	if err := g.Close(); err != nil {
		t.Fatal(err)
	}
	var err error
	g, err = New(filepath.Dir(g.path), commonKey, Options{SuperuserKey: superKey})
	if err != nil {
		t.Fatal(err)
	}
	restarted := httptest.NewServer(g.Handler())
	t.Cleanup(func() { restarted.Close(); _ = g.Close() })
	a.URL = restarted.URL
	check(2, 1, 0)
	if err := a.JSON(t.Context(), "DELETE", "/v1/fleet/nodes/"+system.ID, nil, nil); err != nil {
		t.Fatal(err)
	}
	check(1, 1, 1)
	if err := a.JSON(t.Context(), "DELETE", "/v1/fleet/nodes/"+user.ID, nil, nil); err != nil {
		t.Fatal(err)
	}
	check(0, 0, 2)
}

func TestManagedProxyStatusDoesNotWaitForPersistence(t *testing.T) {
	g, server := installationFixture(t)
	alice, a := createTestUser(t, server, "alice")
	const endpoint = "http://10.0.0.5:3128"
	ws, worker, ok := proxyWorkerPeer(t, server.URL, a.Key, "bridge", alice.ID, []model.ProxyEndpoint{{URL: endpoint, Managed: true}})
	if !ok {
		t.Fatal("worker rejected")
	}
	sendProxyHealth(t, ws, []model.ProxyReadiness{{URL: endpoint, State: model.ProxyStandby, CheckedAt: time.Now().UTC()}})
	waitProxyState(t, a, worker.ID, model.ProxyStandby)
	var link enrollment.Link
	if err := proxyRequest(t, server.URL+"/v1/fleet/installations", a.Key, enrollment.Request{Name: "child", OS: "linux", Arch: "amd64", Gateway: "https://gateway.example", ProxyWorker: worker.ID}, &link); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	var status model.PoolActivitySnapshot
	done := make(chan error, 1)
	g.mu.Lock()
	go func() { done <- a.JSON(ctx, http.MethodGet, "/v1/status", nil, &status) }()
	err := <-done
	g.mu.Unlock()
	if err != nil {
		t.Fatalf("managed status waited behind persistence: %v", err)
	}
	found := false
	for _, n := range status.Nodes {
		if n.ID == worker.ID && len(n.ProxyEndpoints) == 1 {
			d := n.ProxyEndpoints[0].Dependents
			found = d != nil && d.Invitations == 1
		}
	}
	if !found {
		t.Fatal("status omitted the committed invitation dependency")
	}
	expired := g.proxyDependentsFor(alice.ID, g.observations(alice.ID), link.ExpiresAt.Add(time.Second))
	if d := expired[worker.ID][endpoint]; d != nil && d.Invitations != 0 {
		t.Fatal("expired invitation remained in the read-only projection")
	}
}

func TestManagedProxyFirstDependencyUsesEnabledListenerReadiness(t *testing.T) {
	_, server := installationFixture(t)
	alice, a := createTestUser(t, server, "alice")
	const endpoint = "http://10.0.0.5:3128"
	ws, worker, ok := proxyWorkerPeer(t, server.URL, a.Key, "bridge", alice.ID, []model.ProxyEndpoint{{URL: endpoint, Managed: true}})
	if !ok {
		t.Fatal("worker rejected")
	}
	// The listener remains explicitly enabled before the first dependency.
	sendProxyHealth(t, ws, []model.ProxyReadiness{{URL: endpoint, State: model.ProxyReady, CheckedAt: time.Now().UTC()}})
	waitProxyState(t, a, worker.ID, model.ProxyReady)
	var link enrollment.Link
	if err := proxyRequest(t, server.URL+"/v1/fleet/installations", a.Key, enrollment.Request{Name: "child", OS: "linux", Arch: "amd64", Gateway: "https://gateway.example", ProxyWorker: worker.ID}, &link); err != nil {
		t.Fatal(err)
	}
	if link.ProxyActivation != enrollment.ProxyActivationActive {
		t.Fatal("ready enabled listener was incorrectly reported pending")
	}
}
