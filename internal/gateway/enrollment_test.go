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
		defer ws.CloseNow()
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
