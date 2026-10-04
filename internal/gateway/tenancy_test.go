package gateway

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/koltyakov/control/internal/enrollment"
	"github.com/koltyakov/control/internal/identity"
	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/protocol"
	"github.com/koltyakov/control/internal/store"
	"google.golang.org/protobuf/proto"
)

func createTestUser(t *testing.T, server *httptest.Server, name string) (User, testAdmin) {
	t.Helper()
	var issued struct {
		User  User
		Token string
	}
	if err := (testAdmin{URL: server.URL, Key: superKey}).JSON(context.Background(), "POST", "/v1/admin/users", map[string]string{"name": name}, &issued); err != nil {
		t.Fatal(err)
	}
	return issued.User, testAdmin{URL: server.URL, Key: issued.Token}
}

func TestFleetAccountsAndInvitationsAreIsolated(t *testing.T) {
	g, s := installationFixture(t)
	_, a := createTestUser(t, s, "alice")
	bob, b := createTestUser(t, s, "bob")
	ctx := context.Background()
	var ka, kb struct {
		Key   APIKey
		Token string
	}
	for c, out := range map[testAdmin]*struct {
		Key   APIKey
		Token string
	}{a: &ka, b: &kb} {
		if err := c.JSON(ctx, "POST", "/v1/fleet/keys", map[string]string{"name": "host"}, out); err != nil {
			t.Fatal(err)
		}
	}
	if ka.Key.UserID == kb.Key.UserID {
		t.Fatal("accounts share a fleet")
	}
	var keys []APIKey
	if err := a.JSON(ctx, "GET", "/v1/fleet/keys", nil, &keys); err != nil {
		t.Fatal(err)
	}
	for _, k := range keys {
		if k.UserID != ka.Key.UserID {
			t.Fatal("foreign credential disclosed")
		}
	}
	if err := a.JSON(ctx, "DELETE", "/v1/fleet/keys/"+kb.Key.ID, nil, nil); err == nil {
		t.Fatal("foreign key revoked")
	}
	var ia, ib enrollment.Link
	q := enrollment.Request{Name: "worker", OS: "linux", Arch: "amd64", Gateway: s.URL}
	if err := a.JSON(ctx, "POST", "/v1/fleet/installations", q, &ia); err != nil {
		t.Fatal(err)
	}
	if err := b.JSON(ctx, "POST", "/v1/fleet/installations", q, &ib); err != nil {
		t.Fatal("name must be reusable in another fleet", err)
	}
	var list []enrollment.Invitation
	if err := a.JSON(ctx, "GET", "/v1/fleet/installations", nil, &list); err != nil || len(list) != 1 || list[0].ID != ia.ID {
		t.Fatalf("foreign invitation disclosed: %+v %v", list, err)
	}
	if err := a.JSON(ctx, "DELETE", "/v1/fleet/installations/"+ib.ID, nil, nil); err == nil {
		t.Fatal("foreign invitation revoked")
	}
	for _, route := range []string{"/v1/admin/users", "/v1/admin/updates", "/v1/admin/keys"} {
		if err := a.JSON(ctx, "GET", route, nil, nil); err == nil {
			t.Fatal("user obtained gateway administration", route)
		}
	}
	if err := (testAdmin{URL: s.URL, Key: ka.Token}).JSON(ctx, "POST", "/v1/fleet/installations", q, nil); err == nil {
		t.Fatal("machine key gained fleet administration")
	}
	if err := (testAdmin{URL: s.URL, Key: superKey}).JSON(ctx, "DELETE", "/v1/admin/users/"+bob.ID, nil, nil); err != nil {
		t.Fatal(err)
	}
	if p := g.authenticate(b.Key); p.Role != "" {
		t.Fatal("disabled account authenticates")
	}
	if p := g.authenticate(kb.Token); p.Role != "" {
		t.Fatal("disabled account's machine key authenticates")
	}
	resp, err := http.Get(ib.URL)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusGone {
		t.Fatal("disabled account's invitation remains active")
	}
	if err := a.JSON(ctx, "GET", "/v1/fleet/keys", nil, nil); err != nil {
		t.Fatal("revoking Bob affected Alice", err)
	}
}

func rawPeer(t *testing.T, serverURL, token, name, userID string, id *identity.Identity) *websocket.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(ctx, serverURL+"/v1/connect", &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer " + token}}})
	if err != nil {
		t.Fatal(err)
	}
	_, challenge, err := ws.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	n := model.Node{ID: id.ID, PublicKey: id.Public, Name: name, UserID: userID}
	hello := Hello{Version: model.Version, Node: n, Signature: ed25519.Sign(id.Private, append(challenge, model.JSON(n)...))}
	if err = ws.Write(ctx, websocket.MessageBinary, model.JSON(hello)); err != nil {
		t.Fatal(err)
	}
	_, ready, err := ws.Read(ctx)
	if err != nil || string(ready) != "ready" {
		_ = ws.CloseNow()
		return nil
	}
	t.Cleanup(func() { _ = ws.CloseNow() })
	return ws
}

