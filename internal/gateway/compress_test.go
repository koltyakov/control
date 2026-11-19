package gateway

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/koltyakov/control/internal/model"
)

func TestFleetMetadataIsCompressedOnlyWhenAccepted(t *testing.T) {
	g, err := New(t.TempDir(), commonKey)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = g.Close() }()
	g.mu.Lock()
	n := model.Node{ID: "worker", Name: "worker", UserID: legacyUser, Labels: map[string]string{"role": strings.Repeat("render ", 64)}}
	g.nodes[n.ID], g.owners[n.ID] = n, legacyUser
	g.mu.Unlock()
	g.publishObservations(n.ID)
	handler := g.Handler()
	get := func(path, encoding, condition string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, path, nil)
		r.Header.Set("Authorization", "Bearer "+commonKey)
		if encoding != "" {
			r.Header.Set("Accept-Encoding", encoding)
		}
		if condition != "" {
			r.Header.Set("If-None-Match", condition)
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	for _, path := range []string{"/v1/nodes", "/v1/status"} {
		plain := get(path, "", "")
		compressed := get(path, "br, gzip;q=0.8", "")
		if plain.Code != http.StatusOK || compressed.Code != http.StatusOK || plain.Header().Get("Content-Encoding") != "" || compressed.Header().Get("Content-Encoding") != "gzip" {
			t.Fatalf("%s encodings: %d %v / %d %v", path, plain.Code, plain.Header(), compressed.Code, compressed.Header())
		}
		if !strings.Contains(compressed.Header().Get("Vary"), "Accept-Encoding") {
			t.Fatalf("%s does not vary by encoding: %v", path, compressed.Header())
		}
		reader, err := gzip.NewReader(compressed.Body)
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(reader)
		if err != nil {
			t.Fatal(err)
		}
		if path == "/v1/nodes" && !bytes.Equal(body, plain.Body.Bytes()) {
			t.Fatalf("compressed directory differs:\n%s\n%s", body, plain.Body.Bytes())
		}
		if !bytes.Contains(body, []byte(`"worker"`)) || compressed.Body.Len() >= plain.Body.Len() {
			t.Fatalf("%s compressed %d of %d bytes", path, compressed.Body.Len(), plain.Body.Len())
		}
		if refused := get(path, "gzip;q=0, identity", ""); refused.Header().Get("Content-Encoding") != "" {
			t.Fatalf("%s ignored a refused coding", path)
		}
	}
	// The compressed directory carries a weak validator that still revalidates.
	plainTag, weakTag := get("/v1/nodes", "", "").Header().Get("ETag"), get("/v1/nodes", "gzip", "").Header().Get("ETag")
	if plainTag == "" || weakTag != "W/"+plainTag {
		t.Fatalf("validators: %q %q", plainTag, weakTag)
	}
	if w := get("/v1/nodes", "gzip", weakTag); w.Code != http.StatusNotModified || w.Header().Get("ETag") != weakTag || w.Body.Len() != 0 {
		t.Fatalf("weak revalidation: %d %v", w.Code, w.Header())
	}
}

func TestAcceptsGzip(t *testing.T) {
	for header, want := range map[string]bool{"": false, "gzip": true, "GZIP": true, "deflate, gzip": true, "gzip;q=0": false, "gzip; q=0.0": false, "gzip;q=0.5": true, "br": false, "x-gzip": false} {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.Header.Set("Accept-Encoding", header)
		if acceptsGzip(r) != want {
			t.Errorf("%q: want %v", header, want)
		}
	}
}
