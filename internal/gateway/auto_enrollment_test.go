package gateway

import (
	"context"
	"crypto/ed25519"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/koltyakov/control/internal/enrollment"
	"github.com/koltyakov/control/internal/identity"
	"github.com/koltyakov/control/internal/model"
)

func autoInvitation(t *testing.T, server *httptest.Server) enrollment.Link {
	t.Helper()
	var link enrollment.Link
	request := enrollment.Request{AutoName: true, OS: "linux", Arch: "amd64", Gateway: server.URL}
	if err := (testAdmin{URL: server.URL, Key: superKey}).JSON(context.Background(), "POST", "/v1/admin/installations", request, &link); err != nil {
		t.Fatal(err)
	}
	if !link.AutoName || link.Name != "" || link.ReplaceID != "" {
		t.Fatalf("auto invitation reserved a name or identity: %+v", link.Invitation)
	}
	return link
}

func signedAutoProof(t *testing.T, link enrollment.Link, id *identity.Identity, name string) enrollment.Redemption {
	t.Helper()
	proof := enrollment.Redemption{PublicKey: id.Public, CredentialHash: enrollment.Hash("credential-" + id.ID), Name: name}
	ticket := link.URL[strings.LastIndex(link.URL, "/")+1:]
	proof.Signature = ed25519.Sign(id.Private, enrollment.Message(ticket, proof))
	return proof
}

func TestAutoNameBindsHostnameAndPersistsRecovery(t *testing.T) {
	g, server := installationFixture(t)
	link := autoInvitation(t, server)
	autoInvitation(t, server) // Unnamed invitations may coexist.
	id, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	proof := signedAutoProof(t, link, id, "TARGET-PC")
	api := testAdmin{URL: link.URL}
	tampered := proof
	tampered.Name = "other-host"
	if err = api.JSON(context.Background(), "POST", "/redeem", tampered, nil); err == nil {
		t.Fatal("unsigned hostname change was accepted")
	}
	missing := signedAutoProof(t, link, id, "")
	if err = api.JSON(context.Background(), "POST", "/redeem", missing, nil); err == nil {
		t.Fatal("hostname-less redemption was accepted")
	}
	if err = api.JSON(context.Background(), "POST", "/redeem", proof, nil); err != nil {
		t.Fatal(err)
	}
	ticket := link.URL[strings.LastIndex(link.URL, "/")+1:]
	g.mu.Lock()
	saved := g.installations[enrollment.Hash(ticket)]
	n := model.Node{UserID: legacyUser, ID: id.ID, Name: "TARGET-PC", OS: "linux"}
	n.Software.Arch = "amd64"
	allowed := g.allowInstallation("install:"+saved.ID, n)
	n.Name = "other-host"
	wrongNameAllowed := g.allowInstallation("install:"+saved.ID, n)
	g.mu.Unlock()
	if saved.Name != "TARGET-PC" || saved.RedeemedID != id.ID || !allowed || wrongNameAllowed {
		t.Fatal("resolved hostname was not bound to enrollment")
	}
	if err = api.JSON(context.Background(), "POST", "/redeem", proof, nil); err != nil {
		t.Fatal("matching proof could not recover", err)
	}
	changed := signedAutoProof(t, link, id, "new-hostname")
	if err = api.JSON(context.Background(), "POST", "/redeem", changed, nil); err == nil {
		t.Fatal("recovery renamed the host")
	}
	server.Close()
	if err = g.Close(); err != nil {
		t.Fatal(err)
	}
	restored, err := New(filepath.Dir(g.path), commonKey, Options{SuperuserKey: superKey})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = restored.Close() }()
	server = httptest.NewServer(restored.Handler())
	defer server.Close()
	if err = (testAdmin{URL: server.URL + "/install/" + ticket}).JSON(context.Background(), "POST", "/redeem", proof, nil); err != nil {
		t.Fatal("resolved-name recovery did not survive restart", err)
	}
}

