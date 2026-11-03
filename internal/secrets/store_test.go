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
	"time"
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
	entries, err := s.List()
	if err != nil || len(entries) != 4 {
		t.Fatalf("lost metadata: %v", err)
	}
	for _, entry := range entries {
		if entry.CreatedAt.IsZero() || !entry.CreatedAt.Equal(entry.UpdatedAt) {
			t.Fatalf("missing timestamps for %s", entry.Name)
		}
	}
}

func TestStoreTimestamps(t *testing.T) {
	t.Parallel()
	s := Store{Dir: filepath.Join(t.TempDir(), "secrets"), WorkDir: t.TempDir()}
	ctx := context.Background()
	before := time.Now().UTC()
	if err := s.Set(ctx, "api", "private-original"); err != nil {
		t.Fatal(err)
	}
	entries, err := s.List()
	if err != nil || len(entries) != 1 {
		t.Fatalf("created metadata: %v", err)
	}
	created := entries[0]
	if created.Name != "api" || created.CreatedAt.Before(before) || created.CreatedAt.After(time.Now()) ||
		!created.CreatedAt.Equal(created.UpdatedAt) || created.CreatedAt.Location() != time.UTC {
		t.Fatalf("invalid creation timestamps: %+v", created)
	}
	for _, value := range []string{"private-replacement", "private-replacement"} {
		before = time.Now().UTC()
		if err := s.Set(ctx, "api", value); err != nil {
			t.Fatal(err)
		}
		// Reopen to verify that both timestamps survive persistence, including
		// an explicit set that supplies the same value.
		reopened := Store{Dir: s.Dir, WorkDir: s.WorkDir}
		entries, err = reopened.List()
		if err != nil || len(entries) != 1 {
			t.Fatalf("updated metadata: %v", err)
		}
		got := entries[0]
		if !got.CreatedAt.Equal(created.CreatedAt) || got.UpdatedAt.Before(before) || got.UpdatedAt.After(time.Now()) || got.UpdatedAt.Location() != time.UTC {
			t.Fatalf("invalid update timestamps: %+v", got)
		}
		values, err := reopened.Snapshot()
		if err != nil || values["api"] != value {
			t.Fatalf("updated value unavailable for injection: %v", err)
		}
		data, err := json.Marshal(entries)
		if err != nil || strings.Contains(string(data), "private-") || strings.Contains(string(data), `"value"`) {
			t.Fatal("metadata exposes a value or cannot be encoded")
		}
	}
	if err := s.Delete(ctx, "api"); err != nil {
		t.Fatal(err)
	}
	entries, err = s.List()
	if err != nil || entries == nil || len(entries) != 0 {
		t.Fatalf("deleted metadata retained: %v", err)
	}
	before = time.Now().UTC()
	if err := s.Set(ctx, "api", "recreated"); err != nil {
		t.Fatal(err)
	}
	entries, err = s.List()
	if err != nil || len(entries) != 1 || entries[0].CreatedAt.Before(before) || !entries[0].CreatedAt.Equal(entries[0].UpdatedAt) {
		t.Fatalf("recreated secret did not start new metadata: %v", err)
	}
}

func TestLegacyStoreMetadata(t *testing.T) {
	t.Parallel()
	s := Store{Dir: t.TempDir(), WorkDir: t.TempDir()}
	path := filepath.Join(s.Dir, "values.json")
	legacy := `{"old":"private-legacy","untouched":"private-untouched"}`
	if err := os.WriteFile(path, []byte(legacy), 0600); err != nil {
		t.Fatal(err)
	}
	entries, err := s.List()
	if err != nil || !reflect.DeepEqual(entries, []Metadata{{Name: "old"}, {Name: "untouched"}}) {
		t.Fatalf("legacy timestamps must be unknown: %v, %v", entries, err)
	}
	values, err := s.Snapshot()
	if err != nil || values["old"] != "private-legacy" || values["untouched"] != "private-untouched" {
		t.Fatalf("legacy injection unavailable: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != legacy {
		t.Fatal("read rewrote legacy storage")
	}
	if err := s.Set(context.Background(), "new", "private-new"); err != nil {
		t.Fatal(err)
	}
	entries, err = s.List()
	if err != nil || len(entries) != 3 || entries[0].CreatedAt.IsZero() ||
		!reflect.DeepEqual(entries[1:], []Metadata{{Name: "old"}, {Name: "untouched"}}) {
		t.Fatalf("unrelated set invented legacy dates: %v, %v", entries, err)
	}
	before := time.Now().UTC()
	if err := s.Set(context.Background(), "old", "private-rotated"); err != nil {
		t.Fatal(err)
	}
	entries, err = s.List()
	if err != nil || len(entries) != 3 || !entries[1].CreatedAt.IsZero() || entries[1].UpdatedAt.Before(before) ||
		entries[1].UpdatedAt.After(time.Now()) || entries[2] != (Metadata{Name: "untouched"}) {
		t.Fatalf("legacy update did not preserve unknown dates: %v, %v", entries, err)
	}
	updated := entries[1]
	values, err = s.Snapshot()
	if err != nil || len(values) != 3 || values["old"] != "private-rotated" || values["untouched"] != "private-untouched" || values["new"] != "private-new" {
		t.Fatalf("migration lost secret values: %v", err)
	}
	if err := s.Delete(context.Background(), "new"); err != nil {
		t.Fatal(err)
	}
	entries, err = s.List()
	if err != nil || !reflect.DeepEqual(entries, []Metadata{updated, {Name: "untouched"}}) {
		t.Fatalf("unrelated delete changed metadata: %v", err)
	}
}

func TestFailedChangesPreserveMetadata(t *testing.T) {
	t.Parallel()
	s := Store{Dir: t.TempDir(), WorkDir: t.TempDir()}
	if err := s.Set(context.Background(), "api", "original"); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(s.Dir, "values.json")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, change := range []func() error{
		func() error { return s.Set(ctx, "api", "replacement") },
		func() error { return s.Delete(ctx, "api") },
		func() error { return s.Set(context.Background(), "api", "") },
	} {
		if err := change(); err == nil {
			t.Fatal("failed change accepted")
		}
		got, err := os.ReadFile(path)
		if err != nil || string(got) != string(before) {
			t.Fatal("failed change modified the value or timestamps")
		}
	}
}

func TestInvalidRecordMetadata(t *testing.T) {
	t.Parallel()
	for _, data := range []string{
		`{"api":{"value":"private-value","createdAt":"private-invalid-date"}}`,
		`{"api":{"value":"private-value","updatedAt":123}}`,
		`{"api":{"value":123}}`,
		`{"api":{}}`,
		`{"api":null}`,
	} {
		s := Store{Dir: t.TempDir(), WorkDir: t.TempDir()}
		path := filepath.Join(s.Dir, "values.json")
		if err := os.WriteFile(path, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		for _, read := range []func() error{
			func() error { _, err := s.Snapshot(); return err },
			func() error { _, err := s.List(); return err },
			func() error { _, err := s.Names(); return err },
			func() error { return s.Set(context.Background(), "other", "replacement") },
			func() error { return s.Delete(context.Background(), "api") },
		} {
			if err := read(); err == nil || err.Error() != "invalid secret storage" {
				t.Fatal("invalid record must fail without value or timestamp diagnostics")
			}
		}
		got, err := os.ReadFile(path)
		if err != nil || string(got) != data {
			t.Fatal("invalid record was overwritten")
		}
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
