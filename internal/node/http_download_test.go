package node

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/koltyakov/control/internal/model"
)

type downloadResult struct {
	Status   int               `json:"status"`
	Headers  map[string]string `json:"headers"`
	Artifact *model.Artifact   `json:"artifact"`
}

func downloadPayload(size int) []byte {
	b := make([]byte, size)
	for i := range b {
		b[i] = byte(i * 7)
	}
	return b
}

func digest(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// downloadServer serves payload at /file.bin with Content-Length and at
// /chunked without one.
func downloadServer(t *testing.T, payload []byte) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"v1"`)
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("X-Unselected", "ignored")
		if strings.HasPrefix(r.URL.Path, "/chunked") {
			w.(http.Flusher).Flush()
			for chunk := range slices.Chunk(payload, 1000) {
				_, _ = w.Write(chunk)
				w.(http.Flusher).Flush()
			}
			return
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
		_, _ = w.Write(payload)
	}))
	t.Cleanup(server.Close)
	return server
}

func download(t *testing.T, n *Node, ctx context.Context, params map[string]any) (downloadResult, error) {
	t.Helper()
	var result downloadResult
	err := n.Call(ctx, "", "http.download", params, &result)
	return result, err
}

// requireNothingPublished checks that no blob, metadata, staging file or
// budget charge survived a failed download.
func requireNothingPublished(t *testing.T, n *Node) {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(n.Config.DataDir, "artifacts"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	for _, entry := range entries {
		t.Errorf("failed download left %s", entry.Name())
	}
	n.artifactBudgetMu.Lock()
	used, staging := n.artifactUsed, len(n.artifactStaging)
	n.artifactBudgetMu.Unlock()
	if used != 0 || staging != 0 {
		t.Fatalf("failed download kept %d budget bytes and %d staging files", used, staging)
	}
}

func TestHTTPDownloadPublishesArtifact(t *testing.T) {
	t.Parallel()
	payload := downloadPayload(300 << 10)
	server := downloadServer(t, payload)
	for _, c := range []struct {
		name   string
		params map[string]any
		want   string
	}{
		{"without hash", map[string]any{"url": server.URL + "/file.bin"}, "file.bin"},
		{"with hash", map[string]any{"url": server.URL + "/file.bin", "sha256": strings.ToUpper(digest(payload)), "name": "named.bin", "method": "GET"}, "named.bin"},
		{"chunked with hash", map[string]any{"url": server.URL + "/chunked", "sha256": digest(payload), "maxBytes": len(payload)}, "chunked"},
	} {
		t.Run(c.name, func(t *testing.T) {
			n := localNode(t, nil)
			result, err := download(t, n, t.Context(), c.params)
			if err != nil {
				t.Fatal(err)
			}
			a := result.Artifact
			if result.Status != http.StatusOK || a == nil || a.ID != digest(payload) || a.SHA256 != a.ID || a.Size != int64(len(payload)) || a.Name != c.want || a.Node != n.Identity.ID {
				t.Fatalf("unexpected result %+v %+v", result, a)
			}
			if result.Headers["ETag"] != `"v1"` || result.Headers["X-Unselected"] != "" {
				t.Fatalf("headers not selected: %v", result.Headers)
			}
			if raw := model.JSON(result); bytes.Contains(raw, payload[:64]) || len(raw) > 4096 {
				t.Fatalf("result carried body bytes: %d bytes", len(raw))
			}
			stored, err := os.ReadFile(n.artifactPath(a.ID))
			if err != nil || !bytes.Equal(stored, payload) {
				t.Fatalf("stored content differs: %v", err)
			}
			meta, err := n.storedArtifact(a.ID)
			if err != nil || len(meta.Owners) != 1 {
				t.Fatalf("artifact not held by its owner: %+v %v", meta, err)
			}
			entries, _ := os.ReadDir(filepath.Join(n.Config.DataDir, "artifacts"))
			if len(entries) != 2 {
				t.Fatalf("unexpected artifact files: %v", entries)
			}
		})
	}
}

func TestHTTPDownloadRejectsWithoutPublishing(t *testing.T) {
	t.Parallel()
	payload := downloadPayload(64 << 10)
	server := downloadServer(t, payload)
	for _, c := range []struct {
		name   string
		config func(*Config)
		params map[string]any
		want   string
	}{
		{"hash mismatch", nil, map[string]any{"url": server.URL + "/file.bin", "sha256": digest([]byte("other"))}, "sha256 mismatch"},
		{"chunked hash mismatch", nil, map[string]any{"url": server.URL + "/chunked", "sha256": digest([]byte("other"))}, "sha256 mismatch"},
		{"Content-Length over maxBytes", nil, map[string]any{"url": server.URL + "/file.bin", "maxBytes": len(payload) - 1}, "exceeds maxBytes"},
		{"streamed over maxBytes", nil, map[string]any{"url": server.URL + "/chunked", "maxBytes": len(payload) - 1}, "exceeds maxBytes"},
		{"Content-Length over budget", func(c *Config) { c.MaxArtifactBytes = 1024 }, map[string]any{"url": server.URL + "/file.bin"}, "artifact storage budget"},
		{"streamed over budget", func(c *Config) { c.MaxArtifactBytes = 1024 }, map[string]any{"url": server.URL + "/chunked"}, "artifact storage budget"},
		{"Content-Length over reserve", func(c *Config) { c.ArtifactReserveBytes = 1 << 62 }, map[string]any{"url": server.URL + "/file.bin"}, "disk reserve"},
		{"streamed over reserve", func(c *Config) { c.ArtifactReserveBytes = 1 << 62 }, map[string]any{"url": server.URL + "/chunked"}, "disk reserve"},
		{"non-OK status", nil, map[string]any{"url": server.URL + "/missing"}, ""},
		{"file scheme", nil, map[string]any{"url": "file:///etc/passwd"}, "http or https"},
		{"ftp scheme", nil, map[string]any{"url": "ftp://127.0.0.1/file"}, "http or https"},
		{"POST", nil, map[string]any{"url": server.URL + "/file.bin", "method": "POST"}, "only GET"},
		{"invalid hash", nil, map[string]any{"url": server.URL + "/file.bin", "sha256": "abc"}, "sha256"},
		{"path name", nil, map[string]any{"url": server.URL + "/file.bin", "name": "../x"}, "name"},
		{"maxBytes range", nil, map[string]any{"url": server.URL + "/file.bin", "maxBytes": -1}, "maxBytes"},
	} {
		t.Run(c.name, func(t *testing.T) {
			n := localNode(t, c.config)
			if c.name == "non-OK status" {
				// A 404 handler below serves no file.
				missing := httptest.NewServer(http.NotFoundHandler())
				defer missing.Close()
				c.params["url"] = missing.URL
				c.want = "404"
			}
			_, err := download(t, n, t.Context(), c.params)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("got %v, want error containing %q", err, c.want)
			}
			requireNothingPublished(t, n)
		})
	}
}

func TestHTTPDownloadStallAndCancellation(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	started := make(chan struct{}, 4)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/headers" {
			_, _ = w.Write(downloadPayload(64 << 10))
			w.(http.Flusher).Flush()
			started <- struct{}{}
		}
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer server.Close()
	defer close(release)

	for _, path := range []string{"/headers", "/body"} {
		t.Run("stall"+path, func(t *testing.T) {
			n := localNode(t, nil)
			begin := time.Now()
			_, err := download(t, n, t.Context(), map[string]any{"url": server.URL + path, "stallSeconds": 1})
			if err == nil || !strings.Contains(err.Error(), "stalled") || time.Since(begin) > 15*time.Second {
				t.Fatalf("stall not bounded: %v after %v", err, time.Since(begin))
			}
			requireNothingPublished(t, n)
		})
	}
	<-started

	t.Run("cancel", func(t *testing.T) {
		n := localNode(t, nil)
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		go func() {
			_, err := download(t, n, ctx, map[string]any{"url": server.URL + "/body"})
			done <- err
		}()
		<-started
		deadline := time.Now().Add(10 * time.Second)
		for {
			n.artifactBudgetMu.Lock()
			used := n.artifactUsed
			n.artifactBudgetMu.Unlock()
			if used > 0 {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("download never staged bytes")
			}
			time.Sleep(10 * time.Millisecond)
		}
		cancel()
		select {
		case err := <-done:
			if err == nil {
				t.Fatal("cancelled download succeeded")
			}
		case <-time.After(10 * time.Second):
			t.Fatal("cancellation did not stop the download")
		}
		requireNothingPublished(t, n)
	})
}

func TestHTTPDownloadRedirectsAndSecrets(t *testing.T) {
	t.Parallel()
	n := localNode(t, nil)
	const credential = "download-api-token"
	setTestSecret(t, n, "api.token", credential)
	payload := downloadPayload(4096)
	var targetHits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/target":
			targetHits.Add(1)
			_, _ = w.Write(payload)
		case "/redirect":
			http.Redirect(w, r, "/target?token="+credential, http.StatusFound)
		case "/loop":
			http.Redirect(w, r, "/loop", http.StatusFound)
		case "/ftp":
			http.Redirect(w, r, "ftp://127.0.0.1/file", http.StatusFound)
		case "/auth":
			if r.Header.Get("Authorization") != "Bearer "+credential {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			_, _ = w.Write(payload)
		}
	}))
	defer server.Close()
	secretHeaders := map[string]any{"Authorization": map[string]any{"secret": "api.token", "prefix": "Bearer "}}

	result, err := download(t, n, t.Context(), map[string]any{"url": server.URL + "/redirect"})
	if err != nil || result.Artifact == nil || result.Artifact.ID != digest(payload) || targetHits.Load() != 1 {
		t.Fatalf("redirect without secrets not followed: %+v %v", result, err)
	}
	for _, path := range []string{"/loop", "/ftp"} {
		if _, err := download(t, n, t.Context(), map[string]any{"url": server.URL + path}); err == nil {
			t.Fatalf("%s redirect accepted", path)
		}
	}

	result, err = download(t, n, t.Context(), map[string]any{"url": server.URL + "/auth", "headerSecrets": secretHeaders})
	if err != nil || result.Artifact == nil || strings.Contains(string(model.JSON(result)), credential) {
		t.Fatalf("secret-bearing download: %+v %v", result, err)
	}
	for _, extra := range []map[string]any{
		{"headers": map[string]string{"authorization": "literal"}},
		{"headerSecrets": map[string]any{"Authorization": map[string]any{"secret": "missing"}}},
	} {
		params := map[string]any{"url": server.URL + "/auth", "headerSecrets": secretHeaders}
		for k, v := range extra {
			params[k] = v
		}
		if _, err := download(t, n, t.Context(), params); err == nil {
			t.Fatal("accepted invalid secret headers")
		}
	}

	// A secret-bearing redirect is returned, not followed, and its masked
	// Location is visible in the provider result that tasks retain.
	value, err := n.httpDownload(t.Context(), model.JSON(map[string]any{"url": server.URL + "/redirect", "headerSecrets": secretHeaders}), Execution{})
	if err == nil || targetHits.Load() != 1 {
		t.Fatalf("secret-bearing redirect followed: %v", err)
	}
	raw := string(model.JSON(value))
	if !strings.Contains(raw, `"status":302`) || !strings.Contains(raw, "[REDACTED]") || strings.Contains(raw, credential) || strings.Contains(err.Error(), credential) {
		t.Fatalf("redirect result not masked: %s %v", raw, err)
	}

	// Transport errors naming the URL are masked like http.request.
	closed := httptest.NewServer(http.NotFoundHandler())
	closed.Close()
	_, err = download(t, n, t.Context(), map[string]any{"url": closed.URL + "/" + credential, "headerSecrets": secretHeaders})
	if err == nil || strings.Contains(err.Error(), credential) || !strings.Contains(err.Error(), "[REDACTED]") {
		t.Fatalf("error not masked: %v", err)
	}
}

func TestHTTPDownloadTrackedTask(t *testing.T) {
	t.Parallel()
	n := localNode(t, nil)
	payload := downloadPayload(128 << 10)
	server := downloadServer(t, payload)
	ctx := t.Context()
	run := func(id string, args map[string]any) model.Task {
		t.Helper()
		if _, err := n.startTaskContext(ctx, "owner", model.TaskSpec{ID: id, Capability: "http.download", Args: model.JSON(args)}); err != nil {
			t.Fatal(err)
		}
		got, err := n.taskMethod(ctx, "owner", "tasks.get", model.JSON(map[string]any{"id": id, "waitSeconds": 20}))
		if err != nil || !got.(model.Task).Terminal() {
			t.Fatal("task did not finish", got, err)
		}
		return got.(model.Task)
	}
	task := run("download-ok", map[string]any{"url": server.URL + "/file.bin", "sha256": digest(payload)})
	var result downloadResult
	if err := json.Unmarshal(task.Result, &result); err != nil || task.State != "succeeded" || result.Artifact == nil || result.Artifact.ID != digest(payload) {
		t.Fatalf("tracked download: %s %s %v", task.State, task.Result, err)
	}
	meta, err := n.storedArtifact(result.Artifact.ID)
	if err != nil || !slices.Equal(meta.Owners, []string{"owner"}) {
		t.Fatalf("artifact not held by the task owner: %+v %v", meta, err)
	}

	failed := run("download-mismatch", map[string]any{"url": server.URL + "/chunked", "sha256": digest([]byte("other"))})
	result = downloadResult{}
	if err := json.Unmarshal(failed.Result, &result); err != nil || failed.State != "failed" || result.Status != http.StatusOK || result.Artifact != nil || !strings.Contains(failed.Error, "sha256 mismatch") {
		t.Fatalf("failed tracked download: %s %s %s", failed.State, failed.Error, failed.Result)
	}
	entries, _ := os.ReadDir(filepath.Join(n.Config.DataDir, "artifacts"))
	if len(entries) != 2 {
		t.Fatalf("failed task left artifact files: %v", entries)
	}
}
