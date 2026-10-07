package gateway

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/koltyakov/control/internal/buildinfo"
	"github.com/koltyakov/control/internal/enrollment"
	"github.com/koltyakov/control/internal/update"
)

func oldUpdateAsset(t *testing.T, g *Gateway, content string) update.Asset {
	t.Helper()
	sum := sha256.Sum256([]byte(content))
	asset := update.Asset{OS: "linux", Arch: "amd64", File: update.AssetName("linux", "amd64"), Size: int64(len(content)), SHA256: hex.EncodeToString(sum[:])}
	if err := g.updates.Upload(strings.NewReader(content), asset); err != nil {
		t.Fatal(err)
	}
	when := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(g.updates.Blob(asset.SHA256), when, when); err != nil {
		t.Fatal(err)
	}
	return asset
}

func TestUpdateCleanupPreservesLiveInvitationDownloads(t *testing.T) {
	g, err := New(t.TempDir(), commonKey, Options{SuperuserKey: superKey})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = g.Close() }()
	selected := oldUpdateAsset(t, g, "selected")
	active := oldUpdateAsset(t, g, "active invitation")
	second := oldUpdateAsset(t, g, "second architecture")
	expired := oldUpdateAsset(t, g, "expired invitation")
	revoked := oldUpdateAsset(t, g, "revoked invitation")
	redeemed := oldUpdateAsset(t, g, "redeemed invitation")
	ticket := strings.Repeat("a", 43)
	g.installations[enrollment.Hash(ticket)] = installation{Invitation: enrollment.Invitation{UserID: "legacy", Asset: active, Assets: []update.Asset{active, second}, ExpiresAt: time.Now().Add(time.Hour)}}
	for name, asset := range map[string]update.Asset{"expired": expired, "revoked": revoked, "redeemed": redeemed} {
		i := installation{Invitation: enrollment.Invitation{Asset: asset, ExpiresAt: time.Now().Add(time.Hour)}}
		switch name {
		case "expired":
			i.ExpiresAt = time.Now().Add(-time.Hour)
		case "revoked":
			i.Revoked = true
		case "redeemed":
			i.RedeemedID = "worker"
		}
		g.installations[name] = i
	}
	d, err := g.updates.Publish(update.Manifest{Version: "v1", Assets: []update.Asset{selected}}, "test")
	if err != nil {
		t.Fatal(err)
	}
	g.cleanupUpdates(t.Context(), d.ID)
	if err := update.Verify(g.updates.Blob(expired.SHA256), expired); err != nil {
		t.Fatal("cleanup ran before rollout completion", err)
	}
	if err := g.updates.SetPhase(d.ID, "complete", nil); err != nil {
		t.Fatal(err)
	}
	g.cleanupUpdates(t.Context(), d.ID)
	for _, asset := range []update.Asset{selected, active, second} {
		if err := update.Verify(g.updates.Blob(asset.SHA256), asset); err != nil {
			t.Fatal("live reference was pruned", err)
		}
	}
	for _, asset := range []update.Asset{expired, revoked, redeemed} {
		if _, err := os.Stat(g.updates.Blob(asset.SHA256)); !os.IsNotExist(err) {
			t.Fatalf("obsolete invitation binary retained: %v", err)
		}
	}
	handler := g.Handler()
	for _, test := range []struct {
		path string
		code int
	}{
		{"/v1/updates/blobs/" + selected.SHA256, http.StatusOK},
		{"/v1/updates/blobs/" + active.SHA256, http.StatusNotFound},
		{"/v1/updates/blobs/" + second.SHA256, http.StatusNotFound},
		{"/install/" + ticket + "/binary?arch=amd64", http.StatusOK},
	} {
		req := httptest.NewRequest(http.MethodGet, test.path, nil)
		req.Header.Set("Authorization", "Bearer "+commonKey)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, req)
		if response.Code != test.code {
			t.Fatalf("%s: got %d, want %d: %s", test.path, response.Code, test.code, response.Body.String())
		}
	}
	i := g.installations[enrollment.Hash(ticket)]
	i.ExpiresAt = time.Now().Add(-time.Hour)
	g.installations[enrollment.Hash(ticket)] = i
	g.cleanupUpdates(t.Context(), d.ID)
	if _, err := os.Stat(g.updates.Blob(active.SHA256)); !os.IsNotExist(err) {
		t.Fatalf("expired pin retained: %v", err)
	}
}

func TestCompletedRolloutPrunesAfterGatewayRestart(t *testing.T) {
	dir := t.TempDir()
	options := Options{SuperuserKey: superKey, Software: buildinfo.Info{Version: "v1", OS: "linux", Arch: "amd64"}, Apply: func(string) error {
		t.Error("completed rollout restarted gateway")
		return nil
	}}
	g, err := New(dir, commonKey, options)
	if err != nil {
		t.Fatal(err)
	}
	selected := oldUpdateAsset(t, g, "selected")
	old := oldUpdateAsset(t, g, "old")
	staged := filepath.Join(dir, "updates", "staged", old.SHA256)
	if err := os.MkdirAll(staged, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(staged, old.File), []byte("old"), 0700); err != nil {
		t.Fatal(err)
	}
	d, err := g.updates.Publish(update.Manifest{Version: "v1", Assets: []update.Asset{selected}}, "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := g.updates.SetPhase(d.ID, "complete", nil); err != nil {
		t.Fatal(err)
	}
	if err := g.Close(); err != nil {
		t.Fatal(err)
	}
	g, err = New(dir, commonKey, options)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = g.Close() }()
	g.StartUpdates(t.Context())
	deadline := time.Now().Add(5 * time.Second)
	for {
		_, blobErr := os.Stat(g.updates.Blob(old.SHA256))
		_, stageErr := os.Stat(staged)
		if os.IsNotExist(blobErr) && os.IsNotExist(stageErr) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("completed rollout did not automatically prune after restart")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := update.Verify(g.updates.Blob(selected.SHA256), selected); err != nil {
		t.Fatal("selected release cannot update offline nodes", err)
	}
}
