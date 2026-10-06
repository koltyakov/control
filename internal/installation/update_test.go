package installation

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gofrs/flock"
	"github.com/koltyakov/control/internal/update"
)

func TestUpdateSelf(t *testing.T) {
	// A real executable verifies replacement while the old process is running,
	// including Windows' executable locking behavior on native Windows CI.
	dir := t.TempDir()
	source := filepath.Join(dir, "main.go")
	const program = `package main
import ("crypto/sha256"; "encoding/json"; "fmt"; "io"; "os"; "runtime")
func main() {
 if len(os.Args) > 1 && os.Args[1] == "version" {
  path, _ := os.Executable(); f, _ := os.Open(path); defer f.Close()
  h := sha256.New(); io.Copy(h, f)
  json.NewEncoder(os.Stdout).Encode(map[string]string{"version":"v2", "os":runtime.GOOS, "arch":runtime.GOARCH, "sha256":fmt.Sprintf("%x",h.Sum(nil))})
  return
 }
 fmt.Println("ready"); io.Copy(io.Discard, os.Stdin)
}
`
	if err := os.WriteFile(source, []byte(program), 0600); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(dir, update.AssetName(runtime.GOOS, runtime.GOARCH))
	cmd := exec.CommandContext(t.Context(), "go", "build", "-o", binary, source)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build fixture: %v\n%s", err, out)
	}
	data, err := os.ReadFile(binary)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	asset := update.Asset{OS: runtime.GOOS, Arch: runtime.GOARCH, File: filepath.Base(binary), Size: int64(len(data)), SHA256: hex.EncodeToString(sum[:])}
	for _, tc := range []struct {
		name, version, requested, wantError string
		corrupt, missing, cancelled         bool
		unchanged                           bool
	}{
		{name: "latest", version: "v2"},
		{name: "pinned", version: "v2", requested: "v2"},
		{name: "same version different checksum", version: "v2", unchanged: true},
		{name: "same pinned version different checksum", version: "v2", requested: "v2", unchanged: true},
		{name: "corrupt", version: "v2", corrupt: true, wantError: "checksum mismatch"},
		{name: "wrong executable version", version: "v3", wantError: "does not match"},
		{name: "wrong release tag", version: "v2", requested: "v3", wantError: "tag and manifest"},
		{name: "missing release", missing: true, wantError: "404"},
		{name: "cancelled", cancelled: true, wantError: "context canceled"},
		{name: "invalid tag", requested: "../v2", wantError: "invalid release tag"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), filepath.Base(binary))
			// Trailing bytes make the old executable differ without preventing execution.
			old := append(append([]byte(nil), data...), []byte("old build")...)
			if err := os.WriteFile(path, old, 0755); err != nil {
				t.Fatal(err)
			}
			process := exec.CommandContext(t.Context(), path)
			stdin, err := process.StdinPipe()
			if err != nil {
				t.Fatal(err)
			}
			stdout, err := process.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			if err = process.Start(); err != nil {
				t.Fatal(err)
			}
			defer func() { _ = stdin.Close(); _ = process.Wait() }()
			ready := make([]byte, len("ready\n"))
			if _, err = io.ReadFull(stdout, ready); err != nil {
				t.Fatal(err)
			}
			var downloads atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tc.missing {
					http.NotFound(w, r)
					return
				}
				base := "/latest/download"
				if tc.requested != "" {
					base = "/download/" + tc.requested
				}
				switch r.URL.Path {
				case base + "/control-manifest.json":
					_ = json.NewEncoder(w).Encode(update.Manifest{Version: tc.version, Assets: []update.Asset{asset}})
				case "/download/" + tc.version + "/" + asset.File:
					downloads.Add(1)
					if tc.corrupt {
						_, _ = w.Write([]byte("bad binary"))
					} else {
						_, _ = w.Write(data)
					}
				default:
					t.Errorf("unexpected request: %s", r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if tc.cancelled {
				cancel()
			}
			currentVersion := "v1"
			if tc.unchanged {
				currentVersion = "v2"
			}
			result, err := updateSelf(ctx, path, server.URL, tc.requested, currentVersion, server.Client())
			if tc.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantError) {
					t.Fatalf("got %v, want %s", err, tc.wantError)
				}
				got, readErr := os.ReadFile(path)
				if readErr != nil || string(got) != string(old) {
					t.Fatal("failed update changed original executable", readErr)
				}
				return
			}
			if tc.unchanged {
				if err != nil || result.Changed || result.Version != "v2" || downloads.Load() != 0 {
					t.Fatalf("same-version update: %+v, %v; downloads=%d", result, err, downloads.Load())
				}
				got, readErr := os.ReadFile(path)
				if readErr != nil || string(got) != string(old) {
					t.Fatal("same-version update replaced executable", readErr)
				}
				return
			}
			if err != nil || !result.Changed || result.Version != "v2" || result.Path != path {
				t.Fatalf("update: %+v, %v", result, err)
			}
			if err = update.ValidateExecutable(ctx, path, "v2", asset); err != nil {
				t.Fatal(err)
			}
			result, err = updateSelf(ctx, path, server.URL, tc.requested, "v2", server.Client())
			if err != nil || result.Changed || downloads.Load() != 1 {
				t.Fatalf("repeat update: %+v, %v; downloads=%d", result, err, downloads.Load())
			}
		})
	}
}

func TestUpdateSelfRejectsRepository(t *testing.T) {
	for _, repo := range []string{"", "../repo", "owner/repo/extra", "https://example.com/repo"} {
		if _, err := UpdateSelf(t.Context(), repo, ""); err == nil {
			t.Fatalf("accepted repository %q", repo)
		}
	}
}

func TestUpdateSelfRejectsConcurrentUpdate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "control")
	lock := flock.New(path + ".update.lock")
	if err := lock.Lock(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lock.Close() }()
	_, err := updateSelf(t.Context(), path, "http://unused.invalid", "", "v1", http.DefaultClient)
	if err == nil || !strings.Contains(err.Error(), "another CLI update") {
		t.Fatalf("concurrent update: %v", err)
	}
}
