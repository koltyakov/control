package update

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/koltyakov/control/internal/buildinfo"
	"github.com/koltyakov/control/internal/workgate"
)

func TestUpdaterChecksSelectionAtStartupAndStagesWithoutApplying(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	stat, err := os.Stat(executable)
	if err != nil {
		t.Fatal(err)
	}
	info := buildinfo.Current()
	asset := Asset{OS: info.OS, Arch: info.Arch, File: AssetName(info.OS, info.Arch), Size: stat.Size(), SHA256: info.SHA256}
	manifest := Manifest{Version: info.Version, Assets: []Asset{asset}}
	selection := Selection{ID: manifest.ID(), Manifest: manifest}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer node-credential" {
			t.Error("startup check or download omitted node authentication")
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/v1/updates":
			_ = json.NewEncoder(w).Encode(selection)
		case "/v1/updates/blobs/" + asset.SHA256:
			http.ServeFile(w, r, executable)
		default:
			t.Error("unexpected startup request", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	gate := workgate.New()
	active, err := gate.Enter(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer active()
	old := info
	old.Version, old.SHA256 = "older-node", strings.Repeat("0", 64)
	reports := make(chan Status, 16)
	var applied int
	a, err := NewUpdater(UpdaterOptions{Gateway: server.URL, Token: "node-credential", Dir: t.TempDir(), Software: old,
		Busy: func() bool { n, _ := gate.State(); return n > 0 }, Pause: gate.PauseIfIdle, Resume: gate.Resume,
		Report: func(_ context.Context, s Status) error { reports <- s; return nil },
		Apply:  func(path string) error { applied++; return Verify(path, asset) },
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); a.Run(ctx) }()
	defer func() { cancel(); <-done }()
	for {
		select {
		case <-ctx.Done():
			t.Fatal("starting node never staged the current selected update")
		case s := <-reports:
			if s.State != "staged" {
				continue
			}
			cancel()
			<-done
			if s.ID != selection.ID || s.Paused || applied != 0 || !s.Busy {
				t.Fatal("startup check bypassed work admission or applied before gateway commit", s)
			}
			active()
			command := Command{ID: selection.ID, LeaseUntil: time.Now().Add(time.Minute)}
			a.process(t.Context(), queuedCommand{"update.commit", command})
			if applied != 0 {
				t.Fatal("catch-up bypassed an idle reservation")
			}
			a.process(t.Context(), queuedCommand{"update.prepare", command})
			a.process(t.Context(), queuedCommand{"update.commit", command})
			if applied != 1 {
				t.Fatal("starting node did not apply the committed selected update")
			}
			return
		}
	}
}

func TestStartupSelectionCompatibilityAndValidation(t *testing.T) {
	manifest := Manifest{Version: "v1", Assets: []Asset{{OS: "linux", Arch: "amd64", File: AssetName("linux", "amd64"), Size: 1, SHA256: strings.Repeat("a", 64)}}}
	for _, tc := range []struct {
		name, body string
		status     int
		wantError  bool
	}{
		{"older-gateway", "not found", 404, false},
		{"no-selection", `{}`, 200, false},
		{"malformed", `{`, 200, true},
		{"bad-id", `{"id":"incorrect","manifest":` + string(mustJSON(t, manifest)) + `}`, 200, true},
		{"wrong-platform", string(mustJSON(t, Selection{ID: manifest.ID(), Manifest: manifest})), 200, true},
		{"unauthorized", "unauthorized", 401, true},
		{"redirect", "", 302, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Location", "https://untrusted.invalid/selection")
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()
			a := &Updater{options: UpdaterOptions{Gateway: server.URL, Software: buildinfo.Info{OS: "windows", Arch: "arm64"}}}
			if err := a.checkSelection(t.Context()); (err != nil) != tc.wantError {
				t.Fatal("unexpected selection validation result", err)
			}
		})
	}
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
