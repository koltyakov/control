package gateway

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/koltyakov/control/internal/enrollment"
	"github.com/koltyakov/control/internal/identity"
	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/update"
)

func TestAutomaticArchitecturePinsAssetsAndBindsRedemption(t *testing.T) {
	g, s := installationFixture(t)
	var assets []update.Asset
	for _, arch := range []string{"amd64", "arm64"} {
		data := []byte("installer-" + arch)
		a := update.Asset{OS: "linux", Arch: arch, File: update.AssetName("linux", arch), Size: int64(len(data)), SHA256: enrollment.Hash(string(data))}
		if err := g.updates.Upload(bytes.NewReader(data), a); err != nil {
			t.Fatal(err)
		}
		assets = append(assets, a)
	}
	if _, err := g.updates.Publish(update.Manifest{Version: "both", Assets: assets}, "test"); err != nil {
		t.Fatal(err)
	}
	var link enrollment.Link
	if err := (testAdmin{URL: s.URL, Key: superKey}).JSON(context.Background(), "POST", "/v1/fleet/installations", enrollment.Request{Name: "auto-worker", OS: "linux", Gateway: s.URL}, &link); err != nil {
		t.Fatal(err)
	}
	if len(link.Assets) != 2 || link.ExpiresAt.Sub(link.CreatedAt) != 15*time.Minute {
		t.Fatal("missing architecture pins or default lifetime")
	}
	// A new deployment cannot change an already issued installer.
	if _, err := g.updates.Publish(update.Manifest{Version: "later", Assets: assets[:1]}, "test"); err != nil {
		t.Fatal(err)
	}
	for _, arch := range []string{"amd64", "arm64"} {
		resp, err := http.Get(link.URL + "/binary?arch=" + arch)
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil || resp.StatusCode != 200 || string(body) != "installer-"+arch {
			t.Fatal("wrong binary selected", arch, err)
		}
		var info enrollment.Invitation
		if err := (testAdmin{URL: link.URL}).JSON(context.Background(), "GET", "/info?arch="+arch, nil, &info); err != nil || info.Asset.Arch != arch || info.Version != "both" {
			t.Fatal("metadata not pinned", err)
		}
	}
	for _, suffix := range []string{"/binary", "/binary?arch=386", "/info?arch=386"} {
		resp, err := http.Get(link.URL + suffix)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatal("unsupported architecture accepted")
		}
	}
	pub, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ticket := link.URL[strings.LastIndex(link.URL, "/")+1:]
	q := enrollment.Redemption{PublicKey: pub, CredentialHash: enrollment.Hash("machine-secret"), Arch: "arm64"}
	q.Signature = ed25519.Sign(private, enrollment.Message(ticket, q))
	forged := q
	forged.Arch = "amd64"
	if err := (testAdmin{URL: link.URL}).JSON(context.Background(), "POST", "/redeem", forged, nil); err == nil {
		t.Fatal("architecture was not signed")
	}
	if err := (testAdmin{URL: link.URL}).JSON(context.Background(), "POST", "/redeem", q, nil); err != nil {
		t.Fatal(err)
	}
	s.Close()
	_ = g.Close()
	g, err = New(filepath.Dir(g.path), commonKey, Options{SuperuserKey: superKey})
	if err != nil {
		t.Fatal(err)
	}
	s = httptest.NewServer(g.Handler())
	t.Cleanup(func() { s.Close(); _ = g.Close() })
	base := testAdmin{URL: s.URL + "/install/" + ticket}
	if err := base.JSON(context.Background(), "POST", "/redeem", q, nil); err != nil {
		t.Fatal("lost-response recovery failed after restart", err)
	}
	q.Arch = "amd64"
	q.Signature = ed25519.Sign(private, enrollment.Message(ticket, q))
	if err := base.JSON(context.Background(), "POST", "/redeem", q, nil); err == nil {
		t.Fatal("recovery changed bound architecture")
	}
	g.mu.Lock()
	saved := g.installations[enrollment.Hash(ticket)]
	n := model.Node{ID: identity.ID(pub), UserID: legacyUser, Name: link.Name, OS: "linux"}
	n.Software.Arch = "amd64"
	wrong := g.allowInstallation("install:"+saved.ID, n)
	n.Software.Arch = "arm64"
	right := g.allowInstallation("install:"+saved.ID, n)
	g.mu.Unlock()
	if wrong || !right || len(saved.Assets) != 2 {
		t.Fatal("persisted architecture binding incorrect", string(model.JSON(saved)))
	}
}
