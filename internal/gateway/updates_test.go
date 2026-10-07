package gateway

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/koltyakov/control/internal/buildinfo"
	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/protocol"
	"github.com/koltyakov/control/internal/update"
)

const commonKey = "test-common-pool-key-123456"
const superKey = "test-superuser-only-secret-key-123456789"

type testAdmin struct{ URL, Key string }

func (c testAdmin) JSON(ctx context.Context, method, path string, params, result any) error {
	b, err := json.Marshal(params)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, method, c.URL+path, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	if result != nil {
		return json.NewDecoder(resp.Body).Decode(result)
	}
	return nil
}

func TestAdministrativeKeysAndUpdateAuthorization(t *testing.T) {
	dir := t.TempDir()
	g, err := New(dir, commonKey, Options{SuperuserKey: superKey})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(g.Handler())
	t.Cleanup(func() { server.Close(); _ = g.Close() })
	ctx := context.Background()
	admin := testAdmin{URL: server.URL, Key: superKey}
	var issued struct {
		Key   APIKey `json:"key"`
		Token string `json:"token"`
	}
	if err = admin.JSON(ctx, "POST", "/v1/admin/keys", map[string]string{"name": "coworker"}, &issued); err != nil {
		t.Fatal(err)
	}
	for _, token := range []string{commonKey, issued.Token, "wrong"} {
		for _, route := range []struct{ method, path string }{{"GET", "/v1/admin/updates"}, {"POST", "/v1/admin/updates"}, {"PUT", "/v1/admin/updates/blobs/" + strings.Repeat("a", 64)}, {"POST", "/v1/admin/updates/check"}, {"POST", "/v1/admin/keys"}, {"DELETE", "/v1/admin/keys/" + issued.Key.ID}} {
			req, _ := http.NewRequest(route.method, server.URL+route.path, strings.NewReader("{}"))
			req.Header.Set("Authorization", "Bearer "+token)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusForbidden {
				t.Fatalf("%s %s allowed common/invalid key: %d", route.method, route.path, resp.StatusCode)
			}
		}
	}
	common := testAdmin{URL: server.URL, Key: issued.Token}
	var info struct {
		Role         string
		Capabilities []string
	}
	if err = common.JSON(ctx, "GET", "/v1/auth", nil, &info); err != nil || info.Role != "common" || len(info.Capabilities) != 0 {
		t.Fatalf("common key scope: %+v %v", info, err)
	}
	server.Close()
	_ = g.Close()
	g, err = New(dir, commonKey, Options{SuperuserKey: superKey})
	if err != nil {
		t.Fatal(err)
	}
	server = httptest.NewServer(g.Handler())
	admin.URL, common.URL = server.URL, server.URL
	if err = common.JSON(ctx, "GET", "/v1/nodes", nil, nil); err != nil {
		t.Fatal("key lost on restart", err)
	}
	if err = admin.JSON(ctx, "DELETE", "/v1/admin/keys/"+issued.Key.ID, nil, nil); err != nil {
		t.Fatal(err)
	}
	if err = common.JSON(ctx, "GET", "/v1/nodes", nil, nil); err == nil {
		t.Fatal("revoked key still works")
	}
}

