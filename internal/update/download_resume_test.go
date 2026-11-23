package update

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"
)

type resumeFixture struct {
	mu     sync.Mutex
	ranges []string
}

func (f *resumeFixture) record(r *http.Request) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ranges = append(f.ranges, r.Header.Get("Range"))
	return len(f.ranges)
}

func (f *resumeFixture) seen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.ranges...)
}

// cut sends prefix as the start of a full-length response, then drops the
// connection, as a slow link interrupted mid-body would.
func cut(t *testing.T, w http.ResponseWriter, prefix []byte, size int) {
	w.Header().Set("Content-Length", strconv.Itoa(size))
	_, _ = w.Write(prefix)
	w.(http.Flusher).Flush()
	conn, _, err := http.NewResponseController(w).Hijack()
	if err != nil {
		t.Error(err)
		return
	}
	_ = conn.Close()
}

func TestGatewayDownloadResumesInterruptedBody(t *testing.T) {
	data := bytes.Repeat([]byte("resumable executable bytes "), 4096)
	sum := sha256.Sum256(data)
	asset := Asset{OS: "linux", Arch: "amd64", File: AssetName("linux", "amd64"), Size: int64(len(data)), SHA256: hex.EncodeToString(sum[:])}
	half := len(data) / 2
	t.Run("resume", func(t *testing.T) {
		var fixture resumeFixture
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if fixture.record(r) == 1 {
				cut(t, w, data[:half], len(data))
				return
			}
			http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(data))
		}))
		defer server.Close()
		path := filepath.Join(t.TempDir(), "staged")
		if err := downloadRetrying(t.Context(), server.Client(), server.URL, "", path, asset); err != nil {
			t.Fatal(err)
		}
		if ranges := fixture.seen(); len(ranges) != 2 || ranges[1] != "bytes="+strconv.Itoa(half)+"-" {
			t.Fatal("interrupted download restarted instead of resuming", ranges)
		}
		if Verify(path, asset) != nil {
			t.Fatal("resumed download was not verified")
		}
	})
	t.Run("corrupt prefix refetches once", func(t *testing.T) {
		var fixture resumeFixture
		wrong := bytes.Repeat([]byte{'x'}, half)
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if fixture.record(r) == 1 {
				cut(t, w, wrong, len(data))
				return
			}
			http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(data))
		}))
		defer server.Close()
		path := filepath.Join(t.TempDir(), "staged")
		if err := downloadRetrying(t.Context(), server.Client(), server.URL, "", path, asset); err != nil {
			t.Fatal(err)
		}
		if ranges := fixture.seen(); len(ranges) != 3 || ranges[2] != "" {
			t.Fatal("resumed checksum mismatch did not refetch from offset zero", ranges)
		}
		if Verify(path, asset) != nil {
			t.Fatal("refetched download was not verified")
		}
	})
	t.Run("stalled body", func(t *testing.T) {
		idle := downloadIdleTimeout
		downloadIdleTimeout = 100 * time.Millisecond
		defer func() { downloadIdleTimeout = idle }()
		var fixture resumeFixture
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if fixture.record(r) == 1 {
				w.Header().Set("Content-Length", strconv.Itoa(len(data)))
				_, _ = w.Write(data[:half])
				w.(http.Flusher).Flush()
				<-r.Context().Done()
				return
			}
			http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(data))
		}))
		defer server.Close()
		path := filepath.Join(t.TempDir(), "staged")
		// No whole-request timeout applies; a client timeout is ignored too.
		client := server.Client()
		client.Timeout = time.Nanosecond
		if err := downloadRetrying(t.Context(), client, server.URL, "", path, asset); err != nil {
			t.Fatal(err)
		}
		if ranges := fixture.seen(); len(ranges) != 2 || ranges[1] != "bytes="+strconv.Itoa(half)+"-" {
			t.Fatal("stalled download did not resume", ranges)
		}
	})
}