func TestGatewayBlocksForgedCrossFleetRoutingAndIdentityTransfer(t *testing.T) {
	g, s := installationFixture(t)
	alice, a := createTestUser(t, s, "alice")
	bob, b := createTestUser(t, s, "bob")
	ids := make([]*identity.Identity, 3)
	for i := range ids {
		var err error
		ids[i], err = identity.Load(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
	}
	wa := rawPeer(t, s.URL, a.Key, "host", alice.ID, ids[0])
	wb := rawPeer(t, s.URL, b.Key, "host", bob.ID, ids[1])
	wc := rawPeer(t, s.URL, b.Key, "worker", bob.ID, ids[2])
	if wa == nil || wb == nil || wc == nil {
		t.Fatal("fleet peer registration failed")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var nodes []model.Node
	if err := a.JSON(ctx, "GET", "/v1/nodes", nil, &nodes); err != nil || len(nodes) != 1 || nodes[0].ID != ids[0].ID {
		t.Fatalf("directory isolation: %+v %v", nodes, err)
	}
	if err := a.JSON(ctx, "DELETE", "/v1/fleet/nodes/"+ids[1].ID, nil, nil); err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatal("foreign machine existence disclosed", err)
	}
	send := func(ws *websocket.Conn, p *protocol.Packet) {
		t.Helper()
		data, err := proto.Marshal(p)
		if err != nil {
			t.Fatal(err)
		}
		if err = ws.Write(ctx, websocket.MessageBinary, data); err != nil {
			t.Fatal(err)
		}
	}
	read := func(ws *websocket.Conn) *protocol.Packet {
		t.Helper()
		_, data, err := ws.Read(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var p protocol.Packet
		if err = proto.Unmarshal(data, &p); err != nil {
			t.Fatal(err)
		}
		return &p
	}
	// Bypass directory resolution. An attacker knows Bob's IDs and sends raw
	// signaling/relay packets with a forged same-fleet From field.
	for _, kind := range []string{"open", "answer", "data", "close"} {
		send(wa, &protocol.Packet{Kind: kind, From: ids[2].ID, To: ids[1].ID, Session: "attack", Data: []byte("private")})
		if kind == "open" {
			if p := read(wa); p.Kind != "close" {
				t.Fatal("cross-fleet open not rejected")
			}
		}
	}
	send(wa, &protocol.Packet{Kind: "open", To: "missing", Session: "barrier"})
	if p := read(wa); p.Session != "barrier" {
		t.Fatal("routing barrier failed")
	}
	// A same-fleet message is the read barrier. No attacker packet may precede it.
	send(wc, &protocol.Packet{Kind: "open", To: ids[1].ID, Session: "allowed"})
	if p := read(wb); p.Session != "allowed" || p.From != ids[2].ID {
		t.Fatalf("cross-fleet packet escaped filtering: %+v", p)
	}
	_ = wa.CloseNow()
	for {
		g.mu.Lock()
		offline := g.peers[ids[0].ID] == nil
		g.mu.Unlock()
		if offline {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
	if err := a.JSON(ctx, "DELETE", "/v1/fleet/nodes/"+ids[0].ID, nil, nil); err != nil {
		t.Fatal(err)
	}
	send(wc, &protocol.Packet{Kind: "open", To: ids[1].ID, Session: "after-offline"})
	if p := read(wb); p.Session != "after-offline" {
		t.Fatal("foreign offline event disclosed", p.Kind)
	}
	if ws := rawPeer(t, s.URL, b.Key, "stolen", bob.ID, ids[0]); ws != nil {
		t.Fatal("forgotten identity transferred to another fleet")
	}
	if ws := rawPeer(t, s.URL, a.Key, "spoofed", bob.ID, ids[0]); ws != nil {
		t.Fatal("client-supplied userId escaped credential scope")
	}
}

func TestSQLiteLegacyMigrationAndRestart(t *testing.T) {
	dir := t.TempDir()
	n := model.Node{ID: "existing-identity", Name: "worker"}
	k := keyRecord{APIKey: APIKey{ID: "old-key", Name: "old", Role: "common"}, Hash: tokenHash("old-machine-secret")}
	if err := store.Write(filepath.Join(dir, "nodes.json"), map[string]model.Node{n.ID: n}); err != nil {
		t.Fatal(err)
	}
	if err := store.Write(filepath.Join(dir, "keys.json"), map[string]keyRecord{k.ID: k}); err != nil {
		t.Fatal(err)
	}
	g, err := New(dir, commonKey, Options{SuperuserKey: superKey})
	if err != nil {
		t.Fatal(err)
	}
	s := httptest.NewServer(g.Handler())
	u, a := createTestUser(t, s, "alice")
	if g.nodes[n.ID].UserID != legacyUser || g.authenticate("old-machine-secret").UserID != legacyUser {
		t.Fatal("legacy state not scoped during migration")
	}
	s.Close()
	_ = g.Close()
	// Once committed, SQLite wins over stale legacy files on every restart.
	if err = os.WriteFile(filepath.Join(dir, "nodes.json"), []byte("invalid stale JSON"), 0600); err != nil {
		t.Fatal(err)
	}
	g, err = New(dir, commonKey, Options{SuperuserKey: superKey})
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	if g.authenticate(a.Key).UserID != u.ID || g.owners[n.ID] != legacyUser {
		t.Fatal("SQLite lost account or permanent identity ownership")
	}
	var count int
	if err = g.db.QueryRow(`SELECT count(*) FROM users`).Scan(&count); err != nil || count != 2 {
		t.Fatalf("user database: %d %v", count, err)
	}
	var serialized []byte
	if err = g.db.QueryRow(`SELECT data FROM credentials WHERE user_id=?`, u.ID).Scan(&serialized); err != nil {
		t.Fatal(err)
	}
	if !json.Valid(serialized) || strings.Contains(string(serialized), a.Key) {
		t.Fatal("credential stored as plaintext")
	}
}
