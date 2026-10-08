package client

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gofrs/flock"
)

func scenarioRenameServer(t *testing.T, old string, status int, during func()) (Admin, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/nodes":
			_ = json.NewEncoder(w).Encode([]map[string]string{{"id": "stable-id", "name": old}})
		case "/v1/fleet/nodes/stable-id":
			var params map[string]string
			if r.Method != http.MethodPatch || json.NewDecoder(r.Body).Decode(&params) != nil || params["name"] == "" {
				t.Error("invalid rename request")
			}
			calls.Add(1)
			if during != nil {
				during()
			}
			w.WriteHeader(status)
		default:
			t.Error("unexpected request", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(s.Close)
	return Admin{URL: s.URL, Key: "account-key"}, &calls
}

func writeScenarioFixture(t *testing.T, p, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestRenameMachineMigratesProjectScenarios(t *testing.T) {
	t.Chdir(t.TempDir())
	old, name := "win.02", "trader"
	root := ".control-scenarios"
	markdown := "# Connect to VPN\nMachine: `win.02`\n`control call win.02 node.describe`\ncontrol task start win.02 @task.json\ncontrol tunnel start win.02 127.0.0.1:80\ncontrol system win.02-extra\nsecret: win.02\n"
	writeScenarioFixture(t, filepath.Join(root, old, "connect-to-vpn.md"), markdown)
	writeScenarioFixture(t, filepath.Join(root, old, "nested", "task.json"), `{"node":"win.02","args":{"target":"win\u002e02","secret":"win.02","text":"win.02"},"machine":"win.02","id":"win.02"}`)
	writeScenarioFixture(t, filepath.Join(root, old, "image.png"), "binary win.02")
	writeScenarioFixture(t, filepath.Join(root, "other", "scenario.md"), "Machine: `win.02`\n")
	c, calls := scenarioRenameServer(t, old, http.StatusNoContent, func() {
		if _, err := os.Stat(filepath.Join(root, name)); !os.IsNotExist(err) {
			t.Error("scenarios migrated before gateway acknowledgment")
		}
	})
	// CLI and dashboard both call this shared method using the stable ID.
	if err := c.RenameMachine(context.Background(), "stable-id", name); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatal("rename was replayed", calls.Load())
	}
	if _, err := os.Stat(filepath.Join(root, old)); !os.IsNotExist(err) {
		t.Fatal("old scenario folder remains", err)
	}
	got, err := os.ReadFile(filepath.Join(root, name, "connect-to-vpn.md"))
	if err != nil || string(got) != strings.ReplaceAll(strings.ReplaceAll(markdown, "`win.02`", "`trader`"), "win.02 ", "trader ") {
		t.Fatal("markdown references", string(got), err)
	}
	got, err = os.ReadFile(filepath.Join(root, name, "nested", "task.json"))
	want := `{"node":"trader","args":{"target":"trader","secret":"win.02","text":"win.02"},"machine":"trader","id":"win.02"}`
	if err != nil || string(got) != want {
		t.Fatal("JSON references or unrelated values changed", string(got), err)
	}
	for _, item := range []struct{ path, want string }{
		{filepath.Join(root, name, "image.png"), "binary win.02"},
		{filepath.Join(root, "other", "scenario.md"), "Machine: `win.02`\n"},
	} {
		got, err := os.ReadFile(item.path)
		if err != nil || string(got) != item.want {
			t.Fatal("unrelated content changed", item.path, err)
		}
	}
	info, err := os.Stat(filepath.Join(root, name, "connect-to-vpn.md"))
	if err != nil || info.Mode().Perm()&0200 == 0 {
		t.Fatal("file mode was not preserved", err)
	}
}

func TestScenarioRenamePreflightAndGatewayFailure(t *testing.T) {
	for _, test := range []struct {
		name   string
		setup  func(*testing.T, string)
		status int
		calls  int32
	}{
		{"destination conflict", func(t *testing.T, root string) {
			writeScenarioFixture(t, filepath.Join(root, "trader", "keep.md"), "keep")
		}, http.StatusNoContent, 0},
		{"malformed JSON", func(t *testing.T, root string) {
			writeScenarioFixture(t, filepath.Join(root, "old", "task.json"), "not JSON")
		}, http.StatusNoContent, 0},
		{"oversized text", func(t *testing.T, root string) {
			writeScenarioFixture(t, filepath.Join(root, "old", "large.md"), strings.Repeat("x", (1<<20)+1))
		}, http.StatusNoContent, 0},
		{"gateway rejection", nil, http.StatusConflict, 1},
		{"gateway failure", nil, http.StatusInternalServerError, 1},
		{"invalid alias", nil, http.StatusNoContent, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "scenarios")
			p := filepath.Join(root, "old", "scenario.md")
			original := "Machine: `old`\ncontrol call old node.describe\n"
			writeScenarioFixture(t, p, original)
			if test.setup != nil {
				test.setup(t, root)
			}
			c, calls := scenarioRenameServer(t, "old", test.status, nil)
			name := "trader"
			if test.name == "invalid alias" {
				name = "../outside"
			}
			if err := c.renameMachine(context.Background(), "stable-id", name, root); err == nil {
				t.Fatal("rename unexpectedly succeeded")
			}
			if calls.Load() != test.calls {
				t.Fatal("unexpected remote mutation count", calls.Load())
			}
			got, err := os.ReadFile(p)
			if err != nil || string(got) != original {
				t.Fatal("original scenario changed", err)
			}
		})
	}
}

