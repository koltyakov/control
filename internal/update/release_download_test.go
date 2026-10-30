package update

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func TestManagedDownloadPrefersExactSignedReleaseWithGatewayFallback(t *testing.T) {
	data := []byte("trusted managed executable")
	sum := sha256.Sum256(data)
	asset := Asset{OS: "linux", Arch: "amd64", File: AssetName("linux", "amd64"), Size: int64(len(data)), SHA256: hex.EncodeToString(sum[:])}
	for _, scenario := range []string{"matching", "missing", "unavailable", "version-mismatch", "checksum-mismatch", "size-mismatch", "bad-signature", "missing-proof", "missing-binary", "corrupt-binary", "no-repository", "both-unavailable"} {
		t.Run(scenario, func(t *testing.T) {
			manifest := Manifest{Version: "v1", Assets: []Asset{asset}}
			if scenario == "version-mismatch" {
				manifest.Version = "v2"
			}
			if scenario == "checksum-mismatch" {
				manifest.Assets = []Asset{asset}
				manifest.Assets[0].SHA256 = strings.Repeat("f", 64)
			}
			if scenario == "size-mismatch" {
				manifest.Assets[0].Size++
			}
			var binaries, fallbacks, proofs atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/gateway" {
					fallbacks.Add(1)
					if r.Header.Get("Authorization") != "Bearer private-fleet-key" {
						t.Error("gateway fallback lost authentication")
					}
					if scenario == "both-unavailable" {
						http.Error(w, "unavailable", http.StatusServiceUnavailable)
						return
					}
					_, _ = w.Write(data)
					return
				}
				if r.Header.Get("Authorization") != "" {
					t.Error("fleet credential leaked to public release host")
				}
				switch r.URL.Path {
				case "/release/control-manifest.json":
					if scenario == "missing" || scenario == "both-unavailable" {
						http.NotFound(w, r)
						return
					}
					if scenario == "unavailable" {
						http.Error(w, "unavailable", http.StatusServiceUnavailable)
						return
					}
					_ = json.NewEncoder(w).Encode(manifest)
				case "/release/control-manifest.json.sigstore.json":
					if scenario == "missing-proof" {
						http.NotFound(w, r)
						return
					}
					_, _ = w.Write([]byte("fixture signature"))
				case "/release/" + asset.File:
					binaries.Add(1)
					if scenario == "missing-binary" {
						http.NotFound(w, r)
						return
					}
					if scenario == "corrupt-binary" {
						_, _ = w.Write([]byte(strings.Repeat("x", len(data))))
						return
					}
					_, _ = w.Write(data)
				default:
					t.Error("unexpected release URL", r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			verify := func(_ context.Context, repository, tag string, manifest, proof []byte) error {
				proofs.Add(1)
				if repository != "owner/repo" || tag != "v1" || len(manifest) == 0 || string(proof) != "fixture signature" {
					t.Fatal("wrong signed release verification inputs")
				}
				if scenario == "bad-signature" {
					return errors.New("invalid signature")
				}
				return nil
			}
			path := filepath.Join(t.TempDir(), "binary")
			releases := server.URL + "/release"
			if scenario == "no-repository" {
				releases = ""
			}
			err := downloadPreferred(t.Context(), server.Client(), "owner/repo", "v1", releases, server.URL+"/gateway", "private-fleet-key", path, asset, verify)
			if scenario == "both-unavailable" {
				if err == nil {
					t.Fatal("unavailable sources acknowledged a download")
				}
				if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
					t.Fatal("unverified download was published", statErr)
				}
				return
			}
			if err != nil || Verify(path, asset) != nil {
				t.Fatal("neither exact release nor verified fallback completed", err)
			}
			if scenario == "matching" {
				if fallbacks.Load() != 0 || binaries.Load() != 1 || proofs.Load() != 1 {
					t.Fatal("matching release was not preferred", fallbacks.Load(), binaries.Load(), proofs.Load())
				}
			} else if fallbacks.Load() != 1 {
				t.Fatal("gateway fallback was not used exactly once", fallbacks.Load())
			}
			if strings.HasSuffix(scenario, "mismatch") || scenario == "bad-signature" {
				if binaries.Load() != 0 {
					t.Fatal("different or untrusted release reached executable download")
				}
			}
			// A verified staged asset is reused without contacting either source.
			if err := downloadPreferred(t.Context(), server.Client(), "owner/repo", "v1", releases, server.URL+"/gateway", "private-fleet-key", path, asset, verify); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestManagedReleaseCancellationDoesNotTryGateway(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("cancelled download contacted a source")
	}))
	defer server.Close()
	err := downloadPreferred(ctx, server.Client(), "owner/repo", "v1", server.URL+"/release", server.URL+"/gateway", "key", filepath.Join(t.TempDir(), "binary"), Asset{Size: 1}, VerifyReleaseManifest)
	if !errors.Is(err, context.Canceled) {
		t.Fatal("download cancellation was not preserved", err)
	}
}

func TestManagedGatewayFallbackNeverFollowsRedirects(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/gateway" {
			t.Error("fleet credential followed a redirect")
		}
		w.Header().Set("Location", "/unexpected")
		w.WriteHeader(http.StatusFound)
	}))
	defer server.Close()
	err := DownloadPreferred(t.Context(), "../untrusted", "v1", server.URL+"/gateway", "key", filepath.Join(t.TempDir(), "binary"), Asset{Size: 1})
	if err == nil {
		t.Fatal("redirected fallback succeeded")
	}
}
