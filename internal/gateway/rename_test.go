package gateway

import (
	"context"
	"crypto/ed25519"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/koltyakov/control/internal/enrollment"
	"github.com/koltyakov/control/internal/identity"
	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/protocol"
	"github.com/koltyakov/control/internal/update"
	"google.golang.org/protobuf/proto"
)

func TestMachineRenameOwnershipReservationsAndRestart(t *testing.T) {
	g, s := installationFixture(t)
	alice, a := createTestUser(t, s, "alice")
	bob, b := createTestUser(t, s, "bob")
	id, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	ws := rawPeer(t, s.URL, a.Key, "hostname.local", alice.ID, id)
	if ws == nil {
		t.Fatal("register")
	}
	other, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	if rawPeer(t, s.URL, b.Key, "render-01", bob.ID, other) == nil {
		t.Fatal("register foreign machine")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	path := "/v1/fleet/nodes/" + id.ID
	for _, c := range []testAdmin{b, {URL: s.URL, Key: commonKey}} {
		if err := c.JSON(ctx, "PATCH", path, map[string]string{"name": "stolen"}, nil); err == nil {
			t.Fatal("unauthorized rename")
		}
	}
	for _, name := range []string{"", "-bad", "bad name", strings.Repeat("a", 64)} {
		if err := a.JSON(ctx, "PATCH", path, map[string]string{"name": name}, nil); err == nil {
			t.Fatal("invalid name accepted", name)
		}
	}
	var link enrollment.Link
	if err := a.JSON(ctx, "POST", "/v1/fleet/installations", enrollment.Request{Name: "reserved", OS: "linux", Arch: "amd64", Gateway: s.URL}, &link); err != nil {
		t.Fatal(err)
	}
	if err := a.JSON(ctx, "PATCH", path, map[string]string{"name": "reserved"}, nil); err == nil {
		t.Fatal("invitation reservation bypassed")
	}
	clientID, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	g.mu.Lock()
	g.clientOwners[clientID.ID] = true
	reservedClient := "cli-" + clientID.ID[:16]
	g.owners[clientID.ID] = alice.ID
	g.mu.Unlock()
	if err := a.JSON(ctx, "PATCH", path, map[string]string{"name": reservedClient}, nil); err == nil {
		t.Fatal("client name reservation bypassed")
	}
	var state model.MachineState
	if err := a.JSON(ctx, "PATCH", path, map[string]string{"name": "render-01"}, &state); err != nil || state.Name != "render-01" || state.Revision != 0 {
		t.Fatal("rename changed policy or failed", state, err)
	}
	_, data, err := ws.Read(ctx)
	var packet protocol.Packet
	if err != nil || proto.Unmarshal(data, &packet) != nil || packet.Kind != "directory.changed" || packet.From != update.GatewaySender {
		t.Fatal("missing cache invalidation", &packet, err)
	}
	g.mu.Lock()
	connected := g.peers[id.ID] != nil
	g.mu.Unlock()
	if !connected {
		t.Fatal("rename disconnected node")
	}
	if err := a.JSON(ctx, "PATCH", path, map[string]string{"name": "render-01"}, nil); err != nil {
		t.Fatal("repeat rename", err)
	}
	_ = g.Close()
	s.Close()
	g, err = New(filepath.Dir(g.path), commonKey, Options{SuperuserKey: superKey})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = g.Close() }()
	s = httptest.NewServer(g.Handler())
	defer s.Close()
	a.URL = s.URL
	if rawPeer(t, s.URL, a.Key, "hostname.local", alice.ID, id) == nil {
		t.Fatal("reconnect with old config rejected")
	}
	var nodes []model.Node
	if err := a.JSON(ctx, "GET", "/v1/nodes", nil, &nodes); err != nil || len(nodes) != 1 || nodes[0].ID != id.ID || nodes[0].Name != "render-01" || !nodes[0].Online {
		t.Fatal("rename lost on restart/reconnect", nodes, err)
	}
	if err := a.JSON(ctx, "POST", "/v1/fleet/installations", enrollment.Request{Name: "hostname.local", OS: "linux", Arch: "amd64", Gateway: s.URL}, &link); err != nil || link.ReplaceID != "" {
		t.Fatal("old name remains reserved", link, err)
	}
}

