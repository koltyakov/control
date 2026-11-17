package gateway

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/koltyakov/control/internal/buildinfo"
	"github.com/koltyakov/control/internal/testutil"
	"github.com/koltyakov/control/internal/update"
)

func TestReleaseFetchPinnedSignaturesFailClosed(t *testing.T) {
	signer := testutil.NewReleaseSigner(t)
	keys := update.ReleaseKeyring{"owner/repo": {signer.KeyID: signer.PublicKey()}}
	for _, damage := range []string{"none", "wrong-key", "unknown-key", "wrong-repository", "wrong-tag", "bytes", "malformed", "oversized", "missing", "legacy", "corrupt-binary"} {
		t.Run(damage, func(t *testing.T) {
			binary := []byte("authenticated release binary")
			// Reuse the existing manifest constraints and actual SHA-256 checking.
			sum := sha256.Sum256(binary)
			asset := update.Asset{OS: "linux", Arch: "amd64", File: update.AssetName("linux", "amd64"), Size: int64(len(binary)), SHA256: hex.EncodeToString(sum[:])}
			manifest, err := json.Marshal(update.Manifest{Version: "v2", Assets: []update.Asset{asset}})
			if err != nil {
				t.Fatal(err)
			}
			proof := signer.Sign(t, "owner/repo", "v2", manifest)
			switch damage {
			case "wrong-key":
				proof = testutil.NewReleaseSigner(t).Sign(t, "owner/repo", "v2", manifest)
			case "unknown-key":
				other := signer
				other.KeyID = "unknown"
				proof = other.Sign(t, "owner/repo", "v2", manifest)
			case "wrong-repository":
				proof = signer.Sign(t, "other/repo", "v2", manifest)
			case "wrong-tag":
				proof = signer.Sign(t, "owner/repo", "v3", manifest)
			case "bytes":
				manifest = append(manifest, ' ')
			case "malformed":
				proof = []byte(`{}`)
			case "oversized":
				proof = bytes.Repeat([]byte(" "), update.MaxReleaseSignatureSize+1)
			case "legacy":
				proof = []byte(`{"mediaType":"application/vnd.dev.sigstore.bundle.v0.3+json"}`)
			}
			var downloads atomic.Int32
			var base string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/repos/owner/repo/releases/latest":
					assets := []releaseAsset{{"control-manifest.json", base + "/manifest"}, {asset.File, base + "/binary"}}
					if damage != "missing" {
						assets = append(assets, releaseAsset{update.ReleaseSignatureName, base + "/proof"})
					}
					_ = json.NewEncoder(w).Encode(release{Tag: "v2", Published: time.Now().UTC(), Assets: assets})
				case "/manifest":
					_, _ = w.Write(manifest)
				case "/proof":
					_, _ = w.Write(proof)
				case "/binary":
					downloads.Add(1)
					if damage == "corrupt-binary" {
						_, _ = w.Write([]byte(strings.Repeat("x", len(binary))))
					} else {
						_, _ = w.Write(binary)
					}
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
			previousBytes := []byte("previous deployment")
			previousHash := sha256.Sum256(previousBytes)
			previousAsset := asset
			previousAsset.Size, previousAsset.SHA256 = int64(len(previousBytes)), hex.EncodeToString(previousHash[:])
			if err := g.updates.Upload(bytes.NewReader(previousBytes), previousAsset); err != nil {
				t.Fatal(err)
			}
			previous, err := g.updates.Publish(update.Manifest{Version: "v1", CreatedAt: time.Unix(1, 0).UTC(), Assets: []update.Asset{previousAsset}}, "development")
			if err != nil {
				t.Fatal(err)
			}
			deployment, err := g.fetchReleaseVerified(t.Context(), keys.Verify)
			if damage == "none" {
				if err != nil || deployment == nil || downloads.Load() != 1 {
					t.Fatal("valid release not published", err)
				}
				if err := update.Verify(g.updates.Blob(asset.SHA256), asset); err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || deployment != nil || g.updates.Current().ID != previous.ID {
				t.Fatal("failed release changed selection", err)
			}
			if damage != "corrupt-binary" && downloads.Load() != 0 {
				t.Fatal("untrusted manifest reached binary download")
			}
		})
	}
}
