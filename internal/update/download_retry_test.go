package update

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func TestGatewayDownloadRetriesDuringGatewayRestart(t *testing.T) {
	data := []byte("selected executable bytes")
	sum := sha256.Sum256(data)
	asset := Asset{OS: "linux", Arch: "amd64", File: AssetName("linux", "amd64"), Size: int64(len(data)), SHA256: hex.EncodeToString(sum[:])}
	for _, tc := range []struct {
		name     string
		failures int32
		status   int
		ok       bool
		requests int32
	}{
		{"restart window", 2, http.StatusBadGateway, true, 3},
		{"unavailable", 1, http.StatusServiceUnavailable, true, 2},
		{"not selected", 1, http.StatusNotFound, false, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if requests.Add(1) <= tc.failures {
					w.WriteHeader(tc.status)
					return
				}
				_, _ = w.Write(data)
			}))
			defer server.Close()
			path := filepath.Join(t.TempDir(), "staged")
			start := time.Now()
			err := downloadRetrying(t.Context(), server.Client(), server.URL, "token", path, asset)
			if (err == nil) != tc.ok || requests.Load() != tc.requests {
				t.Fatalf("err=%v requests=%d", err, requests.Load())
			}
			if tc.ok && Verify(path, asset) != nil {
				t.Fatal("retried download was not verified")
			}
			if waited := time.Since(start); waited > 10*time.Second {
				t.Fatal("retry took too long", waited)
			}
		})
	}
	t.Run("checksum mismatch", func(t *testing.T) {
		var requests atomic.Int32
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requests.Add(1)
			_, _ = w.Write([]byte("different bytes of equal size!!"[:len(data)]))
		}))
		defer server.Close()
		if err := downloadRetrying(t.Context(), server.Client(), server.URL, "", filepath.Join(t.TempDir(), "staged"), asset); err == nil || requests.Load() != 1 {
			t.Fatal("checksum failure was retried or accepted", err, requests.Load())
		}
	})
}
