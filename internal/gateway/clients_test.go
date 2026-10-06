package gateway

import (
	"context"
	"crypto/ed25519"
	"database/sql"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/koltyakov/control/internal/buildinfo"
	"github.com/koltyakov/control/internal/identity"
	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/update"
)

func rawClientSession(t *testing.T, base, token string, id *identity.Identity, n model.Node, signedClient bool) *websocket.Conn {
	return rawOwnedClientSession(t, base, token, id, n, signedClient, nil, nil)
}

func rawOwnedClientSession(t *testing.T, base, token string, id *identity.Identity, n model.Node, signedClient bool, owner *identity.Identity, mutate func(*Hello)) *websocket.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(base, "http")+"/v1/client/connect", &websocket.DialOptions{HTTPHeader: map[string][]string{"Authorization": {"Bearer " + token}}})
	if err != nil {
		return nil
	}
	_, challenge, err := ws.Read(ctx)
	if err != nil {
		_ = ws.CloseNow()
		return nil
	}
	n.ID, n.PublicKey = id.ID, id.Public
	hello := Hello{Version: model.Version, Node: n, Client: signedClient}
	if owner != nil {
		hello.Node.ClientOwner = owner.ID
		hello.OwnerKey = owner.Public
		hello.OwnerSignature = ed25519.Sign(owner.Private, hello.OwnerMessage(challenge))
	}
	if mutate != nil {
		mutate(&hello)
	}
	hello.Signature = ed25519.Sign(id.Private, hello.Message(challenge))
	hello.Client = true
	if err := ws.Write(ctx, websocket.MessageBinary, model.JSON(hello)); err != nil {
		_ = ws.CloseNow()
		return nil
	}
	_, ready, err := ws.Read(ctx)
	if err != nil || string(ready) != "ready" {
		_ = ws.CloseNow()
		return nil
	}
	t.Cleanup(func() { _ = ws.CloseNow() })
	return ws
}

func TestConcurrentClientOwnerProofsAndRoles(t *testing.T) {
	g, s := installationFixture(t)
	alice, a := createTestUser(t, s, "alice")
	_, b := createTestUser(t, s, "bob")
	owner, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	id, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	n := model.Node{Name: "cli-" + owner.ID[:16]}
	for _, mutate := range []func(*Hello){
		func(h *Hello) { h.OwnerSignature = nil },
		func(h *Hello) { h.OwnerSignature[0] ^= 1 },
		func(h *Hello) { h.Node.ClientOwner = id.ID },
		func(h *Hello) { h.Node.OS = "forged-after-owner-proof" },
	} {
		if ws := rawOwnedClientSession(t, s.URL, a.Key, id, n, true, owner, mutate); ws != nil {
			t.Fatal("forged owner accepted")
		}
	}
	ws := rawOwnedClientSession(t, s.URL, a.Key, id, n, true, owner, nil)
	if ws == nil {
		t.Fatal("valid owner proof rejected")
	}
	second, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	if ws := rawOwnedClientSession(t, s.URL, a.Key, second, n, true, owner, nil); ws == nil {
		t.Fatal("concurrent owner rejected")
	}
	if ws := rawOwnedClientSession(t, s.URL, b.Key, second, n, true, owner, nil); ws != nil {
		t.Fatal("owner moved accounts")
	}
	_ = ws.CloseNow()
	waitSessionClosed(t, g, id.ID)
	if ws := rawPeer(t, s.URL, a.Key, "machine", alice.ID, id); ws != nil {
		t.Fatal("transport enrolled as machine")
	}
	if ws := rawPeer(t, s.URL, a.Key, "machine", alice.ID, owner); ws != nil {
		t.Fatal("task owner enrolled as machine")
	}
	otherOwner, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	if ws := rawOwnedClientSession(t, s.URL, a.Key, id, model.Node{Name: "cli-" + otherOwner.ID[:16]}, true, otherOwner, nil); ws != nil {
		t.Fatal("transport changed owners")
	}
	if ws := rawClientSession(t, s.URL, a.Key, id, model.Node{Name: "cli-" + id.ID[:16]}, true); ws != nil {
		t.Fatal("transport promoted to permanent owner")
	}
	var stored string
	if err := g.db.QueryRow(`SELECT owner_id FROM client_transports WHERE id=?`, id.ID).Scan(&stored); err != nil || stored != owner.ID {
		t.Fatal("binding not durable", stored, err)
	}
	if ws := rawOwnedClientSession(t, s.URL, a.Key, id, n, true, owner, nil); ws == nil {
		t.Fatal("same transport could not reconnect")
	}
	forgotten, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	machine := rawPeer(t, s.URL, a.Key, "forgotten-worker", alice.ID, forgotten)
	if machine == nil {
		t.Fatal("machine registration failed")
	}
	_ = machine.CloseNow()
	waitSessionClosed(t, g, forgotten.ID)
	if err := a.JSON(context.Background(), "DELETE", "/v1/fleet/nodes/"+forgotten.ID, nil, nil); err != nil {
		t.Fatal(err)
	}
	if ws := rawClientSession(t, s.URL, a.Key, forgotten, model.Node{Name: "cli-" + forgotten.ID[:16]}, true); ws != nil {
		t.Fatal("forgotten machine promoted to client owner")
	}
	if ws := rawOwnedClientSession(t, s.URL, a.Key, second, model.Node{Name: "cli-" + forgotten.ID[:16]}, true, forgotten, nil); ws != nil {
		t.Fatal("forgotten machine delegated client ownership")
	}
	dir := filepath.Dir(g.path)
	s.Close()
	if err := g.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := New(dir, commonKey, Options{SuperuserKey: superKey})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(restarted.Handler())
	t.Cleanup(func() { server.Close(); _ = restarted.Close() })
	if restarted.clientTransports[id.ID] != owner.ID || !restarted.clientOwners[owner.ID] || len(restarted.clients) != 0 {
		t.Fatal("owner binding lost on gateway restart")
	}
	if ws := rawPeer(t, server.URL, a.Key, "machine", alice.ID, id); ws != nil {
		t.Fatal("restarted gateway lost transport role")
	}
	if ws := rawOwnedClientSession(t, server.URL, a.Key, id, model.Node{Name: "cli-" + otherOwner.ID[:16]}, true, otherOwner, nil); ws != nil {
		t.Fatal("restarted gateway changed owner")
	}
	if ws := rawOwnedClientSession(t, server.URL, a.Key, id, n, true, owner, nil); ws == nil {
		t.Fatal("restarted gateway rejected valid owner")
	}
}

