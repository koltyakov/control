package gateway

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/koltyakov/control/internal/enrollment"
	"github.com/koltyakov/control/internal/identity"
	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/update"
)

func installationFixture(t *testing.T) (*Gateway, *httptest.Server) {
	t.Helper()
	g, err := New(t.TempDir(), commonKey, Options{SuperuserKey: superKey})
	if err != nil {
		t.Fatal(err)
	}
	data := []byte("installer fixture")
	a := update.Asset{OS: "linux", Arch: "amd64", File: "control_linux_amd64", Size: int64(len(data)), SHA256: enrollment.Hash(string(data))}
	if err = g.updates.Upload(bytes.NewReader(data), a); err != nil {
		t.Fatal(err)
	}
	if _, err = g.updates.Publish(update.Manifest{Version: "test", Assets: []update.Asset{a}}, "test"); err != nil {
		t.Fatal(err)
	}
	s := httptest.NewServer(g.Handler())
	t.Cleanup(func() { s.Close(); _ = g.Close() })
	return g, s
}

func invite(t *testing.T, s *httptest.Server, name string) enrollment.Link {
	t.Helper()
	var link enrollment.Link
	err := (testAdmin{URL: s.URL, Key: superKey}).JSON(context.Background(), "POST", "/v1/admin/installations", enrollment.Request{Name: name, OS: "linux", Arch: "amd64", Gateway: s.URL}, &link)
	if err != nil {
		t.Fatal(err)
	}
	return link
}
func redeemRequest(t *testing.T, link enrollment.Link) (enrollment.Redemption, string) {
	t.Helper()
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	credential := "credential-" + identity.NewID()
	q := enrollment.Redemption{PublicKey: pub, CredentialHash: enrollment.Hash(credential)}
	ticket := link.URL[strings.LastIndex(link.URL, "/")+1:]
	q.Signature = ed25519.Sign(key, enrollment.Message(ticket, q))
	return q, credential
}

