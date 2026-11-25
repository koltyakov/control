package secrets

import (
	"context"
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func policyStore(t *testing.T) Store {
	t.Helper()
	return Store{Dir: t.TempDir(), WorkDir: t.TempDir()}
}

func httpRef(t *testing.T, name, raw string) Reference {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return Reference{Name: name, Use: UseHTTP, URL: u}
}

// A store without policies keeps its legacy bytes, has no format marker, and
// resolves every reference for every use exactly as before.
func TestPolicyFreeStoreUnchanged(t *testing.T) {
	t.Parallel()
	s := policyStore(t)
	path := filepath.Join(s.Dir, "values.json")
	legacy := `{"old":"private-legacy"}`
	if err := os.WriteFile(path, []byte(legacy), 0600); err != nil {
		t.Fatal(err)
	}
	r, err := s.Resolver()
	if err != nil {
		t.Fatal(err)
	}
	values, err := r.Resolve([]Reference{{Name: "old", Use: UseExec}, {Name: "old", Use: UseDesktop}, httpRef(t, "old", "http://anywhere.test:9/")})
	if err != nil || values["old"] != "private-legacy" {
		t.Fatalf("unrestricted secret refused: %v", err)
	}
	if err := s.Set(context.Background(), "new", "private-new"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil || strings.Contains(string(data), formatKey) || strings.Contains(string(data), "policy") {
		t.Fatalf("policy-free store gained policy data: %s %v", data, err)
	}
	if entries, err := s.List(); err != nil || entries[0].Policy != nil || entries[0].PolicyInvalid {
		t.Fatalf("legacy metadata changed: %v %v", entries, err)
	}
}

func TestPolicyStoreLifecycle(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := policyStore(t)
	path := filepath.Join(s.Dir, "values.json")
	httpOnly := mustPolicy(t, []string{"http"}, []string{"api.example.com"}, false)
	if err := s.SetWithPolicy(ctx, "api.token", "private-token", httpOnly); err != nil {
		t.Fatal(err)
	}
	if err := s.Set(ctx, "free", "private-free"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]json.RawMessage
	if json.Unmarshal(data, &raw) != nil || string(raw[formatKey]) != policyFormat || ValidName(formatKey) {
		t.Fatalf("policy-bearing store lacks a marker that older readers refuse: %s", data)
	}
	before, err := s.List()
	if err != nil || !reflect.DeepEqual(before[0].Policy, httpOnly) {
		t.Fatalf("policy metadata missing: %+v %v", before, err)
	}

	// A value-only set keeps the policy, as console and older clients do.
	if err := s.Set(ctx, "api.token", "private-rotated"); err != nil {
		t.Fatal(err)
	}
	entries, _ := s.List()
	if !reflect.DeepEqual(entries[0].Policy, httpOnly) {
		t.Fatal("value-only set dropped the policy")
	}

	// A metadata-only change keeps the value and timestamps.
	execOnly := mustPolicy(t, []string{"exec"}, nil, false)
	if err := s.SetPolicy(ctx, "api.token", execOnly); err != nil {
		t.Fatal(err)
	}
	after, _ := s.List()
	values, _ := s.Snapshot()
	if !reflect.DeepEqual(after[0].Policy, execOnly) || !after[0].UpdatedAt.Equal(entries[0].UpdatedAt) || !after[0].CreatedAt.Equal(entries[0].CreatedAt) || values["api.token"] != "private-rotated" {
		t.Fatalf("policy update changed value or dates: %+v", after[0])
	}
	if err := s.SetPolicy(ctx, "missing", execOnly); err == nil {
		t.Fatal("set a policy for a secret that does not exist")
	}
	if err := s.SetPolicy(ctx, "api.token", &Policy{Uses: []Use{"http"}}); err == nil {
		t.Fatal("stored an invalid policy")
	}

	// Clearing makes it unrestricted; with no policy left the marker goes.
	if err := s.SetPolicy(ctx, "api.token", nil); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(path)
	if strings.Contains(string(data), formatKey) {
		t.Fatal("format marker kept without policies")
	}
	r, _ := s.Resolver()
	if _, err := r.Resolve([]Reference{httpRef(t, "api.token", "https://other.test/")}); err != nil {
		t.Fatal(err)
	}

	// Deleting removes the policy with the record; recreation is unrestricted.
	if err := s.SetWithPolicy(ctx, "api.token", "private-token", execOnly); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(ctx, "api.token"); err != nil {
		t.Fatal(err)
	}
	if err := s.Set(ctx, "api.token", "private-new"); err != nil {
		t.Fatal(err)
	}
	if entries, _ := s.List(); entries[0].Policy != nil {
		t.Fatal("recreated secret inherited a deleted policy")
	}
	if err := s.SetWithPolicy(ctx, "api.token", "private-new", httpOnly); err != nil {
		t.Fatal(err)
	}
	if err := s.SetWithPolicy(ctx, "api.token", "private-new", nil); err != nil {
		t.Fatal(err)
	}
	if entries, _ := s.List(); entries[0].Policy != nil {
		t.Fatal("explicit nil policy did not clear")
	}
}

func TestResolveChecksEveryReferenceFirst(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := policyStore(t)
	if err := s.SetWithPolicy(ctx, "api.token", "private-api", mustPolicy(t, []string{"http"}, []string{"api.example.com"}, false)); err != nil {
		t.Fatal(err)
	}
	if err := s.SetWithPolicy(ctx, "db.password", "private-db", mustPolicy(t, []string{"exec"}, nil, false)); err != nil {
		t.Fatal(err)
	}
	if err := s.SetWithPolicy(ctx, "app.password", "private-app", mustPolicy(t, []string{"desktop"}, nil, false)); err != nil {
		t.Fatal(err)
	}
	if err := s.Set(ctx, "free", "private-free"); err != nil {
		t.Fatal(err)
	}
	r, err := s.Resolver()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		refs []Reference
		want string // empty when permitted
	}{
		{[]Reference{httpRef(t, "api.token", "https://api.example.com/x"), httpRef(t, "free", "https://api.example.com/x")}, ""},
		{[]Reference{{Name: "db.password", Use: UseExec}, {Name: "free", Use: UseExec}}, ""},
		{[]Reference{{Name: "app.password", Use: UseDesktop}}, ""},
		{[]Reference{{Name: "api.token", Use: UseExec}}, `secret "api.token" is not permitted for exec use`},
		{[]Reference{{Name: "db.password", Use: UseDesktop}}, `secret "db.password" is not permitted for desktop use`},
		{[]Reference{httpRef(t, "db.password", "https://api.example.com/")}, `secret "db.password" is not permitted for http use`},
		{[]Reference{httpRef(t, "app.password", "https://api.example.com/")}, `secret "app.password" is not permitted for http use`},
		{[]Reference{httpRef(t, "api.token", "https://api.example.com.evil.test/")}, `secret "api.token" is not permitted for this HTTP destination`},
		{[]Reference{httpRef(t, "api.token", "http://api.example.com/")}, `secret "api.token" is not permitted for this HTTP destination`},
		{[]Reference{{Name: "api.token", Use: UseHTTP}}, `secret "api.token" is not permitted for this HTTP destination`},
		// One refused reference fails the whole set, whatever the order.
		{[]Reference{httpRef(t, "free", "https://evil.test/"), httpRef(t, "api.token", "https://evil.test/")}, `"api.token"`},
		{[]Reference{httpRef(t, "api.token", "https://evil.test/"), httpRef(t, "free", "https://evil.test/")}, `"api.token"`},
		{[]Reference{{Name: "free", Use: UseExec}, {Name: "missing", Use: UseExec}}, "not configured"},
		{[]Reference{{Name: "free", Use: "ssh"}}, "invalid secret use"},
		{[]Reference{{Name: "../values", Use: UseExec}}, "invalid secret reference"},
	} {
		values, err := r.Resolve(tc.refs)
		if tc.want == "" {
			if err != nil || len(values) == 0 {
				t.Errorf("refused %+v: %v", tc.refs, err)
			}
			continue
		}
		if err == nil || values != nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("Resolve(%+v) = %v, %v; want error containing %s", tc.refs, values, err, tc.want)
			continue
		}
		// Errors name the secret, never a value or another secret's policy.
		for _, leak := range []string{"private-", "api.example.com", "desktop, exec", "exec, http"} {
			if strings.Contains(err.Error(), leak) {
				t.Errorf("refusal %q discloses %q", err, leak)
			}
		}
	}
	// Masking still covers refused values.
	if masking := r.Masking(); len(masking) != 4 || masking["api.token"] != "private-api" {
		t.Fatal("masking omitted a restricted value")
	}
}