func TestScenarioRenameMissingAndUnchanged(t *testing.T) {
	for _, test := range []string{"missing root", "missing source", "same alias"} {
		t.Run(test, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "scenarios")
			name := "trader"
			if test != "missing root" {
				writeScenarioFixture(t, filepath.Join(root, "other", "scenario.md"), "keep")
			}
			if test == "same alias" {
				name = "old"
			}
			c, calls := scenarioRenameServer(t, "old", http.StatusNoContent, nil)
			if err := c.renameMachine(context.Background(), "stable-id", name, root); err != nil || calls.Load() != 1 {
				t.Fatal("rename without migration failed", err, calls.Load())
			}
		})
	}
}

func TestScenarioRenamePreservesConcurrentEdits(t *testing.T) {
	root := filepath.Join(t.TempDir(), "scenarios")
	p := filepath.Join(root, "old", "scenario.md")
	writeScenarioFixture(t, p, "Machine: `old`\n")
	c, calls := scenarioRenameServer(t, "old", http.StatusNoContent, func() {
		writeScenarioFixture(t, p, "user changed this during the request")
	})
	err := c.renameMachine(context.Background(), "stable-id", "trader", root)
	if err == nil || !strings.Contains(err.Error(), "was renamed") || calls.Load() != 1 {
		t.Fatal("missing partial-success error", err, calls.Load())
	}
	got, err := os.ReadFile(filepath.Join(root, "trader", "scenario.md"))
	if err != nil || string(got) != "user changed this during the request" {
		t.Fatal("concurrent edit was overwritten", err)
	}
}