func TestReleaseFetchChecksManifestAndDoesNotReplaceNewerDevelopment(t *testing.T) {
	data := []byte("release executable fixture")
	sum := sha256.Sum256(data)
	asset := update.Asset{OS: "linux", Arch: "amd64", File: update.AssetName("linux", "amd64"), Size: int64(len(data)), SHA256: hex.EncodeToString(sum[:])}
	manifest := update.Manifest{Version: "v2", Assets: []update.Asset{asset}}
	published := time.Now().Add(-time.Hour).UTC()
	var base string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/owner/repo/releases/latest":
			_ = json.NewEncoder(w).Encode(release{Tag: "v2", Published: published, Assets: []releaseAsset{{"control-manifest.json", base + "/manifest"}, {asset.File, base + "/binary"}}})
		case "/manifest":
			_ = json.NewEncoder(w).Encode(manifest)
		case "/binary":
			_, _ = w.Write(data)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	base = server.URL
	g, err := New(t.TempDir(), commonKey, Options{SuperuserKey: superKey, Software: buildinfo.Info{OS: "linux", Arch: "amd64"}, ReleaseRepo: "owner/repo", ReleaseAPI: base})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = g.Close() }()
	d, err := g.fetchRelease(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if d.Manifest.Version != "v2" || d.Source != "github:owner/repo" {
		t.Fatalf("wrong release: %+v", d)
	}
	if err = update.Verify(g.updates.Blob(asset.SHA256), asset); err != nil {
		t.Fatal(err)
	}
	manifest.Version, manifest.CreatedAt = "dev-test", time.Now().UTC()
	dev, err := g.updates.Publish(manifest, "development")
	if err != nil {
		t.Fatal(err)
	}
	d, err = g.fetchRelease(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if d.ID != dev.ID {
		t.Fatal("old release replaced development build")
	}
	if err = validateReleaseURL(base, "https://other.example/asset"); err == nil {
		t.Fatal("allowed release token to another origin")
	}
	if err = g.updates.Upload(bytes.NewReader([]byte("corrupt")), asset); err == nil {
		t.Fatal("corrupt release blob accepted")
	}
}

func TestGatewayRestartRequiresCurrentReservedMembership(t *testing.T) {
	asset := update.Asset{OS: "linux", Arch: "amd64", SHA256: strings.Repeat("a", 64)}
	d := &update.Deployment{ID: "deployment", Manifest: update.Manifest{Assets: []update.Asset{asset}}}
	status := update.Status{ID: d.ID, Software: buildinfo.Info{OS: asset.OS, Arch: asset.Arch, SHA256: asset.SHA256}, Paused: true, SeenAt: time.Now(), LeaseUntil: time.Now().Add(time.Minute)}
	g := &Gateway{peers: map[string]*connection{"worker": {}}, updateStatus: map[string]update.Status{"worker": status}}
	peers := []rolloutPeer{{node: model.Node{ID: "worker"}}}
	g.peers["newcomer"] = &connection{}
	if g.reserveRestart(d, peers) {
		t.Fatal("gateway reserved despite new peer absent from idle snapshot")
	}
	delete(g.peers, "newcomer")
	busy := status
	busy.Busy = true
	g.updateStatus["worker"] = busy
	if g.reserveRestart(d, peers) {
		t.Fatal("gateway reserved with newly active peer")
	}
	g.updateStatus["worker"] = status
	if !g.reserveRestart(d, peers) || !g.restarting {
		t.Fatal("idle acknowledged membership did not reserve restart")
	}
	d.Manifest.Version, status.Software.Version = "v1", "v1"
	status.Software.SHA256 = strings.Repeat("b", 64)
	g.updateStatus["worker"] = status
	if !g.reserveRestart(d, peers) {
		t.Fatal("same-version participant blocked gateway restart despite a live idle reservation")
	}
}

func TestRolloutSkipsSameVersionWhileBusy(t *testing.T) {
	data := []byte("rebuilt binary")
	sum := sha256.Sum256(data)
	asset := update.Asset{OS: "linux", Arch: "amd64", File: update.AssetName("linux", "amd64"), Size: int64(len(data)), SHA256: hex.EncodeToString(sum[:])}
	info := buildinfo.Info{Version: "v1", OS: asset.OS, Arch: asset.Arch, SHA256: strings.Repeat("a", 64)}
	g, err := New(t.TempDir(), commonKey, Options{SuperuserKey: superKey, Software: info, Apply: func(string) error {
		t.Error("same-version rollout restarted gateway")
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		delete(g.peers, "worker") // The rollout fixture has no WebSocket to close.
		_ = g.Close()
	}()
	if err = g.updates.Upload(bytes.NewReader(data), asset); err != nil {
		t.Fatal(err)
	}
	d, err := g.updates.Publish(update.Manifest{Version: info.Version, Assets: []update.Asset{asset}}, "development")
	if err != nil {
		t.Fatal(err)
	}
	obsolete := oldUpdateAsset(t, g, "obsolete binary")
	g.nodes["worker"] = model.Node{ID: "worker", OS: info.OS, Software: info}
	out := make(chan *protocol.Packet, 8)
	g.peers["worker"] = &connection{out: out}
	g.updateStatus["worker"] = update.Status{ID: d.ID, Software: info, Busy: true, State: "applied", SeenAt: time.Now()}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); g.rolloutLoop(ctx) }()
	defer func() { cancel(); <-done }()
	for {
		select {
		case <-ctx.Done():
			t.Fatal("same-version rollout did not complete")
		case packet := <-out:
			switch packet.Kind {
			case "update.prepare", "update.commit":
				t.Fatalf("same-version rollout requested maintenance: %s", packet.Kind)
			case "update.resume":
				for update.Verify(g.updates.Blob(obsolete.SHA256), obsolete) == nil {
					if ctx.Err() != nil {
						t.Fatal("successful rollout did not prune obsolete binaries")
					}
					time.Sleep(10 * time.Millisecond)
				}
				cancel()
				<-done
				if current := g.updates.Current(); current.Phase != "complete" || g.restarting {
					t.Fatalf("same-version rollout was not skipped: %+v", current)
				}
				if err := update.Verify(g.updates.Blob(obsolete.SHA256), obsolete); err == nil {
					t.Fatal("successful rollout did not prune obsolete binaries")
				}
				return
			}
		}
	}
}
