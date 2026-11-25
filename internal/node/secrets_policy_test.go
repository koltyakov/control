package node

import (
	"context"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/koltyakov/control/internal/identity"
	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/secrets"
)

func setPolicySecret(t *testing.T, n *Node, name, value string, uses []string, hosts []string, plain bool) {
	t.Helper()
	policy, err := secrets.NewPolicy(uses, hosts, plain)
	if err != nil {
		t.Fatal(err)
	}
	if err := n.secretStore().SetWithPolicy(context.Background(), name, value, policy); err != nil {
		t.Fatal(err)
	}
}

func hostOf(t *testing.T, raw string) string {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u.Host
}

func TestHTTPSecretPolicy(t *testing.T) {
	t.Parallel()
	n := localNode(t, nil)
	var hits atomic.Int32
	var authorization atomic.Value
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		authorization.Store(r.Header.Get("Authorization") + "|" + r.Header.Get("X-Other"))
		if r.URL.Path == "/redirect" {
			http.Redirect(w, r, r.URL.Query().Get("to"), http.StatusFound)
		}
	}))
	defer server.Close()
	var otherHits atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { otherHits.Add(1) }))
	defer other.Close()
	host := hostOf(t, server.URL)
	setPolicySecret(t, n, "api.token", "private-api-token", []string{"http"}, []string{host}, true)
	setPolicySecret(t, n, "https.token", "private-https-token", []string{"http"}, []string{host}, false)
	setPolicySecret(t, n, "exec.token", "private-exec-token", []string{"exec"}, nil, false)
	setTestSecret(t, n, "free.token", "private-free-token")
	request := func(rawURL string, refs map[string]string) (any, error) {
		headers := map[string]any{}
		for header, name := range refs {
			headers[header] = map[string]any{"secret": name, "prefix": "Bearer "}
		}
		return n.httpRequest(t.Context(), model.JSON(map[string]any{"url": rawURL, "headerSecrets": headers}), Execution{Log: io.Discard})
	}

	if _, err := request(server.URL+"/ok", map[string]string{"Authorization": "api.token", "X-Other": "free.token"}); err != nil || hits.Load() != 1 || authorization.Load() != "Bearer private-api-token|Bearer private-free-token" {
		t.Fatalf("permitted request failed: %v", err)
	}
	for name, tc := range map[string]struct {
		url  string
		refs map[string]string
	}{
		"https required":     {server.URL, map[string]string{"Authorization": "https.token"}},
		"other host":         {other.URL, map[string]string{"Authorization": "api.token"}},
		"other name":         {"http://" + strings.Replace(host, "127.0.0.1", "localhost", 1), map[string]string{"Authorization": "api.token"}},
		"userinfo":           {strings.Replace(server.URL, "http://", "http://user@", 1), map[string]string{"Authorization": "api.token"}},
		"exec only":          {server.URL, map[string]string{"Authorization": "exec.token"}},
		"one of two refused": {server.URL, map[string]string{"Authorization": "free.token", "X-Other": "exec.token"}},
	} {
		before := hits.Load() + otherHits.Load()
		value, err := request(tc.url, tc.refs)
		if err == nil || value != nil || !strings.Contains(err.Error(), "not permitted") || strings.Contains(err.Error(), "private-") {
			t.Errorf("%s: %v, %v", name, value, err)
		}
		if hits.Load()+otherHits.Load() != before {
			t.Errorf("%s: a request was sent", name)
		}
	}
	// Policy is checked against the connected URL only, and secret-bearing
	// requests never follow redirects, so a permitted host cannot forward it.
	value, err := request(server.URL+"/redirect?to="+url.QueryEscape(other.URL+"/stolen"), map[string]string{"Authorization": "api.token"})
	if err != nil || otherHits.Load() != 0 || value.(map[string]any)["status"] != http.StatusFound {
		t.Fatalf("secret-bearing redirect followed: %v %v", value, err)
	}

	download := func(rawURL, name string) error {
		_, err := n.httpDownload(t.Context(), model.JSON(map[string]any{"url": rawURL, "headerSecrets": map[string]any{"Authorization": map[string]any{"secret": name}}}), Execution{Log: io.Discard, Owner: "owner"})
		return err
	}
	before := otherHits.Load()
	if err := download(other.URL, "api.token"); err == nil || !strings.Contains(err.Error(), `secret "api.token" is not permitted for this HTTP destination`) || otherHits.Load() != before {
		t.Fatalf("http.download sent a restricted secret: %v", err)
	}
	if err := download(server.URL+"/redirect?to="+url.QueryEscape(other.URL), "api.token"); err == nil || otherHits.Load() != before {
		t.Fatalf("http.download followed a secret-bearing redirect: %v", err)
	}
	if err := download(server.URL+"/file", "exec.token"); err == nil || !strings.Contains(err.Error(), "not permitted for http use") {
		t.Fatalf("http.download used an exec-only secret: %v", err)
	}
}