func TestAutoNamePreservesReservationsAndAtomicNameClaims(t *testing.T) {
	g, server := installationFixture(t)
	link := autoInvitation(t, server)
	id, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"reserved-host", "registered-host"} {
		if name == "reserved-host" {
			invite(t, server, name)
		} else {
			other, err := identity.Generate()
			if err != nil {
				t.Fatal(err)
			}
			g.mu.Lock()
			g.owners[other.ID] = legacyUser
			g.nodes[other.ID] = model.Node{UserID: legacyUser, ID: other.ID, Name: name, PublicKey: other.Public}
			g.mu.Unlock()
		}
		if err = (testAdmin{URL: link.URL}).JSON(context.Background(), "POST", "/redeem", signedAutoProof(t, link, id, name), nil); err == nil {
			t.Fatalf("claimed reserved name %q", name)
		}
	}
	links := []enrollment.Link{link, autoInvitation(t, server)}
	other, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	ids := []*identity.Identity{id, other}
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for i, link := range links {
		proof := signedAutoProof(t, link, ids[i], "shared-host")
		wg.Go(func() {
			results <- (testAdmin{URL: link.URL}).JSON(context.Background(), "POST", "/redeem", proof, nil)
		})
	}
	wg.Wait()
	close(results)
	successes := 0
	for err := range results {
		if err == nil {
			successes++
		}
	}
	if successes != 1 {
		t.Fatalf("concurrent claims for one hostname succeeded %d times", successes)
	}
}

func TestAutoNameRollbackKeepsInvitationUnnamed(t *testing.T) {
	g, server := installationFixture(t)
	link := autoInvitation(t, server)
	id, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	proof := signedAutoProof(t, link, id, "rollback-host")
	if _, err = g.db.Exec(`CREATE TRIGGER reject_auto BEFORE INSERT ON invitations BEGIN SELECT RAISE(FAIL, 'test failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err = (testAdmin{URL: link.URL}).JSON(context.Background(), "POST", "/redeem", proof, nil); err == nil {
		t.Fatal("acknowledged failed name persistence")
	}
	g.mu.Lock()
	ticket := link.URL[strings.LastIndex(link.URL, "/")+1:]
	saved := g.installations[enrollment.Hash(ticket)]
	owner := g.owners[id.ID]
	g.mu.Unlock()
	if saved.Name != "" || saved.RedeemedID != "" || owner != "" {
		t.Fatal("failed transaction retained the name or identity claim")
	}
	if _, err = g.db.Exec(`DROP TRIGGER reject_auto`); err != nil {
		t.Fatal(err)
	}
	if err = (testAdmin{URL: link.URL}).JSON(context.Background(), "POST", "/redeem", proof, nil); err != nil {
		t.Fatal("could not recover failed persistence", err)
	}
}

func TestAutoNameRejectsRetiredAndForeignIdentities(t *testing.T) {
	g, server := installationFixture(t)
	link := autoInvitation(t, server)
	id, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	proof := signedAutoProof(t, link, id, "foreign-host")
	g.mu.Lock()
	g.owners[id.ID] = "another-fleet"
	g.mu.Unlock()
	if err = (testAdmin{URL: link.URL}).JSON(context.Background(), "POST", "/redeem", proof, nil); err == nil {
		t.Fatal("foreign identity moved fleets")
	}
	g.mu.Lock()
	delete(g.owners, id.ID)
	g.machineStates[id.ID] = model.MachineState{Revision: 1, Unregistered: true}
	g.mu.Unlock()
	if err = (testAdmin{URL: link.URL}).JSON(context.Background(), "POST", "/redeem", proof, nil); err == nil {
		t.Fatal("auto naming revived a retired identity")
	}
	// The rejected attempts must not consume the ticket or change its expiry.
	g.mu.Lock()
	for _, saved := range g.installations {
		if saved.ID == link.ID && (saved.RedeemedID != "" || !saved.ExpiresAt.After(time.Now())) {
			t.Error("rejection consumed the auto-name invitation")
		}
	}
	g.mu.Unlock()
}

func TestAutoNameReenrollmentPreservesIdentity(t *testing.T) {
	g, server := installationFixture(t)
	id, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	ws := rawPeer(t, server.URL, commonKey, "manually-named", legacyUser, id)
	if ws == nil {
		t.Fatal("initial registration failed")
	}
	link := autoInvitation(t, server)
	proof := signedAutoProof(t, link, id, "ACTUAL-HOST")
	if err = (testAdmin{URL: link.URL}).JSON(context.Background(), "POST", "/redeem", proof, nil); err != nil {
		t.Fatal(err)
	}
	g.mu.Lock()
	count, name := len(g.nodes), g.nodes[id.ID].Name
	g.mu.Unlock()
	if count != 1 || name != "ACTUAL-HOST" {
		t.Fatal("auto-name replacement changed identity or did not rename the node")
	}
}