// A present but unreadable policy refuses every use of that secret only. It
// never falls back to unrestricted, survives unrelated rewrites verbatim, and
// is reported locally.
func TestCorruptPolicyFailsClosed(t *testing.T) {
	t.Parallel()
	for _, policy := range []string{
		`null`, `{}`, `"exec"`, `[]`, `{"uses":["exec"],"unknown":1}`, `{"uses":["http"]}`, `{"uses":["exec","desktop"]}`,
		`{"uses":["http"],"hosts":["API.example.com"]}`, `{"uses":["http"],"hosts":["*.com"]}`, `{"uses":["exec"],"hosts":"x"}`,
		`{"uses":["exec"],"hosts":["` + strings.Repeat("a", MaxPolicy) + `"]}`,
	} {
		s := policyStore(t)
		path := filepath.Join(s.Dir, "values.json")
		stored := `{"#format":2,"bad":{"value":"private-bad","policy":` + policy + `},"good":"private-good"}`
		if err := os.WriteFile(path, []byte(stored), 0600); err != nil {
			t.Fatal(err)
		}
		r, err := s.Resolver()
		if err != nil {
			t.Fatalf("policy %s: whole store refused: %v", policy, err)
		}
		for _, ref := range []Reference{{Name: "bad", Use: UseExec}, {Name: "bad", Use: UseDesktop}, httpRef(t, "bad", "https://api.example.com/")} {
			if _, err := r.Resolve([]Reference{ref}); err == nil || !strings.Contains(err.Error(), `secret "bad" has an invalid use policy`) || strings.Contains(err.Error(), "private-") {
				t.Fatalf("policy %s: %s use not refused clearly: %v", policy, ref.Use, err)
			}
		}
		if _, err := r.Resolve([]Reference{{Name: "good", Use: UseExec}}); err != nil {
			t.Fatalf("policy %s: unrelated secret refused: %v", policy, err)
		}
		if r.Masking()["bad"] != "private-bad" {
			t.Fatal("refused value is no longer masked")
		}
		entries, err := s.List()
		if err != nil || !entries[0].PolicyInvalid || entries[0].Policy != nil {
			t.Fatalf("invalid policy not reported: %+v %v", entries, err)
		}
		// Rewriting the store for another secret keeps the bad policy as is.
		if err := s.Set(context.Background(), "good", "private-good-2"); err != nil {
			t.Fatal(err)
		}
		data, _ := os.ReadFile(path)
		var records map[string]json.RawMessage
		if json.Unmarshal(data, &records) != nil || string(records[formatKey]) != policyFormat {
			t.Fatalf("rewrite dropped the format marker: %s", data)
		}
		var bad struct{ Policy json.RawMessage }
		_ = json.Unmarshal(records["bad"], &bad)
		if compact(t, bad.Policy) != compact(t, []byte(policy)) {
			t.Fatalf("rewrite changed the unreadable policy: %s -> %s", policy, bad.Policy)
		}
		// Setting a valid policy or clearing it recovers the secret.
		if err := s.SetPolicy(context.Background(), "bad", nil); err != nil {
			t.Fatal(err)
		}
		r, _ = s.Resolver()
		if _, err := r.Resolve([]Reference{{Name: "bad", Use: UseExec}}); err != nil {
			t.Fatal(err)
		}
	}
	// An unknown format is never read as a known one.
	s := policyStore(t)
	if err := os.WriteFile(filepath.Join(s.Dir, "values.json"), []byte(`{"#format":3,"a":"private"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Resolver(); err == nil {
		t.Fatal("read an unknown storage format")
	}
}

func compact(t *testing.T, raw []byte) string {
	t.Helper()
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return string(raw)
	}
	return string(must(json.Marshal(v)))
}

func TestPolicyStoreCapacityWithMarker(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := policyStore(t)
	p := mustPolicy(t, []string{"exec"}, nil, false)
	for i := range 128 {
		if err := s.SetWithPolicy(ctx, "s"+strings.Repeat("x", i%3)+string(rune('a'+i%26))+string(rune('a'+i/26)), "private", p); err != nil {
			t.Fatal(err)
		}
	}
	if entries, err := s.List(); err != nil || len(entries) != 128 {
		t.Fatalf("marker counted against capacity: %d %v", len(entries), err)
	}
}