func waitSessionClosed(t *testing.T, g *Gateway, id string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for {
		g.mu.Lock()
		closed := g.peers[id] == nil
		g.mu.Unlock()
		if closed {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func TestClientSessionsAreNotFleetMachines(t *testing.T) {
	g, s := installationFixture(t)
	alice, a := createTestUser(t, s, "alice")
	_, b := createTestUser(t, s, "bob")
	id, err := identity.Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	n := model.Node{Name: "cli-" + id.ID[:16], OS: "darwin", Software: buildinfo.Info{OS: "darwin", Arch: "arm64"}}
	if ws := rawClientSession(t, s.URL, a.Key, id, n, false); ws != nil {
		t.Fatal("machine proof replayed as a client proof")
	}
	var issued struct {
		Token string
		Key   APIKey
	}
	ctx := context.Background()
	if err := a.JSON(ctx, "POST", "/v1/fleet/keys", map[string]string{"name": "cli"}, &issued); err != nil {
		t.Fatal(err)
	}
	ws := rawClientSession(t, s.URL, issued.Token, id, n, true)
	if ws == nil {
		t.Fatal("client session rejected")
	}
	var nodes []model.Node
	if err := a.JSON(ctx, "GET", "/v1/nodes", nil, &nodes); err != nil || len(nodes) != 0 {
		t.Fatal("client enrolled as a machine", nodes, err)
	}
	var pool model.PoolActivitySnapshot
	if err := a.JSON(ctx, "GET", "/v1/status", nil, &pool); err != nil || len(pool.Nodes) != 0 {
		t.Fatal("client appeared in dashboard", pool, err)
	}
	var peer model.Node
	if err := a.JSON(ctx, "GET", "/v1/peers/"+id.ID, nil, &peer); err != nil || peer.UserID != alice.ID || peer.ID != id.ID {
		t.Fatal("client identity not scoped", peer, err)
	}
	if err := b.JSON(ctx, "GET", "/v1/peers/"+id.ID, nil, nil); err == nil {
		t.Fatal("foreign client identity disclosed")
	}
	for _, method := range []string{"PATCH", "DELETE"} {
		if err := a.JSON(ctx, method, "/v1/fleet/nodes/"+id.ID, map[string]any{"disabled": true}, nil); err == nil {
			t.Fatal("machine management accepted a client identity", method)
		}
	}
	g.mu.Lock()
	correct := len(g.nodes) == 0 && len(g.clients) == 1 && g.owners[id.ID] == alice.ID && g.clientOwners[id.ID]
	g.mu.Unlock()
	if !correct {
		t.Fatal("client/session ownership state mixed with machines")
	}
	var count int
	if err := g.db.QueryRow(`SELECT count(*) FROM nodes`).Scan(&count); err != nil || count != 0 {
		t.Fatal("client metadata persisted in machine table", count, err)
	}
	if err := g.db.QueryRow(`SELECT count(*) FROM client_identities`).Scan(&count); err != nil || count != 1 {
		t.Fatal("client role not durable", count, err)
	}
	if peers := g.rolloutPeers(&update.Deployment{}); len(peers) != 0 {
		t.Fatal("client included in managed rollout", peers)
	}
	asset := update.Asset{OS: "linux", Arch: "amd64", File: "control_linux_amd64", Size: 1, SHA256: strings.Repeat("a", 64)}
	manifest := update.Manifest{Version: "test", Assets: []update.Asset{asset}}
	g.options.Software = buildinfo.Info{OS: "linux", Arch: "amd64"}
	if err := g.validatePlatforms(manifest); err != nil {
		t.Fatal("client platform required by update bundle", err)
	}
	g.control(id.ID, "update.offer", update.Command{})
	g.mu.Lock()
	queued := len(g.peers[id.ID].out)
	g.mu.Unlock()
	if queued != 0 {
		t.Fatal("client received an update command")
	}
	if !g.reserveRestart(&update.Deployment{}, nil) {
		t.Fatal("idle client connection blocked gateway restart")
	}
	g.mu.Lock()
	g.restarting = false
	g.mu.Unlock()
	if err := a.JSON(ctx, "DELETE", "/v1/fleet/keys/"+issued.Key.ID, nil, nil); err != nil {
		t.Fatal(err)
	}
	waitSessionClosed(t, g, id.ID)
	_ = ws.CloseNow()
	if err := a.JSON(ctx, "GET", "/v1/peers/"+id.ID, nil, nil); err == nil {
		t.Fatal("client routing metadata survived disconnect")
	}
	if ws := rawPeer(t, s.URL, a.Key, "worker", alice.ID, id); ws != nil {
		t.Fatal("client identity reused for machine enrollment")
	}
	impersonator, err := identity.Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if ws := rawPeer(t, s.URL, a.Key, n.Name, alice.ID, impersonator); ws != nil {
		t.Fatal("machine impersonated a disconnected client's access-rule name")
	}
	if err := a.JSON(ctx, "POST", "/v1/fleet/installations", map[string]any{"name": n.Name, "os": "linux", "arch": "amd64", "gateway": s.URL}, nil); err == nil {
		t.Fatal("installation invitation claimed a reserved client name")
	}
	if ws := rawClientSession(t, s.URL, b.Key, id, n, true); ws != nil {
		t.Fatal("client identity moved to another account")
	}
	bad := n
	bad.Capabilities = []model.Capability{{Name: "exec.run", InputSchema: model.JSON(map[string]any{})}}
	if ws := rawClientSession(t, s.URL, a.Key, id, bad, true); ws != nil {
		t.Fatal("client advertised execution capabilities")
	}
}

func TestLegacyCLIRegistrationMigratesWithoutLosingOwnership(t *testing.T) {
	dir := t.TempDir()
	g, err := New(dir, commonKey, Options{SuperuserKey: superKey})
	if err != nil {
		t.Fatal(err)
	}
	s := httptest.NewServer(g.Handler())
	id, err := identity.Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	name := "cli-" + id.ID[:16]
	ws := rawPeer(t, s.URL, commonKey, name, legacyUser, id)
	if ws == nil {
		t.Fatal("legacy registration failed")
	}
	_ = ws.CloseNow()
	waitSessionClosed(t, g, id.ID)
	// An ordinary capability-bearing machine must not be migrated by its name.
	other, err := identity.Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	g.mu.Lock()
	g.owners[other.ID] = legacyUser
	g.nodes[other.ID] = model.Node{ID: other.ID, PublicKey: other.Public, UserID: legacyUser, Name: "cli-" + other.ID[:16], Capabilities: []model.Capability{{Name: "exec.run"}}}
	err = g.persist()
	g.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	_ = g.Close()
	// Exercise the deployed v2 database upgrade as well as registration cleanup.
	db, err := sql.Open("sqlite", dir+"/gateway.db")
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`DROP TABLE client_transports; DROP TABLE client_identities; PRAGMA user_version=2;`)
	_ = db.Close()
	if err != nil {
		t.Fatal(err)
	}
	g, err = New(dir, commonKey, Options{SuperuserKey: superKey})
	if err != nil {
		t.Fatal(err)
	}
	s = httptest.NewServer(g.Handler())
	t.Cleanup(func() { s.Close(); _ = g.Close() })
	if _, exists := g.nodes[id.ID]; exists || !g.clientOwners[id.ID] || g.owners[id.ID] != legacyUser {
		t.Fatal("CLI registration not migrated with stable ownership")
	}
	if _, exists := g.nodes[other.ID]; !exists {
		t.Fatal("migration removed an execution machine")
	}
	if ws := rawPeer(t, s.URL, commonKey, name, legacyUser, id); ws != nil {
		t.Fatal("old CLI could recreate the fleet entry")
	}
	ws = rawClientSession(t, s.URL, commonKey, id, model.Node{Name: name}, true)
	if ws == nil {
		t.Fatal("migrated client could not reconnect with its original identity")
	}
	_ = ws.CloseNow()
	waitSessionClosed(t, g, id.ID)
	s.Close()
	_ = g.Close()
	g, err = New(dir, commonKey, Options{SuperuserKey: superKey})
	if err != nil {
		t.Fatal(err)
	}
	s = httptest.NewServer(g.Handler())
	if g.owners[id.ID] != legacyUser || !g.clientOwners[id.ID] || len(g.clients) != 0 {
		t.Fatal("client ownership or transient session lifetime lost on restart")
	}
}