func TestMachineRenamePreservesInstallationProofAndReplacement(t *testing.T) {
	g, s := installationFixture(t)
	link := autoInvitation(t, s)
	id, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	proof := signedAutoProof(t, link, id, "original")
	ctx := context.Background()
	api := testAdmin{URL: link.URL}
	if err := api.JSON(ctx, "POST", "/redeem", proof, nil); err != nil {
		t.Fatal(err)
	}
	ticket := link.URL[strings.LastIndex(link.URL, "/")+1:]
	g.mu.Lock()
	i := g.installations[enrollment.Hash(ticket)]
	n := model.Node{ID: id.ID, UserID: legacyUser, Name: "original", OS: "linux", PublicKey: id.Public}
	n.Software.Arch = "amd64"
	g.nodes[id.ID] = n
	if err := g.persist(); err != nil {
		t.Error(err)
	}
	g.mu.Unlock()
	a := testAdmin{URL: s.URL, Key: superKey}
	path := "/v1/fleet/nodes/" + id.ID
	if err := a.JSON(ctx, "PATCH", path, map[string]string{"name": "alias"}, nil); err != nil {
		t.Fatal(err)
	}
	if err := api.JSON(ctx, "POST", "/redeem", proof, nil); err != nil {
		t.Fatal("rename broke signed response recovery", err)
	}
	g.mu.Lock()
	n.Name = "alias"
	allowed := g.allowInstallation("install:"+i.ID, n)
	n.ID = "foreign"
	foreignAllowed := g.allowInstallation("install:"+i.ID, n)
	g.mu.Unlock()
	if !allowed || foreignAllowed {
		t.Fatal("installation authorization changed", allowed, foreignAllowed)
	}
	oldName := invite(t, s, "original")
	if oldName.ReplaceID != "" {
		t.Fatal("old installation name still reserves identity")
	}
	replacement := invite(t, s, "alias")
	if replacement.ReplaceID != id.ID {
		t.Fatal("alias replacement did not bind existing identity")
	}
	q := enrollment.Redemption{PublicKey: id.Public, CredentialHash: enrollment.Hash("new-credential")}
	ticket = replacement.URL[strings.LastIndex(replacement.URL, "/")+1:]
	q.Signature = ed25519.Sign(id.Private, enrollment.Message(ticket, q))
	if err := (testAdmin{URL: replacement.URL}).JSON(ctx, "POST", "/redeem", q, nil); err != nil {
		t.Fatal("replace renamed enrollment", err)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.machineStates[id.ID].Name != "" || g.nodes[id.ID].Name != "alias" {
		t.Fatal("replacement did not reset override")
	}
}

func TestMachineRenamePersistenceFailureRollsBack(t *testing.T) {
	g, s := installationFixture(t)
	id, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	g.mu.Lock()
	n := model.Node{ID: id.ID, PublicKey: id.Public, UserID: legacyUser, Name: "original"}
	g.owners[id.ID], g.nodes[id.ID] = legacyUser, n
	if err := g.persist(); err != nil {
		t.Error(err)
	}
	g.mu.Unlock()
	if err := g.db.Close(); err != nil {
		t.Fatal(err)
	}
	err = (testAdmin{URL: s.URL, Key: superKey}).JSON(context.Background(), "PATCH", "/v1/fleet/nodes/"+id.ID, map[string]string{"name": "alias"}, nil)
	if err == nil {
		t.Fatal("rename acknowledged failed persistence")
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.nodes[id.ID].Name != "original" || g.machineStates[id.ID].Name != "" {
		t.Fatal("failed rename changed cache")
	}
}
