package update

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/koltyakov/control/internal/buildinfo"
	"github.com/koltyakov/control/internal/workgate"
)

func TestMain(m *testing.M) {
	if len(os.Args) == 3 && os.Args[1] == "version" && os.Args[2] == "--json" {
		_ = json.NewEncoder(os.Stdout).Encode(buildinfo.Current())
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func TestUnverifiedUploadNeverReplacesPublishedBinary(t *testing.T) {
	data := []byte("trusted binary")
	sum := sha256.Sum256(data)
	a := Asset{OS: "linux", Arch: "amd64", File: AssetName("linux", "amd64"), Size: int64(len(data)), SHA256: hex.EncodeToString(sum[:])}
	repo, err := OpenRepository(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err = repo.Upload(bytes.NewReader(data), a); err != nil {
		t.Fatal(err)
	}
	if err = repo.Upload(strings.NewReader("different data"), a); err == nil {
		t.Fatal("accepted corrupt binary")
	}
	if err = Verify(repo.Blob(a.SHA256), a); err != nil {
		t.Fatal("corrupt upload damaged published binary", err)
	}
	m := Manifest{Version: "v1", Assets: []Asset{a}}
	d, err := repo.Publish(m, "test")
	if err != nil {
		t.Fatal(err)
	}
	if err = repo.BeginInstall(d.ID, []string{"worker"}); err != nil {
		t.Fatal(err)
	}
	m.Version = "v2"
	if _, err = repo.Publish(m, "test"); err == nil {
		t.Fatal("superseded an installing update")
	}
	reloaded, err := OpenRepository(repo.dir)
	if err != nil {
		t.Fatal(err)
	}
	if d := reloaded.Current(); d.Phase != "installing" || len(d.Participants) != 1 {
		t.Fatal("lost rollout participants on restart")
	}
}

func TestAgentWaitsForWorkAndRequiresLiveReservation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(executable)
	if err != nil {
		t.Fatal(err)
	}
	stat, err := f.Stat()
	_ = f.Close()
	if err != nil {
		t.Fatal(err)
	}
	info := buildinfo.Current()
	asset := Asset{OS: info.OS, Arch: info.Arch, File: AssetName(info.OS, info.Arch), Size: stat.Size(), SHA256: info.SHA256}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.ServeFile(w, r, executable) }))
	defer server.Close()
	gate := workgate.New()
	active, err := gate.Enter(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer active()
	old := info
	old.SHA256 = strings.Repeat("0", 64)
	applied := 0
	options := AgentOptions{Dir: t.TempDir(), Gateway: server.URL, Software: old, Busy: func() bool { n, _ := gate.State(); return n > 0 }, Pause: gate.PauseIfIdle, Resume: gate.Resume, Report: func(context.Context, Status) error { return nil }, Apply: func(path string) error { applied++; return Verify(path, asset) }}
	a, err := NewAgent(options)
	if err != nil {
		t.Fatal(err)
	}
	command := Command{ID: strings.Repeat("a", 64), Version: info.Version, Asset: asset, LeaseUntil: time.Now().Add(time.Minute)}
	a.process(ctx, agentCommand{"update.offer", command})
	if s := a.Snapshot(); s.State != "staged" {
		t.Fatalf("staging: %+v", s)
	}
	a.process(ctx, agentCommand{"update.prepare", command})
	a.process(ctx, agentCommand{"update.commit", command})
	if applied != 0 || a.Snapshot().Paused {
		t.Fatal("updated while work was active")
	}
	active()
	a.process(ctx, agentCommand{"update.commit", command})
	if applied != 0 {
		t.Fatal("commit without reservation")
	}
	a.process(ctx, agentCommand{"update.prepare", command})
	if !a.Snapshot().Paused {
		t.Fatal("idle reservation missing")
	}
	waitCtx, stop := context.WithTimeout(ctx, 30*time.Millisecond)
	if release, err := gate.Enter(waitCtx); err == nil {
		release()
		t.Fatal("accepted work during reservation")
	}
	stop()
	a.process(ctx, agentCommand{"update.commit", command})
	if applied != 1 {
		t.Fatal("idle committed update did not apply")
	}
	// A restarted process keeps admissions closed until the coordinator resumes.
	gate.Resume()
	options.Software = info
	restarted, err := NewAgent(options)
	if err != nil {
		t.Fatal(err)
	}
	if s := restarted.Snapshot(); !s.Paused || s.State != "applied" {
		t.Fatalf("lost restart reservation: %+v", s)
	}
	restarted.process(ctx, agentCommand{"update.resume", command})
	if _, paused := gate.State(); paused {
		t.Fatal("resume did not release gate")
	}
	if _, err := os.Stat(filepath.Join(options.Dir, "update-maintenance.json")); !os.IsNotExist(err) {
		t.Fatal("reservation marker retained")
	}
	// A truncated candidate must never be executed or applied.
	bad := command
	bad.ID = strings.Repeat("b", 64)
	bad.Asset.Size++
	a.process(ctx, agentCommand{"update.offer", bad})
	if a.Snapshot().State != "error" {
		t.Fatal("accepted truncated update")
	}
}