func TestInvitationSingleIdentityAtomicRedemptionAndRevocation(t *testing.T) {
	g, s := installationFixture(t)
	link := invite(t, s, "new-worker")
	for _, suffix := range []string{"", "/info", "/binary"} {
		resp, err := http.Get(link.URL + suffix)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != 200 || resp.Header.Get("Cache-Control") != "no-store" {
			t.Fatalf("installer GET %s: %d", suffix, resp.StatusCode)
		}
	}
	q1, key1 := redeemRequest(t, link)
	q2, _ := redeemRequest(t, link)
	var wg sync.WaitGroup
	results := make(chan int, 2)
	for _, q := range []enrollment.Redemption{q1, q2} {
		wg.Add(1)
		go func(q enrollment.Redemption) {
			defer wg.Done()
			b, _ := json.Marshal(q)
			resp, err := http.Post(link.URL+"/redeem", "application/json", bytes.NewReader(b))
			if err != nil {
				results <- 0
				return
			}
			_ = resp.Body.Close()
			results <- resp.StatusCode
		}(q)
	}
	wg.Wait()
	close(results)
	successes := 0
	for status := range results {
		if status == 204 {
			successes++
		} else if status != 410 {
			t.Fatalf("unexpected redemption status %d", status)
		}
	}
	if successes != 1 {
		t.Fatalf("redeemed %d identities", successes)
	}
	ticket := link.URL[strings.LastIndex(link.URL, "/")+1:]
	g.mu.Lock()
	saved := g.installations[enrollment.Hash(ticket)]
	g.mu.Unlock()
	winner := q1
	if saved.RedeemedID != identity.ID(q1.PublicKey) {
		winner = q2
	}
	if err := (testAdmin{URL: link.URL}).JSON(context.Background(), "POST", "/redeem", winner, nil); err != nil {
		t.Fatal("same identity could not recover lost response", err)
	}
	resp, err := http.Get(link.URL)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != 410 {
		t.Fatal("redeemed link still serves installer")
	}
	n := model.Node{UserID: legacyUser, ID: saved.RedeemedID, Name: link.Name, OS: link.Asset.OS}
	n.Software.Arch = link.Asset.Arch
	g.mu.Lock()
	if !g.allowInstallation("install:"+saved.ID, n) {
		t.Error("bound machine rejected")
	}
	n.Name = "other-worker"
	if g.allowInstallation("install:"+saved.ID, n) {
		t.Error("credential escaped its machine name")
	}
	n.Name = link.Name
	n.ID = "other-identity"
	if g.allowInstallation("install:"+saved.ID, n) {
		t.Error("credential escaped its identity")
	}
	g.mu.Unlock()
	if winner.CredentialHash == q1.CredentialHash {
		if role, _ := g.role(key1); role != "common" {
			t.Fatal("redeemed credential unavailable")
		}
	}
	if err := (testAdmin{URL: s.URL, Key: superKey}).JSON(context.Background(), "DELETE", "/v1/admin/installations/"+saved.ID, nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := (testAdmin{URL: link.URL}).JSON(context.Background(), "POST", "/redeem", winner, nil); err == nil {
		t.Fatal("revoked invitation recovered")
	}
}

func TestInvitationExpiryReservationAndCommonKeyDenial(t *testing.T) {
	g, s := installationFixture(t)
	q := enrollment.Request{Name: "reserved", OS: "linux", Arch: "amd64", Gateway: s.URL}
	if err := (testAdmin{URL: s.URL, Key: commonKey}).JSON(context.Background(), "POST", "/v1/admin/installations", q, nil); err == nil {
		t.Fatal("common key issued invitation")
	}
	link := invite(t, s, "reserved")
	if err := (testAdmin{URL: s.URL, Key: superKey}).JSON(context.Background(), "POST", "/v1/admin/installations", q, nil); err == nil {
		t.Fatal("duplicate name reservation")
	}
	g.mu.Lock()
	if g.allowInstallation("bootstrap", model.Node{UserID: legacyUser, Name: "reserved"}) {
		t.Error("bootstrap stole reserved name")
	}
	for hash, i := range g.installations {
		i.ExpiresAt = time.Now().Add(-time.Second)
		g.installations[hash] = i
	}
	g.mu.Unlock()
	proof, _ := redeemRequest(t, link)
	if err := (testAdmin{URL: link.URL}).JSON(context.Background(), "POST", "/redeem", proof, nil); err == nil {
		t.Fatal("expired invitation redeemed")
	}
	resp, err := http.Get(link.URL + "/binary")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != 410 {
		t.Fatal("expired invitation downloaded binary")
	}
	invite(t, s, "reserved")
}

func TestInstallationPersistsAndEnforcesSignedRegistration(t *testing.T) {
	g, server := installationFixture(t)
	link := invite(t, server, "persistent-worker")
	ticket := link.URL[strings.LastIndex(link.URL, "/")+1:]
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	credential := "unique-machine-credential"
	proof := enrollment.Redemption{PublicKey: pub, CredentialHash: enrollment.Hash(credential)}
	proof.Signature = ed25519.Sign(key, enrollment.Message(ticket, proof))
	if err = (testAdmin{URL: link.URL}).JSON(context.Background(), "POST", "/redeem", proof, nil); err != nil {
		t.Fatal(err)
	}
	server.Close()
	if err = g.Close(); err != nil {
		t.Fatal(err)
	}
	g, err = New(filepath.Dir(g.path), commonKey, Options{SuperuserKey: superKey})
	if err != nil {
		t.Fatal(err)
	}
	server = httptest.NewServer(g.Handler())
	t.Cleanup(func() { server.Close(); _ = g.Close() })
	if role, _ := g.role(credential); role != "common" {
		t.Fatal("machine credential did not survive restart")
	}
	if err = (testAdmin{URL: server.URL + "/install/" + ticket}).JSON(context.Background(), "POST", "/redeem", proof, nil); err != nil {
		t.Fatal("persisted redemption was not recoverable", err)
	}
	n := model.Node{ID: identity.ID(pub), PublicKey: pub, Name: link.Name, OS: "linux"}
	n.Software.Arch = "amd64"
	connect := func(n model.Node, accepted bool) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		ws, _, err := websocket.Dial(ctx, server.URL+"/v1/connect", &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer " + credential}}})
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = ws.CloseNow() }()
		_, challenge, err := ws.Read(ctx)
		if err != nil {
			t.Fatal(err)
		}
		hello := Hello{Version: model.Version, Node: n, Signature: ed25519.Sign(key, append(challenge, model.JSON(n)...))}
		if err = ws.Write(ctx, websocket.MessageBinary, model.JSON(hello)); err != nil {
			t.Fatal(err)
		}
		_, result, err := ws.Read(ctx)
		if accepted {
			if err != nil || string(result) != "ready" {
				t.Fatalf("bound registration rejected: %s %v", result, err)
			}
			if err = (testAdmin{URL: server.URL, Key: superKey}).JSON(ctx, "DELETE", "/v1/admin/installations/"+link.ID, nil, nil); err != nil {
				t.Fatal(err)
			}
			if _, _, err = ws.Read(ctx); err == nil {
				t.Fatal("revoked gateway session remained open")
			}
		} else if err == nil {
			t.Fatalf("unauthorized registration accepted: %s", result)
		}
	}
	other := n
	other.Name = "stolen-name"
	connect(other, false)
	other = n
	other.OS = "windows"
	connect(other, false)
	other = n
	other.Software.Arch = "arm64"
	connect(other, false)
	connect(n, true)
	if role, _ := g.role(credential); role != "" {
		t.Fatal("revoked machine credential still authenticates")
	}
}

