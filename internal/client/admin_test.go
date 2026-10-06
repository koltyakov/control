package client

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/koltyakov/control/internal/update"
)

func TestAdminBinaryRequestStreamsBeforeBodyCompletes(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	const size = 16 << 20
	firstRead := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Content-Type") != "application/octet-stream" || r.ContentLength != size {
			t.Error("binary request lost its content type or exact length")
		}
		if _, err := io.CopyN(io.Discard, r.Body, 32<<10); err != nil {
			t.Error(err)
			return
		}
		close(firstRead)
		n, err := io.Copy(io.Discard, r.Body)
		if err != nil || n != size-(32<<10) {
			t.Errorf("remaining streamed bytes = %d, error = %v", n, err)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	reader, writer := io.Pipe()
	defer func() { _ = reader.Close() }()
	defer func() { _ = writer.Close() }()
	writerDone := make(chan error, 1)
	go func() {
		chunk := make([]byte, 32<<10)
		_, err := writer.Write(chunk)
		if err == nil {
			select {
			case <-firstRead:
			case <-ctx.Done():
				err = ctx.Err()
			}
		}
		for written := len(chunk); err == nil && written < size; written += len(chunk) {
			_, err = writer.Write(chunk)
		}
		_ = writer.CloseWithError(err)
		writerDone <- err
	}()
	err := (Admin{URL: server.URL}).request(ctx, http.MethodPut, "/binary", reader, size, "application/octet-stream", nil)
	_ = reader.Close()
	if writerErr := <-writerDone; writerErr != nil {
		t.Fatal(writerErr)
	}
	if err != nil {
		t.Fatal(err)
	}
}

func TestAdminPushStreamsBinaryAndPublishesJSONOnlyAfterUpload(t *testing.T) {
	dir := t.TempDir()
	file, err := os.Create(filepath.Join(dir, "control_linux_amd64"))
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.New()
	chunk := bytes.Repeat([]byte{0, 255, 1, 254}, 8<<10)
	const size = 16 << 20
	for written := 0; written < size; written += len(chunk) {
		if _, err := io.MultiWriter(file, hash).Write(chunk); err != nil {
			_ = file.Close()
			t.Fatal(err)
		}
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	asset := update.Asset{OS: "linux", Arch: "amd64", File: "control_linux_amd64", Size: size, SHA256: hex.EncodeToString(hash.Sum(nil))}
	manifest := update.Manifest{Version: "dev-stream-test", CreatedAt: time.Now().UTC(), Assets: []update.Asset{asset}}
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "control-manifest.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	for _, reject := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "rejected-upload"}[reject], func(t *testing.T) {
			var uploaded, published atomic.Bool
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer test-operator" {
					t.Error("missing upload or publication authorization")
				}
				switch r.Method + " " + r.URL.Path {
				case "PUT /v1/admin/updates/blobs/" + asset.SHA256:
					if r.Header.Get("Content-Type") != "application/octet-stream" || r.ContentLength != size {
						t.Error("binary request lost its content type or exact length")
					}
					if reject {
						http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
						return
					}
					received := sha256.New()
					n, err := io.Copy(received, r.Body)
					if err != nil || n != size || hex.EncodeToString(received.Sum(nil)) != asset.SHA256 {
						t.Errorf("invalid streamed upload: bytes=%d, error=%v", n, err)
					}
					uploaded.Store(true)
					w.WriteHeader(http.StatusNoContent)
				case "POST /v1/admin/updates":
					published.Store(true)
					if !uploaded.Load() || r.Header.Get("Content-Type") != "application/json" {
						t.Error("manifest published before upload or with non-JSON content type")
					}
					var received update.Manifest
					if err := json.NewDecoder(r.Body).Decode(&received); err != nil || received.Version != manifest.Version || len(received.Assets) != 1 || received.Assets[0] != asset {
						t.Error("manifest changed during publication", err)
					}
					w.WriteHeader(http.StatusAccepted)
					_ = json.NewEncoder(w).Encode(update.Deployment{ID: "streamed", Manifest: received})
				default:
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			deployment, err := (Admin{URL: server.URL, Key: "test-operator"}).Push(t.Context(), dir)
			if reject {
				if err == nil || !strings.Contains(err.Error(), "413") || published.Load() {
					t.Fatalf("rejected upload published or lost error: %v", err)
				}
			} else if err != nil || deployment.ID != "streamed" || !published.Load() {
				t.Fatalf("streamed push failed: %v", err)
			}
		})
	}
}
