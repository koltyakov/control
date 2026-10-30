package update

import (
	"context"
	"crypto"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sigstore/sigstore-go/pkg/tuf"
	"github.com/sigstore/sigstore/pkg/signature"
	"github.com/theupdateframework/go-tuf/v2/metadata"
)

type trustTransport func(*http.Request) (*http.Response, error)

func (f trustTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func trustResponse(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}
}

func TestTrustFetcherUsesOfficialMirrorOnlyForAvailabilityFailures(t *testing.T) {
	for _, tc := range []struct {
		name, target string
		status       int
		mirror       bool
	}{
		{"available", tuf.DefaultMirror + "/15.root.json", 200, false},
		{"forbidden", tuf.DefaultMirror + "/15.root.json", 403, true},
		{"timeout", tuf.DefaultMirror + "/timestamp.json", 408, true},
		{"rate-limit", tuf.DefaultMirror + "/timestamp.json", 429, true},
		{"server-error", tuf.DefaultMirror + "/timestamp.json", 503, true},
		{"missing-next-root", tuf.DefaultMirror + "/16.root.json", 404, false},
		{"other-host", "https://example.invalid/15.root.json", 403, false},
		{"http", "http://tuf-repo-cdn.sigstore.dev/15.root.json", 403, false},
		{"host-prefix", "https://tuf-repo-cdn.sigstore.dev.attacker.invalid/15.root.json", 403, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var requests []string
			client := &http.Client{Transport: trustTransport(func(r *http.Request) (*http.Response, error) {
				requests = append(requests, r.URL.String())
				if len(requests) == 1 {
					return trustResponse(tc.status, "primary metadata"), nil
				}
				if r.URL.Host != "sigstore.github.io" || !strings.HasPrefix(r.URL.Path, "/root-signing/") || r.Header.Get("Authorization") != "" {
					t.Fatal("unexpected mirror or credential forwarding", r.URL, r.Header)
				}
				return trustResponse(http.StatusOK, "mirror metadata"), nil
			})}
			data, err := (trustFetcher{ctx: t.Context(), client: client}).DownloadFile(tc.target, 100, time.Second)
			if tc.mirror {
				if err != nil || string(data) != "mirror metadata" || len(requests) != 2 {
					t.Fatal("available official mirror was not used", string(data), requests, err)
				}
			} else if len(requests) != 1 || (err == nil) != (tc.status == http.StatusOK) {
				t.Fatal("unexpected fallback or response", requests, err)
			}
		})
	}
}

func TestTrustMirrorPreservesMissingRootLimitsAndCancellation(t *testing.T) {
	for _, scenario := range []string{"missing-root", "oversize", "cancelled", "network"} {
		t.Run(scenario, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			requests := 0
			client := &http.Client{Transport: trustTransport(func(r *http.Request) (*http.Response, error) {
				requests++
				if requests == 1 {
					switch scenario {
					case "oversize":
						return trustResponse(http.StatusOK, strings.Repeat("x", 101)), nil
					case "cancelled":
						cancel()
						return nil, context.Canceled
					case "network":
						return nil, errors.New("network unavailable")
					}
					return trustResponse(http.StatusForbidden, "unavailable in this location"), nil
				}
				if scenario == "network" {
					return trustResponse(http.StatusOK, "metadata"), nil
				}
				return trustResponse(http.StatusNotFound, "no newer root"), nil
			})}
			_, err := (trustFetcher{ctx: ctx, client: client}).DownloadFile(tuf.DefaultMirror+"/16.root.json", 100, time.Second)
			switch scenario {
			case "missing-root":
				var status *metadata.ErrDownloadHTTP
				if !errors.As(err, &status) || status.StatusCode != http.StatusNotFound || requests != 2 {
					t.Fatal("missing next root did not retain TUF's normal termination signal", requests, err)
				}
			case "oversize", "cancelled":
				if err == nil || requests != 1 {
					t.Fatal("limits or cancellation caused mirror retry", requests, err)
				}
			case "network":
				if err != nil || requests != 2 {
					t.Fatal("network failure did not use mirror", requests, err)
				}
			}
		})
	}
}

func TestTrustMirrorStillRequiresValidTUFMetadata(t *testing.T) {
	for _, scenario := range []string{"valid", "untrusted-root", "tampered-targets", "expired-timestamp"} {
		t.Run(scenario, func(t *testing.T) {
			public, private, err := ed25519.GenerateKey(rand.Reader)
			if err != nil {
				t.Fatal(err)
			}
			key, err := metadata.KeyFromPublicKey(public)
			if err != nil {
				t.Fatal(err)
			}
			signer, err := signature.LoadSignerVerifier(private, crypto.Hash(0))
			if err != nil {
				t.Fatal(err)
			}
			expires := time.Now().Add(time.Hour).UTC()
			root := metadata.Root(expires)
			root.Signed.ConsistentSnapshot = false
			for _, role := range []string{metadata.ROOT, metadata.TARGETS, metadata.SNAPSHOT, metadata.TIMESTAMP} {
				if err := root.Signed.AddKey(key, role); err != nil {
					t.Fatal(err)
				}
			}
			targets := metadata.Targets(expires)
			snapshot := metadata.Snapshot(expires)
			timestamp := metadata.Timestamp(expires)
			if scenario == "expired-timestamp" {
				timestamp.Signed.Expires = time.Now().Add(-time.Hour)
			}
			for _, sign := range []func(signature.Signer) (*metadata.Signature, error){root.Sign, targets.Sign, snapshot.Sign, timestamp.Sign} {
				if _, err := sign(signer); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "tampered-targets" {
				targets.Signed.Version++
			}
			encode := func(value any) string {
				t.Helper()
				data, err := json.Marshal(value)
				if err != nil {
					t.Fatal(err)
				}
				return string(data)
			}
			files := map[string]string{"timestamp.json": encode(timestamp), "snapshot.json": encode(snapshot), "targets.json": encode(targets)}
			if scenario == "untrusted-root" {
				rotated := metadata.Root(expires)
				rotated.Signed = root.Signed
				rotated.Signed.Version = 2
				files["2.root.json"] = encode(rotated) // No signatures from the trusted root.
			}
			client := &http.Client{Transport: trustTransport(func(r *http.Request) (*http.Response, error) {
				if r.URL.Host == "tuf-repo-cdn.sigstore.dev" {
					return trustResponse(http.StatusForbidden, "location unavailable"), nil
				}
				if r.URL.Host != "sigstore.github.io" {
					t.Fatal("unexpected trust metadata source", r.URL)
				}
				if data, ok := files[strings.TrimPrefix(r.URL.Path, "/root-signing/")]; ok {
					return trustResponse(http.StatusOK, data), nil
				}
				return trustResponse(http.StatusNotFound, "not found"), nil
			})}
			opts := tuf.DefaultOptions().WithContext(t.Context()).WithRoot([]byte(encode(root))).WithDisableLocalCache().WithFetcher(trustFetcher{ctx: t.Context(), client: client})
			_, err = tuf.New(opts)
			if (err == nil) != (scenario == "valid") {
				t.Fatal("mirror bypassed TUF verification or rejected valid metadata", scenario, err)
			}
		})
	}
}
