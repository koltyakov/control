package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/koltyakov/control/internal/client"
	"github.com/koltyakov/control/internal/store"
	"github.com/koltyakov/control/internal/update"
)

func captureUpdatePushOutput(t *testing.T, run func() error) (string, error) {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "stdout")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	previous := os.Stdout
	os.Stdout = f
	defer func() { os.Stdout = previous }()
	runErr := run()
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	output, err := io.ReadAll(f)
	if err != nil {
		t.Fatal(err)
	}
	return string(output), runErr
}

func TestUpdatePushSummaryAndJSON(t *testing.T) {
	dir := t.TempDir()
	binary := []byte("local development build")
	asset := update.Asset{OS: "linux", Arch: "amd64", File: update.AssetName("linux", "amd64"), Size: int64(len(binary)), SHA256: fmt.Sprintf("%x", sha256.Sum256(binary))}
	manifest := update.Manifest{Version: "dev-local", CreatedAt: time.Now().UTC(), Assets: []update.Asset{asset}}
	if err := os.WriteFile(filepath.Join(dir, asset.File), binary, 0600); err != nil {
		t.Fatal(err)
	}
	if err := store.Write(filepath.Join(dir, "control-manifest.json"), manifest); err != nil {
		t.Fatal(err)
	}
	// A same-version push can return an older manifest and its existing phase.
	retained := asset
	retained.SHA256 = strings.Repeat("a", 64)
	deployment := update.Deployment{
		ID: "retained-deployment", Manifest: update.Manifest{Version: manifest.Version, CreatedAt: manifest.CreatedAt.Add(-time.Hour), Assets: []update.Asset{retained}},
		Source: "development", Phase: "staging", UpdatedAt: manifest.CreatedAt, Participants: []string{"worker"},
	}
	uploads, publications := 0, 0
	reject := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/auth":
			_, _ = io.WriteString(w, `{"role":"superuser"}`)
		case r.Method == http.MethodPut && r.URL.Path == "/v1/admin/updates/blobs/"+asset.SHA256:
			uploads++
			if reject {
				http.Error(w, "upload rejected", http.StatusRequestEntityTooLarge)
			}
		case r.Method == http.MethodPost && r.URL.Path == "/v1/admin/updates":
			publications++
			_ = json.NewEncoder(w).Encode(deployment)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	c := client.Admin{URL: server.URL, Key: "superuser-key"}

	for _, phase := range []string{"staging", "waiting-idle", "complete", "failed"} {
		t.Run(phase, func(t *testing.T) {
			deployment.Phase, deployment.Error = phase, ""
			if phase == "failed" {
				deployment.Error = "worker staging failed"
			}
			output, err := captureUpdatePushOutput(t, func() error {
				return adminCLI(t.Context(), c, []string{"update", "push", dir})
			})
			want := fmt.Sprintf("Control dev-local accepted by gateway.\nDeployment retained-deployment, phase %s, 1-platform bundle.\n", phase)
			if deployment.Error != "" {
				want += "Rollout error: " + deployment.Error + "\n"
			}
			want += "Follow rollout with: control update status\n"
			if err != nil || output != want {
				t.Fatalf("push output = %q, error = %v; want %q", output, err, want)
			}
		})
	}
	for _, args := range [][]string{{dir, "--json"}, {"--json", dir}} {
		output, err := captureUpdatePushOutput(t, func() error {
			return adminCLI(t.Context(), c, append([]string{"update", "push"}, args...))
		})
		var got update.Deployment
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal([]byte(output), &got); err != nil || !reflect.DeepEqual(got, deployment) {
			t.Fatalf("full deployment JSON = %q, error = %v", output, err)
		}
	}
	if uploads != 6 || publications != 6 {
		t.Fatalf("uploads = %d, publications = %d; want 6 each", uploads, publications)
	}
	for _, args := range [][]string{nil, {"--json"}, {dir, "extra"}, {dir, "--unknown"}, {"--json", dir, "extra"}} {
		output, err := captureUpdatePushOutput(t, func() error {
			return adminCLI(t.Context(), c, append([]string{"update", "push"}, args...))
		})
		if err == nil || output != "" || uploads != 6 || publications != 6 {
			t.Fatalf("invalid arguments %v: output %q, error %v, uploads %d, publications %d", args, output, err, uploads, publications)
		}
	}
	if err := adminCLI(t.Context(), c, []string{"update", "push", "--help"}); err != nil || uploads != 6 {
		t.Fatalf("push help: %v, uploads %d", err, uploads)
	}
	reject = true
	output, err := captureUpdatePushOutput(t, func() error {
		return adminCLI(t.Context(), c, []string{"update", "push", dir})
	})
	if err == nil || !strings.Contains(err.Error(), "413") || output != "" || uploads != 7 || publications != 6 {
		t.Fatalf("rejected upload: output %q, error %v, uploads %d, publications %d", output, err, uploads, publications)
	}
}