func TestScenarioRenameRejectsSymlinks(t *testing.T) {
	for _, entry := range []string{"root", "source", "nested", "destination", "lock"} {
		t.Run(entry, func(t *testing.T) {
			base := t.TempDir()
			root := filepath.Join(base, "scenarios")
			writeScenarioFixture(t, filepath.Join(root, "old", "scenario.md"), "Machine: `old`\n")
			outside := t.TempDir()
			writeScenarioFixture(t, filepath.Join(outside, "keep.md"), "keep")
			p := map[string]string{"root": root, "source": filepath.Join(root, "old"), "nested": filepath.Join(root, "old", "link"), "destination": filepath.Join(root, "trader"), "lock": filepath.Join(root, ".rename.lock")}[entry]
			if entry == "root" || entry == "source" {
				if err := os.Rename(p, p+"-original"); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Symlink(outside, p); err != nil {
				t.Skip("symlinks unavailable", err)
			}
			c, calls := scenarioRenameServer(t, "old", http.StatusNoContent, nil)
			if err := c.renameMachine(context.Background(), "stable-id", "trader", root); err == nil || calls.Load() != 0 {
				t.Fatal("symlink did not block remote rename", err, calls.Load())
			}
		})
	}
}

func TestScenarioRenameCaseOnly(t *testing.T) {
	root := filepath.Join(t.TempDir(), "scenarios")
	writeScenarioFixture(t, filepath.Join(root, "old", "scenario.md"), "Machine: `old`\n")
	c, calls := scenarioRenameServer(t, "old", http.StatusNoContent, nil)
	if err := c.renameMachine(context.Background(), "stable-id", "OLD", root); err != nil || calls.Load() != 1 {
		t.Fatal("case-only rename failed", err, calls.Load())
	}
	b, err := os.ReadFile(filepath.Join(root, "OLD", "scenario.md"))
	if err != nil || string(b) != "Machine: `OLD`\n" {
		t.Fatal("case-only references were not updated", err)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.IsDir() && entry.Name() != "OLD" {
			t.Fatal("old directory spelling remains", entry.Name())
		}
	}
}

func TestScenarioRenameLockAndCancellation(t *testing.T) {
	root := filepath.Join(t.TempDir(), "scenarios")
	writeScenarioFixture(t, filepath.Join(root, "old", "scenario.md"), "Machine: `old`\n")
	lock := flock.New(filepath.Join(root, ".rename.lock"))
	if err := lock.Lock(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lock.Close() })
	c, calls := scenarioRenameServer(t, "old", http.StatusNoContent, nil)
	if err := c.renameMachine(context.Background(), "stable-id", "trader", root); err == nil || calls.Load() != 0 {
		t.Fatal("contended lock permitted mutation", err, calls.Load())
	}
	if err := lock.Unlock(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := c.renameMachine(ctx, "stable-id", "trader", root); err == nil || calls.Load() != 0 {
		t.Fatal("cancelled migration permitted mutation", err, calls.Load())
	}
}

func TestScenarioRenameUncertainResponse(t *testing.T) {
	root := filepath.Join(t.TempDir(), "scenarios")
	p := filepath.Join(root, "old", "scenario.md")
	writeScenarioFixture(t, p, "Machine: `old`\n")
	var calls atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/nodes" {
			_, _ = w.Write([]byte(`[{"id":"stable-id","name":"old"}]`))
			return
		}
		calls.Add(1)
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		_ = conn.Close()
	}))
	t.Cleanup(s.Close)
	c := Admin{URL: s.URL, Key: "account-key"}
	err := c.renameMachine(context.Background(), "stable-id", "trader", root)
	if err == nil || !strings.Contains(err.Error(), "not confirmed") || calls.Load() != 1 {
		t.Fatal("uncertain rename was not reported once", err, calls.Load())
	}
	b, err := os.ReadFile(p)
	if err != nil || string(b) != "Machine: `old`\n" {
		t.Fatal("uncertain rename changed scenarios", err)
	}
}

func TestScenarioRenameLateConflict(t *testing.T) {
	root := filepath.Join(t.TempDir(), "scenarios")
	writeScenarioFixture(t, filepath.Join(root, "old", "scenario.md"), "Machine: `old`\n")
	c, calls := scenarioRenameServer(t, "old", http.StatusNoContent, func() {
		writeScenarioFixture(t, filepath.Join(root, "trader", "keep.md"), "user file")
	})
	err := c.renameMachine(context.Background(), "stable-id", "trader", root)
	if err == nil || !strings.Contains(err.Error(), "incomplete") || calls.Load() != 1 {
		t.Fatal("late conflict was not reported", err, calls.Load())
	}
	for _, item := range []struct{ path, want string }{
		{filepath.Join(root, "old", "scenario.md"), "Machine: `old`\n"},
		{filepath.Join(root, "trader", "keep.md"), "user file"},
	} {
		b, err := os.ReadFile(item.path)
		if err != nil || string(b) != item.want {
			t.Fatal("conflict overwrote a file", item.path, err)
		}
	}
}