func TestExecSecretPolicy(t *testing.T) {
	t.Parallel()
	n := localNode(t, nil)
	setPolicySecret(t, n, "domain.admin.password", "private-token", []string{"exec"}, nil, false)
	setPolicySecret(t, n, "stdin.password", "stdin-token", []string{"desktop", "exec"}, nil, false)
	setPolicySecret(t, n, "http.only", "private-http", []string{"http"}, []string{"api.example.com"}, false)
	run := func(mutate func(map[string]any)) (string, error) {
		args := execSecretArgs(t, "echo")
		marker := filepath.Join(t.TempDir(), "started")
		args["env"].(map[string]string)["CONTROL_TEST_EXEC_MARKER"] = marker
		mutate(args)
		value, err := n.execRun(t.Context(), model.JSON(args), Execution{Dir: n.Config.WorkDir, Log: io.Discard})
		if _, statErr := os.Stat(marker); err != nil && !os.IsNotExist(statErr) {
			t.Fatal("command launched before a refused reference was rejected")
		}
		if value == nil {
			return "", err
		}
		return value.(commandResult).Stdout, err
	}
	if out, err := run(func(map[string]any) {}); err != nil || out != "[REDACTED]|[REDACTED]|ordinary\n" {
		t.Fatalf("permitted exec references refused: %q %v", out, err)
	}
	for name, mutate := range map[string]func(map[string]any){
		"env":   func(a map[string]any) { a["envSecrets"] = map[string]string{"CONTROL_TEST_PASSWORD": "http.only"} },
		"stdin": func(a map[string]any) { a["stdinSecret"] = "http.only" },
		"mixed": func(a map[string]any) {
			a["envSecrets"] = map[string]string{"CONTROL_TEST_PASSWORD": "domain.admin.password", "OTHER": "http.only"}
		},
	} {
		if _, err := run(mutate); err == nil || !strings.Contains(err.Error(), `secret "http.only" is not permitted for exec use`) || strings.Contains(err.Error(), "private-") {
			t.Errorf("%s: restricted secret injected: %v", name, err)
		}
	}
	// A corrupt policy fails closed for exec too.
	path := filepath.Join(n.Config.DataDir, "secrets", "values.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var records map[string]json.RawMessage
	if err := json.Unmarshal(data, &records); err != nil {
		t.Fatal(err)
	}
	records["domain.admin.password"] = json.RawMessage(`{"value":"private-token","policy":{"uses":["exec"],"bogus":true}}`)
	if err := os.WriteFile(path, model.JSON(records), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := run(func(map[string]any) {}); err == nil || !strings.Contains(err.Error(), "invalid use policy") {
		t.Fatalf("corrupt policy did not fail closed: %v", err)
	}
}

func TestDesktopSecretPolicy(t *testing.T) {
	t.Parallel()
	n := desktopNode(t, "echo")
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	setValue := json.RawMessage(`{"command":"setValue","ref":"@s123:e1","secret":"app.password"}`)
	setPolicySecret(t, n, "app.password", desktopCredential, []string{"exec"}, nil, false)
	if value, err := n.desktopRun(ctx, setValue, Execution{Dir: n.Config.WorkDir, Log: io.Discard}); err == nil || value != nil || !strings.Contains(err.Error(), "not permitted for desktop use") || hasDesktopCredential([]byte(err.Error())) {
		t.Fatalf("desktop used an exec-only secret: %v %v", value, err)
	}
	setPolicySecret(t, n, "app.password", desktopCredential, []string{"desktop"}, nil, false)
	if _, err := n.desktopRun(ctx, setValue, Execution{Dir: n.Config.WorkDir, Log: io.Discard}); err != nil {
		t.Fatalf("permitted desktop reference refused: %v", err)
	}
}

func TestRemoteSecretPolicy(t *testing.T) {
	t.Parallel()
	n := localNode(t, nil)
	key, err := secrets.NewManagementKey()
	if err != nil {
		t.Fatal(err)
	}
	m := &secretManagement{key: key, seen: map[string]int64{}}
	public := key.PublicKey().Bytes()
	set := func(e secrets.Envelope) secrets.ManagementReply {
		return m.run(t.Context(), n, secrets.ManagementRequest{ID: e.ID, Operation: "set", Name: "api", ExpiresAt: e.ExpiresAt, Envelope: &e})
	}
	list := func(policies bool) []secrets.Metadata {
		reply := m.run(t.Context(), n, secrets.ManagementRequest{ID: identity.NewID(), Operation: "list", ExpiresAt: time.Now().Add(30 * time.Second).Unix(), Policies: policies})
		if reply.Error != "" {
			t.Fatal(reply.Error)
		}
		return reply.Secrets
	}
	policy, _ := secrets.NewPolicy([]string{"http"}, []string{"api.example.com"}, false)
	e, err := secrets.SealPolicy(n.Identity.ID, "api", "private-first", policy, public)
	if err != nil {
		t.Fatal(err)
	}
	if reply := set(e); reply.Error != "" {
		t.Fatal(reply.Error)
	}
	if got := list(true); !reflect.DeepEqual(got[0].Policy, policy) {
		t.Fatalf("policy not stored: %+v", got)
	}
	// Gateways that did not ask never receive policy fields.
	if got := list(false); got[0].Policy != nil || got[0].PolicyInvalid || strings.Contains(string(model.JSON(got)), "policy") {
		t.Fatalf("unrequested policy metadata: %s", model.JSON(got))
	}
	// A value-only write keeps the policy; an explicit policy write replaces it.
	plain, _ := secrets.Seal(n.Identity.ID, "api", "private-second", public)
	if reply := set(plain); reply.Error != "" {
		t.Fatal(reply.Error)
	}
	if got := list(true); !reflect.DeepEqual(got[0].Policy, policy) {
		t.Fatal("value-only remote write dropped the policy")
	}
	values, _ := n.secretValues()
	if values["api"] != "private-second" {
		t.Fatal("value-only write did not update the value")
	}
	// A relay that relabels a policy envelope cannot get it stored.
	stripped, _ := secrets.SealPolicy(n.Identity.ID, "api", "private-third", nil, public)
	stripped.Version = 0
	if reply := set(stripped); reply.Error != "invalid" {
		t.Fatalf("relabelled envelope accepted: %+v", reply)
	}
	cleared, _ := secrets.SealPolicy(n.Identity.ID, "api", "private-third", nil, public)
	if reply := set(cleared); reply.Error != "" {
		t.Fatal(reply.Error)
	}
	if got := list(true); got[0].Policy != nil {
		t.Fatal("explicit remote clear ignored")
	}
}

// TestSecretValueCallSites keeps the set of production functions that obtain
// secret values closed. Values for use must come from Resolver.Resolve, which
// enforces use policies; Masking and secretValues return every value for
// redaction or validation only. A new consumer fails here until it routes
// through Resolve and is reviewed into this list.
func TestSecretValueCallSites(t *testing.T) {
	t.Parallel()
	root := repositoryRoot(t)
	want := []string{
		"internal/node/browser.go browserRun secretValues",
		"internal/node/desktop.go desktopRun Masking",
		"internal/node/desktop.go desktopRun Resolve",
		"internal/node/desktop.go desktopRun secretResolver",
		"internal/node/exec_secrets.go execRun Masking",
		"internal/node/exec_secrets.go execRun Resolve",
		"internal/node/exec_secrets.go execRun secretResolver",
		"internal/node/http_download.go httpDownload Masking",
		"internal/node/http_download.go httpDownload secretResolver",
		"internal/node/http_secrets.go applyHeaderSecrets Resolve",
		"internal/node/node.go New secretValues",
		"internal/node/providers.go httpRequest Masking",
		"internal/node/providers.go httpRequest secretResolver",
		"internal/node/secrets.go secretResolver Resolver",
		"internal/node/secrets.go secretValues Snapshot",
	}
	value := map[string]bool{"Snapshot": true, "Resolver": true, "Resolve": true, "Masking": true, "secretValues": true, "secretResolver": true}
	var got []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if name := d.Name(); rel != "." && (strings.HasPrefix(name, ".") || name == "tmp" || name == "testdata" || name == "node_modules") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(rel, ".go") || strings.HasSuffix(rel, "_test.go") || strings.HasPrefix(rel, "internal/secrets/") {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if strings.Contains(string(src), "values.json") {
			// Reading the store's file directly would bypass every check.
			got = append(got, rel+" reads values.json")
		}
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, rel, src, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		imports := strings.HasPrefix(rel, "internal/node/")
		for _, spec := range file.Imports {
			imports = imports || spec.Path.Value == `"github.com/koltyakov/control/internal/secrets"`
		}
		if !imports {
			return nil
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(node ast.Node) bool {
				call, ok := node.(*ast.CallExpr)
				if !ok {
					return true
				}
				if sel, ok := call.Fun.(*ast.SelectorExpr); ok && value[sel.Sel.Name] {
					receiver := string(src[fset.Position(sel.X.Pos()).Offset:fset.Position(sel.X.End()).Offset])
					// Unrelated methods sharing a name: the resource sampler,
					// clipboard batches and peer name resolution hold no
					// secrets. Resolve itself enforces policy, so it is listed
					// only to document the consumers.
					if (sel.Sel.Name == "Snapshot" && (receiver == "n.system" || receiver == "clipboard")) || (sel.Sel.Name == "Resolve" && receiver != "resolver") {
						return true
					}
					got = append(got, rel+" "+fn.Name.Name+" "+sel.Sel.Name)
				}
				return true
			})
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(got)
	got = slices.Compact(got)
	if !slices.Equal(got, want) {
		t.Fatalf("secret value call sites changed; route new consumers through secrets.Resolver.Resolve with their use and destination, then update this list.\n got %q\nwant %q", got, want)
	}
}
