package gateway

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestConsoleSessionsSurviveRestarts(t *testing.T) {
	for _, scenario := range []string{"active", "logout", "replacement", "password", "disabled", "idle", "absolute", "operator", "operator-disabled"} {
		t.Run(scenario, func(t *testing.T) {
			dir := t.TempDir()
			var g *Gateway
			var server *httptest.Server
			open := func(operator bool) {
				t.Helper()
				var err error
				options := Options{}
				if operator {
					options.SuperuserKey = superKey
				}
				g, err = New(dir, commonKey, options)
				if err != nil {
					t.Fatal(err)
				}
				server = httptest.NewServer(g.Handler())
			}
			closeGateway := func() {
				t.Helper()
				server.Close()
				if err := g.Close(); err != nil {
					t.Fatal(err)
				}
			}
			open(true)
			t.Cleanup(func() { server.Close(); _ = g.Close() })
			user, admin := createTestUser(t, server, "alice")
			username := "alice"
			if strings.HasPrefix(scenario, "operator") {
				username = "@operator"
				admin = testAdmin{URL: server.URL, Key: superKey}
			}
			provisionBrowser(t, admin, "")
			browser := browserLogin(t, server.URL, username, browserPassword)
			key := tokenHash(browser.Cookie.Value)
			var persisted string
			if err := g.db.QueryRow(`SELECT data FROM console_sessions WHERE hash=?`, key).Scan(&persisted); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(persisted, browser.Cookie.Value) {
				t.Fatal("raw session cookie persisted")
			}
			g.mu.Lock()
			session := g.console.sessions[key]
			session.Created = time.Now().Add(-2 * time.Hour)
			session.Seen = time.Now().Add(-29 * time.Minute)
			session.saved = session.Seen
			g.console.sessions[key] = session
			err := g.commit(change{sessions: []string{key}})
			g.mu.Unlock()
			if err != nil {
				t.Fatal(err)
			}
			// Force an activity checkpoint, then exercise graceful-shutdown saving.
			browser.request(t, "GET", "/console/api/session", nil, 200)
			browser.request(t, "GET", "/console/api/session", nil, 200)
			g.mu.Lock()
			session = g.console.sessions[key]
			created := session.Created
			seen := session.Seen
			g.mu.Unlock()
			if err := g.db.QueryRow(`SELECT data FROM console_sessions WHERE hash=?`, key).Scan(&persisted); err != nil {
				t.Fatal(err)
			}
			var checkpoint consoleSession
			if err := json.Unmarshal([]byte(persisted), &checkpoint); err != nil || !checkpoint.Seen.Equal(session.saved) || !checkpoint.Seen.Before(seen) {
				t.Fatal("polling did not coalesce activity writes", checkpoint, session, err)
			}
			switch scenario {
			case "logout":
				browser.request(t, "POST", "/console/api/logout", nil, 204)
			case "replacement":
				browser.request(t, "POST", "/console/api/login", map[string]string{"username": username, "password": browserPassword}, 200)
			case "password":
				provisionBrowser(t, admin, "")
			case "disabled":
				if err := (testAdmin{URL: server.URL, Key: superKey}).JSON(context.Background(), "DELETE", "/v1/admin/users/"+user.ID, nil, nil); err != nil {
					t.Fatal(err)
				}
			case "idle", "absolute":
				g.mu.Lock()
				session = g.console.sessions[key]
				if scenario == "idle" {
					session.Seen = time.Now().Add(-31 * time.Minute)
				} else {
					session.Created = time.Now().Add(-13 * time.Hour)
				}
				g.console.sessions[key] = session
				err := g.commit(change{sessions: []string{key}})
				g.mu.Unlock()
				if err != nil {
					t.Fatal(err)
				}
			}
			closeGateway()
			open(scenario != "operator-disabled")
			browser.URL = server.URL
			status := 401
			if scenario == "active" || scenario == "operator" {
				status = 200
				g.mu.Lock()
				restored := g.console.sessions[key]
				g.mu.Unlock()
				if !restored.Created.Equal(created) || restored.Seen.Before(seen) || restored.CSRF != browser.CSRF {
					t.Fatal("restart reset session lifetime, lost activity, or changed CSRF", restored)
				}
				browser.request(t, "POST", "/console/api/fleet/keys", map[string]string{"name": "after-update"}, 200)
				browser.request(t, "GET", "/v1/status", nil, 401)
			}
			browser.request(t, "GET", "/console/api/session", nil, status)
			// Repeated restarts cannot restore a revoked or expired session.
			closeGateway()
			open(scenario != "operator-disabled")
			browser.URL = server.URL
			browser.request(t, "GET", "/console/api/session", nil, status)
		})
	}
}

func TestConsoleSessionPersistenceFailure(t *testing.T) {
	g, server := installationFixture(t)
	_, admin := createTestUser(t, server, "alice")
	provisionBrowser(t, admin, "")
	browser := browserLogin(t, server.URL, "alice", browserPassword)
	if _, err := g.db.Exec(`PRAGMA query_only=ON`); err != nil {
		t.Fatal(err)
	}
	_, headers := browser.request(t, "POST", "/console/api/login", map[string]string{"username": "alice", "password": browserPassword}, 500)
	if len(headers.Values("Set-Cookie")) != 0 {
		t.Fatal("failed login issued a cookie")
	}
	_, headers = browser.request(t, "POST", "/console/api/logout", nil, 500)
	if len(headers.Values("Set-Cookie")) != 0 {
		t.Fatal("failed logout cleared the cookie without durable revocation")
	}
	browser.request(t, "GET", "/console/api/session", nil, 200)
	if _, err := g.db.Exec(`PRAGMA query_only=OFF`); err != nil {
		t.Fatal(err)
	}
	browser.request(t, "POST", "/console/api/logout", nil, 204)
	var count int
	if err := g.db.QueryRow(`SELECT count(*) FROM console_sessions`).Scan(&count); err != nil || count != 0 {
		t.Fatal("failed login leaked a session or logout was not persisted", count, err)
	}
}

func TestConsoleSessionSchemaMigration(t *testing.T) {
	dir := t.TempDir()
	g, err := New(dir, commonKey, Options{SuperuserKey: superKey})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(g.Handler())
	_, admin := createTestUser(t, server, "alice")
	provisionBrowser(t, admin, "")
	server.Close()
	if err := g.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(dir, "gateway.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DROP TABLE console_sessions; PRAGMA user_version=6;`); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	g, err = New(dir, commonKey, Options{SuperuserKey: superKey})
	if err != nil {
		t.Fatal(err)
	}
	server = httptest.NewServer(g.Handler())
	t.Cleanup(func() { server.Close(); _ = g.Close() })
	browser := browserLogin(t, server.URL, "alice", browserPassword)
	var data []byte
	if err := g.db.QueryRow(`SELECT data FROM console_sessions WHERE hash=?`, tokenHash(browser.Cookie.Value)).Scan(&data); err != nil {
		t.Fatal(err)
	}
	var session consoleSession
	if err := json.Unmarshal(data, &session); err != nil || session.Username != "alice" {
		t.Fatal("migration did not preserve browser access", err)
	}
	browser.request(t, http.MethodGet, "/console/api/status", nil, http.StatusOK)
}
