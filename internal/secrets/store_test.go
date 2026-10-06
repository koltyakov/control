package secrets

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
)

func TestStore(t *testing.T) {
	s := Store{Dir: filepath.Join(t.TempDir(), "secrets"), WorkDir: t.TempDir()}
	ctx := context.Background()
	if names, err := s.Names(); err != nil || len(names) != 0 {
		t.Fatalf("empty store: %v, %v", names, err)
	}
	if err := s.Set(ctx, "app.password", "  private 世界  "); err != nil {
		t.Fatal(err)
	}
	if err := s.Set(ctx, "api", "token"); err != nil {
		t.Fatal(err)
	}
	names, err := s.Names()
	if err != nil || !reflect.DeepEqual(names, []string{"api", "app.password"}) {
		t.Fatalf("names: %v, %v", names, err)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(filepath.Join(s.Dir, "values.json"))
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatalf("private permissions: %v, %v", info, err)
		}
	}
	for _, value := range []string{"", strings.Repeat("a", MaxValue+1), "nul\x00", string([]byte{255})} {
		if err := s.Set(ctx, "bad", value); err == nil {
			t.Fatal("accepted invalid secret")
		}
	}
	if err := s.Set(ctx, "../escape", "token"); err == nil {
		t.Fatal("accepted invalid name")
	}
	if err := s.Set(ctx, "api", "replacement"); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(ctx, "app.password"); err != nil {
		t.Fatal(err)
	}
	values, err := s.Snapshot()
	if err != nil || len(values) != 1 || values["api"] != "replacement" {
		t.Fatalf("snapshot: %v", err)
	}
	if err := os.WriteFile(filepath.Join(s.Dir, "values.json"), []byte(`{"password":"sensitive" broken}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Snapshot(); err == nil || strings.Contains(err.Error(), "sensitive") {
		t.Fatal("corrupt store must fail without value diagnostics")
	}
	if err := os.WriteFile(filepath.Join(s.Dir, "values.json"), []byte(`null`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := s.Set(ctx, "api", "replacement"); err == nil {
		t.Fatal("null storage must fail closed")
	}
}

func TestConcurrentChanges(t *testing.T) {
	s := Store{Dir: filepath.Join(t.TempDir(), "secrets"), WorkDir: t.TempDir()}
	var wg sync.WaitGroup
	for _, name := range []string{"one", "two", "three", "four"} {
		wg.Go(func() {
			if err := s.Set(context.Background(), name, "value"); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	names, err := s.Names()
	if err != nil || len(names) != 4 {
		t.Fatalf("lost changes: %v, %v", names, err)
	}
}

func TestWorkspaceRejected(t *testing.T) {
	work := t.TempDir()
	s := Store{Dir: filepath.Join(work, "state", "secrets"), WorkDir: work}
	if err := s.Set(context.Background(), "password", "sensitive"); err == nil {
		t.Fatal("stored credentials in file API root")
	}
	if _, err := os.Stat(filepath.Join(s.Dir, "values.json")); !os.IsNotExist(err) {
		t.Fatal("wrote rejected value")
	}
	link := filepath.Join(t.TempDir(), "workspace-link")
	if err := os.Symlink(work, link); err == nil {
		s.Dir = filepath.Join(link, "state", "secrets")
		if err := s.Set(context.Background(), "password", "sensitive"); err == nil {
			t.Fatal("stored credentials through a workspace symlink")
		}
	}
}

func TestRedaction(t *testing.T) {
	value := "pass\"word\n世界"
	r := NewRedactor(map[string]string{"password": value, "short": "x"})
	quoted, _ := json.Marshal(value)
	for _, text := range []string{value, string(quoted[1 : len(quoted)-1]), base64.StdEncoding.EncodeToString([]byte(value))} {
		if got := r.Text("before " + text + " after"); strings.Contains(got, text) || !strings.Contains(got, "[REDACTED]") {
			t.Fatal("secret not masked")
		}
	}
	clean, err := r.JSON([]byte(`{"value":"x","number":9007199254740993}`))
	if err != nil || !json.Valid(clean) || !strings.Contains(string(clean), "9007199254740993") || !strings.Contains(string(clean), "[REDACTED]") {
		t.Fatalf("invalid redacted JSON: %s, %v", clean, err)
	}
}
