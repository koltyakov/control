package gateway

import (
	"bytes"
	"context"
	"crypto/tls"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/koltyakov/control/internal/enrollment"
	"github.com/koltyakov/control/internal/identity"
	"github.com/koltyakov/control/internal/model"
)

const browserPassword = "a separate browser password"

type testBrowser struct {
	URL, CSRF string
	Cookie    *http.Cookie
}

func (b testBrowser) request(t *testing.T, method, path string, body any, status int) ([]byte, http.Header) {
	t.Helper()
	var data []byte
	if body != nil {
		data = model.JSON(body)
	}
	r, err := http.NewRequest(method, b.URL+path, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Origin", b.URL)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-CSRF-Token", b.CSRF)
	if b.Cookie != nil {
		r.AddCookie(b.Cookie)
	}
	c := &http.Client{CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := c.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	result, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != status {
		t.Fatalf("%s %s: got %d, want %d: %s", method, path, resp.StatusCode, status, result)
	}
	return result, resp.Header
}

func browserLogin(t *testing.T, base, username, password string) testBrowser {
	t.Helper()
	b := testBrowser{URL: base}
	data, headers := b.request(t, "POST", "/console/api/login", map[string]string{"username": username, "password": password}, 200)
	var session struct{ CSRF string }
	if err := json.Unmarshal(data, &session); err != nil || session.CSRF == "" {
		t.Fatalf("invalid session: %s: %v", data, err)
	}
	cookies := (&http.Response{Header: headers}).Cookies()
	if len(cookies) != 1 || !cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteStrictMode {
		t.Fatal("unsafe cookie", cookies)
	}
	b.Cookie, b.CSRF = cookies[0], session.CSRF
	return b
}

func provisionBrowser(t *testing.T, a testAdmin, username string) {
	t.Helper()
	if err := a.JSON(context.Background(), "PUT", "/v1/console/password", map[string]string{"username": username, "password": browserPassword}, nil); err != nil {
		t.Fatal(err)
	}
}

func TestConsoleFleetIsolationAndManagement(t *testing.T) {
	g, s := installationFixture(t)
	alice, a := createTestUser(t, s, "alice")
	bob, b := createTestUser(t, s, "bob")
	provisionBrowser(t, a, "")
	provisionBrowser(t, b, "")
	provisionBrowser(t, testAdmin{URL: s.URL, Key: superKey}, "")
	ba := browserLogin(t, s.URL, "alice", browserPassword)
	bb := browserLogin(t, s.URL, "bob", browserPassword)
	operator := browserLogin(t, s.URL, "@operator", browserPassword)
	ids := make([]string, 2)
	for i, owner := range []struct {
		user User
		key  string
	}{{alice, a.Key}, {bob, b.Key}} {
		id, err := identity.Load(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		if rawPeer(t, s.URL, owner.key, "worker", owner.user.ID, id) == nil {
			t.Fatal("node registration failed")
		}
		ids[i] = id.ID
		g.mu.Lock()
		n := g.nodes[id.ID]
		n.Managed = true
		g.nodes[id.ID] = n
		err = g.commit(change{nodes: []string{id.ID}})
		g.mu.Unlock()
		if err != nil {
			t.Fatal(err)
		}
	}
	for i, browser := range []testBrowser{ba, bb} {
		data, _ := browser.request(t, "GET", "/console/api/status", nil, 200)
		var pool model.PoolActivitySnapshot
		if err := json.Unmarshal(data, &pool); err != nil || len(pool.Nodes) != 1 || pool.Nodes[0].ID != ids[i] {
			t.Fatalf("foreign fleet visible: %s: %v", data, err)
		}
		browser.request(t, "PATCH", "/console/api/fleet/nodes/"+ids[1-i], map[string]string{"name": "stolen"}, 404)
		browser.request(t, "GET", "/console/api/admin/users", nil, 403)
		browser.request(t, "GET", "/console/api/admin/updates", nil, 403)
	}
	ba.request(t, "PATCH", "/console/api/fleet/nodes/"+ids[0], map[string]string{"name": "renamed"}, 200)
	ba.request(t, "PATCH", "/console/api/fleet/nodes/"+ids[0], map[string]bool{"disabled": true}, 200)
	ba.request(t, "PATCH", "/console/api/fleet/nodes/"+ids[0], map[string]bool{"disabled": false}, 200)
	data, _ := ba.request(t, "POST", "/console/api/fleet/keys", map[string]string{"name": "browser-created"}, 200)
	var issued struct {
		Key   APIKey
		Token string
	}
	if err := json.Unmarshal(data, &issued); err != nil || issued.Key.UserID != alice.ID || issued.Token == "" {
		t.Fatal("key issuance", string(data), err)
	}
	bb.request(t, "DELETE", "/console/api/fleet/keys/"+issued.Key.ID, nil, 404)
	ba.request(t, "DELETE", "/console/api/fleet/keys/"+issued.Key.ID, nil, 204)
	data, _ = ba.request(t, "POST", "/console/api/fleet/installations", enrollment.Request{Name: "new-machine", OS: "linux", Arch: "amd64", Gateway: s.URL}, 200)
	var link enrollment.Link
	if err := json.Unmarshal(data, &link); err != nil || link.Command == "" {
		t.Fatal("installation command", string(data), err)
	}
	bb.request(t, "DELETE", "/console/api/fleet/installations/"+link.ID, nil, 404)
	ba.request(t, "DELETE", "/console/api/fleet/installations/"+link.ID, nil, 204)
	operator.request(t, "GET", "/console/api/admin/users", nil, 200)
	operator.request(t, "GET", "/console/api/admin/updates", nil, 200)
	operator.request(t, "PATCH", "/console/api/fleet/nodes/"+ids[0], map[string]string{"name": "operator-stolen"}, 404)
	ba.request(t, "DELETE", "/console/api/fleet/nodes/"+ids[0]+"?stop=true", nil, 204)
}

func TestConsoleHybridUserReplacement(t *testing.T) {
	g, server := installationFixture(t)
	owner, admin := createTestUser(t, server, "startup-owner")
	provisionBrowser(t, admin, "")
	browser := browserLogin(t, server.URL, owner.Name, browserPassword)
	id, err := identity.Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if rawPeer(t, server.URL, admin.Key, "existing-user", owner.ID, id) == nil {
		t.Fatal("node registration failed")
	}
	data, _ := browser.request(t, "POST", "/console/api/fleet/installations", enrollment.Request{
		Name: "existing-user", OS: "linux", Arch: "amd64", Gateway: server.URL, ServiceMode: "hybrid",
	}, http.StatusOK)
	var link enrollment.Link
	if err := json.Unmarshal(data, &link); err != nil {
		t.Fatal(err)
	}
	if link.ReplaceID != id.ID || link.ServiceMode != "hybrid" || strings.Contains(link.Command, "sudo bash") {
		t.Fatal("replacement did not retain the User identity and target-local elevation", link)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	node, ok := g.nodes[id.ID]
	if !ok || node.Name != "existing-user" || !node.Online || node.Disabled || len(g.nodes) != 1 {
		t.Fatal("preparing an installation changed the existing node", node)
	}
	var user, system installation
	for _, invitation := range g.installations {
		if invitation.HybridID != link.HybridID {
			continue
		}
		if invitation.HybridRole == "system" {
			system = invitation
		} else {
			user = invitation
		}
	}
	if user.ReplaceID != id.ID || system.ID == "" || system.ReplaceID != "" || system.Name != "existing-user-system" || system.UserID != owner.ID {
		t.Fatal("Hybrid replacement did not create an independent, same-fleet companion", user, system)
	}
}

func TestConsoleSessionBoundariesAndRevocation(t *testing.T) {
	g, s := installationFixture(t)
	alice, a := createTestUser(t, s, "alice")
	provisionBrowser(t, a, "")
	b := browserLogin(t, s.URL, "alice", browserPassword)
	for _, path := range []string{"/v1/status", "/v1/auth", "/v1/connect", "/v1/client/connect"} {
		b.request(t, "GET", path, nil, 401)
	}
	b.request(t, "PUT", "/v1/console/password", map[string]string{"password": "not a bearer credential"}, 403)
	for _, path := range []string{"/console/api/client/connect", "/console/api/connect", "/console/api/call", "/console/api/node/state", "/console/api/admin/updates/blobs/abc"} {
		b.request(t, "GET", path, nil, 404)
	}
	noCSRF := b
	noCSRF.CSRF = ""
	noCSRF.request(t, "POST", "/console/api/fleet/keys", map[string]string{"name": "forged"}, 403)
	noCSRF.request(t, "POST", "/console/api/logout", nil, 403)
	b.request(t, "PUT", "/console/api/console/password", map[string]string{"password": "changed password", "currentPassword": "wrong"}, 403)
	b.request(t, "PUT", "/console/api/console/password", map[string]string{"username": "@operator", "password": "changed password"}, 403)
	b.request(t, "PUT", "/console/api/console/password", map[string]string{"password": "changed password", "currentPassword": browserPassword}, 204)
	b.request(t, "GET", "/console/api/session", nil, 401)
	reset := browserLogin(t, s.URL, "alice", "changed password")
	provisionBrowser(t, testAdmin{URL: s.URL, Key: superKey}, "alice")
	reset.request(t, "GET", "/console/api/status", nil, 401)
	b = browserLogin(t, s.URL, "alice", browserPassword)
	b.request(t, "POST", "/console/api/logout", nil, 204)
	b.request(t, "GET", "/console/api/session", nil, 401)
	b = browserLogin(t, s.URL, "alice", browserPassword)
	if err := (testAdmin{URL: s.URL, Key: superKey}).JSON(context.Background(), "DELETE", "/v1/admin/users/"+alice.ID, nil, nil); err != nil {
		t.Fatal(err)
	}
	b.request(t, "GET", "/console/api/status", nil, 401)
	(testBrowser{URL: s.URL}).request(t, "POST", "/console/api/login", map[string]string{"username": "alice", "password": browserPassword}, 401)
	if g.authenticate(a.Key).Role != "" {
		t.Fatal("disabled account still authenticates")
	}
}

func TestConsoleExpiryThrottlingAndHeaders(t *testing.T) {
	g, s := installationFixture(t)
	_, a := createTestUser(t, s, "alice")
	provisionBrowser(t, a, "")
	for _, absolute := range []bool{false, true} {
		b := browserLogin(t, s.URL, "alice", browserPassword)
		key := tokenHash(b.Cookie.Value)
		g.console.mu.Lock()
		session := g.console.sessions[key]
		if absolute {
			session.Created = time.Now().Add(-13 * time.Hour)
		} else {
			session.Seen = time.Now().Add(-31 * time.Minute)
		}
		g.console.sessions[key] = session
		g.console.mu.Unlock()
		b.request(t, "GET", "/console/api/session", nil, 401)
	}
	b := testBrowser{URL: s.URL}
	for _, path := range []string{"/login", "/console/static/console.js", "/console/static/console.css", "/console/static/tokens.css", "/console/static/brand-icon.png", "/console/static/favicon-32.png", "/console/static/favicon.ico"} {
		_, headers := b.request(t, "GET", path, nil, 200)
		if headers.Get("Cache-Control") != "no-store" || headers.Get("Content-Security-Policy") == "" || headers.Get("Referrer-Policy") != "no-referrer" || headers.Get("X-Content-Type-Options") != "nosniff" {
			t.Fatal("missing security headers", path, headers)
		}
	}
	b.request(t, "GET", "/console", nil, 303)
	unknown, _ := b.request(t, "POST", "/console/api/login", map[string]string{"username": "unknown", "password": browserPassword}, 401)
	wrong, _ := b.request(t, "POST", "/console/api/login", map[string]string{"username": "alice", "password": "wrong"}, 401)
	if string(unknown) != string(wrong) {
		t.Fatal("login reveals account existence")
	}
	for range 2 {
		g.console.verify <- struct{}{}
	}
	b.request(t, "POST", "/console/api/login", map[string]string{"username": "alice", "password": browserPassword}, 429)
	for range 2 {
		<-g.console.verify
	}
	g.console.mu.Lock()
	g.console.attempts["user:alice"] = consoleAttempt{Count: 10, Until: time.Now().Add(time.Minute)}
	g.console.mu.Unlock()
	b.request(t, "POST", "/console/api/login", map[string]string{"username": "alice", "password": browserPassword}, 429)
}

func TestConsoleHTTPSAndOriginPolicy(t *testing.T) {
	g, s := installationFixture(t)
	_, a := createTestUser(t, s, "alice")
	provisionBrowser(t, a, "")
	b := browserLogin(t, s.URL, "alice", browserPassword)
	for _, origin := range []string{"", "https://evil.example"} {
		r := httptest.NewRequest("POST", s.URL+"/console/api/logout", nil)
		r.RemoteAddr = "127.0.0.1:1234"
		r.AddCookie(b.Cookie)
		r.Header.Set("Origin", origin)
		r.Header.Set("X-CSRF-Token", b.CSRF)
		w := httptest.NewRecorder()
		g.Handler().ServeHTTP(w, r)
		if w.Code != 403 {
			t.Fatalf("accepted foreign/missing origin %q: %d", origin, w.Code)
		}
	}
	for _, tc := range []struct {
		public, host, remote string
		tls, allowed         bool
	}{
		{"", "localhost:7330", "127.0.0.1:1234", false, true},
		{"", "localhost:7330", "192.0.2.1:1234", false, false},
		{"", "evil.example", "127.0.0.1:1234", false, false},
		{"", "control.example", "192.0.2.1:1234", true, true},
		{"https://control.example", "control.example", "127.0.0.1:1234", false, true},
		{"https://control.example", "control.example", "192.0.2.1:1234", false, false},
		{"https://control.example", "evil.example", "127.0.0.1:1234", false, false},
		{"http://localhost:7330", "localhost:7330", "127.0.0.1:1234", false, true},
	} {
		g.options.PublicURL = tc.public
		r := httptest.NewRequest("GET", "http://"+tc.host+"/login", nil)
		r.RemoteAddr = tc.remote
		r.Header.Set("X-Forwarded-Proto", "https")
		if tc.tls {
			r.TLS = &tls.ConnectionState{}
		}
		_, allowed := g.consoleOrigin(r)
		if allowed != tc.allowed {
			t.Fatalf("origin policy %+v: %v", tc, allowed)
		}
	}
	g.options.PublicURL = "https://control.example"
	r := httptest.NewRequest("POST", "http://control.example/console/api/login", strings.NewReader(string(model.JSON(map[string]string{"username": "alice", "password": browserPassword}))))
	r.RemoteAddr = "127.0.0.1:1234"
	r.Header.Set("Origin", "https://control.example")
	w := httptest.NewRecorder()
	g.Handler().ServeHTTP(w, r)
	if w.Code != 200 || !w.Result().Cookies()[0].Secure || !strings.HasPrefix(w.Result().Cookies()[0].Name, "__Host-") {
		t.Fatal("proxy login missing secure cookie", w.Code)
	}
}

func TestConsolePasswordPersistenceAndRollback(t *testing.T) {
	dir := t.TempDir()
	g, err := New(dir, commonKey, Options{SuperuserKey: superKey})
	if err != nil {
		t.Fatal(err)
	}
	s := httptest.NewServer(g.Handler())
	t.Cleanup(func() { s.Close(); _ = g.Close() })
	_, a := createTestUser(t, s, "alice")
	provisionBrowser(t, a, "")
	b := browserLogin(t, s.URL, "alice", browserPassword)
	old := g.console.passwords["alice"]
	if len(old.Salt) != 16 || len(old.Hash) != 32 || bytes.Contains(old.Hash, []byte(browserPassword)) {
		t.Fatal("password not stored as salted hash")
	}
	if _, err := g.db.Exec(`PRAGMA query_only=ON`); err != nil {
		t.Fatal(err)
	}
	if err := a.JSON(context.Background(), "PUT", "/v1/console/password", map[string]string{"password": "another long password"}, nil); err == nil {
		t.Fatal("read-only database accepted password change")
	}
	if g.console.passwords["alice"].Revision != old.Revision {
		t.Fatal("failed password commit was published")
	}
	b.request(t, "GET", "/console/api/session", nil, 200)
	s.Close()
	_ = g.Close()
	// Simulate an actual deployed version 4, with no console table.
	db, err := sql.Open("sqlite", filepath.Join(dir, "gateway.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DROP TABLE console_passwords; PRAGMA user_version=4;`); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	g, err = New(dir, commonKey, Options{SuperuserKey: superKey})
	if err != nil {
		t.Fatal(err)
	}
	s = httptest.NewServer(g.Handler())
	a.URL = s.URL
	provisionBrowser(t, a, "")
	s.Close()
	_ = g.Close()
	g, err = New(dir, commonKey, Options{SuperuserKey: superKey})
	if err != nil {
		t.Fatal(err)
	}
	s = httptest.NewServer(g.Handler())
	b.URL = s.URL
	b.request(t, "GET", "/console/api/session", nil, 401)
	browserLogin(t, s.URL, "alice", browserPassword)
}