func TestReenrollmentReplacesRegistrationAndCredential(t *testing.T) {
	g, server := installationFixture(t)
	id, err := identity.Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ws := rawPeer(t, server.URL, commonKey, "original", legacyUser, id)
	if ws == nil {
		t.Fatal("initial registration failed")
	}
	var previousLink enrollment.Link
	var previousProof enrollment.Redemption
	previousCredential := ""
	for _, name := range []string{"original", "renamed", "renamed"} {
		link := invite(t, server, name)
		if name != "renamed" || previousLink.Name == name {
			if link.ReplaceID != id.ID {
				t.Fatal("same-name invitation did not reserve existing identity")
			}
			foreign, _ := redeemRequest(t, link)
			if err := (testAdmin{URL: link.URL}).JSON(context.Background(), "POST", "/redeem", foreign, nil); err == nil {
				t.Fatal("another identity replaced a registered name")
			}
		}
		credential := "new-credential-" + identity.NewID()
		proof := enrollment.Redemption{PublicKey: id.Public, CredentialHash: enrollment.Hash(credential)}
		ticket := link.URL[strings.LastIndex(link.URL, "/")+1:]
		proof.Signature = ed25519.Sign(id.Private, enrollment.Message(ticket, proof))
		if previousCredential != "" {
			// A failed SQLite commit must preserve both the old registration and
			// credential, in memory and on disk.
			if _, err := g.db.Exec(`CREATE TRIGGER reject_enrollment BEFORE INSERT ON invitations BEGIN SELECT RAISE(FAIL, 'test failure'); END`); err != nil {
				t.Fatal(err)
			}
			if err := (testAdmin{URL: link.URL}).JSON(context.Background(), "POST", "/redeem", proof, nil); err == nil {
				t.Fatal("replacement acknowledged failed persistence")
			}
			if _, err := g.db.Exec(`DROP TRIGGER reject_enrollment`); err != nil {
				t.Fatal(err)
			}
			if role, _ := g.role(previousCredential); role != "common" {
				t.Fatal("failed replacement revoked previous credential")
			}
			if role, _ := g.role(credential); role != "" {
				t.Fatal("failed replacement issued new credential")
			}
			var savedName string
			if err := g.db.QueryRow(`SELECT name FROM nodes WHERE id = ?`, id.ID).Scan(&savedName); err != nil || savedName != previousLink.Name {
				t.Fatal("failed replacement changed persisted registration", savedName, err)
			}
			g.mu.Lock()
			if g.nodes[id.ID].Name != previousLink.Name {
				t.Error("failed replacement changed cached registration")
			}
			g.mu.Unlock()
		}
		for range 2 {
			if err := (testAdmin{URL: link.URL}).JSON(context.Background(), "POST", "/redeem", proof, nil); err != nil {
				t.Fatal("replacement or response recovery failed", err)
			}
		}
		if previousCredential != "" {
			if role, _ := g.role(previousCredential); role != "" {
				t.Fatal("old installation credential still authenticates")
			}
			if err := (testAdmin{URL: previousLink.URL}).JSON(context.Background(), "POST", "/redeem", previousProof, nil); err == nil {
				t.Fatal("superseded enrollment can be recovered")
			}
		}
		g.mu.Lock()
		if len(g.nodes) != 1 || g.nodes[id.ID].Name != name {
			t.Error("registration was duplicated or not renamed", g.nodes)
		}
		n := model.Node{UserID: legacyUser, ID: id.ID, Name: name, OS: "linux"}
		n.Software.Arch = "amd64"
		if !g.allowInstallation("install:"+link.ID, n) || g.allowInstallation("bootstrap", n) {
			t.Error("replacement identity credential binding failed")
		}
		g.mu.Unlock()
		previousLink, previousProof, previousCredential = link, proof, credential
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, _, err := ws.Read(ctx); err == nil {
		t.Fatal("old connection survived replacement")
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
	if len(restored.nodes) != 1 || restored.nodes[id.ID].Name != "renamed" {
		t.Fatal("replacement did not survive gateway restart")
	}
	if role, _ := restored.role(previousCredential); role != "common" {
		t.Fatal("replacement credential did not survive gateway restart")
	}
}

func TestReenrollmentRejectsForeignAndRetiredIdentities(t *testing.T) {
	g, server := installationFixture(t)
	alice, a := createTestUser(t, server, "alice")
	id, err := identity.Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ws := rawPeer(t, server.URL, a.Key, "alice-node", alice.ID, id)
	if ws == nil {
		t.Fatal("initial registration failed")
	}
	link := invite(t, server, "foreign-replacement")
	proof := enrollment.Redemption{PublicKey: id.Public, CredentialHash: enrollment.Hash("replacement")}
	ticket := link.URL[strings.LastIndex(link.URL, "/")+1:]
	proof.Signature = ed25519.Sign(id.Private, enrollment.Message(ticket, proof))
	if err := (testAdmin{URL: link.URL}).JSON(context.Background(), "POST", "/redeem", proof, nil); err == nil {
		t.Fatal("replacement moved an identity between fleets")
	}
	retired, err := identity.Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	g.mu.Lock()
	g.owners[retired.ID] = legacyUser
	g.machineStates[retired.ID] = model.MachineState{Revision: 1, Unregistered: true, Disabled: true}
	if err = g.persist(); err != nil {
		t.Error(err)
	}
	g.mu.Unlock()
	proof.PublicKey = retired.Public
	proof.Signature = ed25519.Sign(retired.Private, enrollment.Message(ticket, proof))
	if err := (testAdmin{URL: link.URL}).JSON(context.Background(), "POST", "/redeem", proof, nil); err == nil {
		t.Fatal("replacement reused a retired identity")
	}
}
