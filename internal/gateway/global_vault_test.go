package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/koltyakov/control/internal/model"
)

func TestGlobalVaultOperatorBoundaryAndDurability(t *testing.T) {
	const path = "/console/api/admin/vault"
	dir := t.TempDir()
	open := func() (*Gateway, *httptest.Server) {
		t.Helper()
		g, err := New(dir, commonKey, Options{SuperuserKey: superKey})
		if err != nil {
			t.Fatal(err)
		}
		return g, httptest.NewServer(g.Handler())
	}
	g, srv := open()
	defer func() { srv.Close(); _ = g.Close() }()
	user, key := createTestUser(t, srv, "console-owner")
	provisionBrowser(t, key, "")
	provisionBrowser(t, testAdmin{URL: srv.URL, Key: superKey}, "")
	operator := browserLogin(t, srv.URL, "@operator", browserPassword)
	owner := browserLogin(t, srv.URL, user.Name, browserPassword)
	owner.request(t, "GET", path, nil, http.StatusForbidden)
	q := map[string]any{"enabled": true, "revision": 0}
	owner.request(t, "PUT", path, q, http.StatusForbidden)
	noCSRF := operator
	noCSRF.CSRF = ""
	noCSRF.request(t, "PUT", path, q, http.StatusForbidden)
	if err := (testAdmin{URL: srv.URL, Key: superKey}).JSON(t.Context(), "PUT", path, q, nil); err == nil {
		t.Fatal("bearer-only request changed the master lock")
	}
	// An incapable worker in *another* tenant blocks activation.
	g.mu.Lock()
	g.nodes["old"] = model.Node{ID: "old", UserID: user.ID}
	g.mu.Unlock()
	operator.request(t, "PUT", path, q, http.StatusConflict)
	g.mu.Lock()
	delete(g.nodes, "old")
	g.mu.Unlock()

	q["pauseSeconds"] = 3600
	body, _ := operator.request(t, "PUT", path, q, http.StatusOK)
	var p globalVaultState
	if err := json.Unmarshal(body, &p); err != nil || p.Revision != 1 || !p.Enabled || p.active(time.Now()) || !p.active(p.PausedUntil) {
		t.Fatalf("unexpected paused policy %s: %v", body, err)
	}
	operator.request(t, "PUT", path, q, http.StatusConflict) // Stale browser tab.
	srv.Close()
	if err := g.Close(); err != nil {
		t.Fatal(err)
	}
	g, srv = open()
	operator.URL = srv.URL
	got, _ := operator.request(t, "GET", path, nil, http.StatusOK)
	var restored globalVaultState
	if err := json.Unmarshal(got, &restored); err != nil || restored != p {
		t.Fatalf("restart changed the lock deadline: %s", got)
	}
	// An audit insert failure must leave the old policy enforced.
	if _, err := g.db.Exec(`CREATE TRIGGER refuse_vault_audit BEFORE INSERT ON audit_events WHEN NEW.action='vault.gateway' BEGIN SELECT RAISE(ABORT,'fixture'); END`); err != nil {
		t.Fatal(err)
	}
	q = map[string]any{"revision": p.Revision, "enabled": false}
	operator.request(t, "PUT", path, q, http.StatusInternalServerError)
	got, _ = operator.request(t, "GET", path, nil, http.StatusOK)
	if err := json.Unmarshal(got, &restored); err != nil || restored != p {
		t.Fatal("failed commit changed the master lock")
	}
	// Simulate a delayed PUT whose headers passed authentication before the
	// operator signed out, but whose body did not arrive until afterward.
	request := httptest.NewRequest("PUT", srv.URL+path, strings.NewReader(`{"enabled":false,"revision":1}`))
	request.RemoteAddr = "127.0.0.1:12345"
	request.AddCookie(operator.Cookie)
	request.Header.Set("Origin", srv.URL)
	request.Header.Set("X-CSRF-Token", operator.CSRF)
	writer := httptest.NewRecorder()
	g.consoleGuard(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		operator.request(t, "POST", "/console/api/logout", nil, http.StatusNoContent)
		g.globalVaultHTTP(w, r)
	})).ServeHTTP(writer, request)
	if writer.Code != http.StatusUnauthorized {
		t.Fatalf("revoked browser session at policy commit: got %d, want 401", writer.Code)
	}
}

func TestOperatorAndFleetVaultCompose(t *testing.T) {
	g, srv := installationFixture(t)
	root := testAdmin{URL: srv.URL, Key: superKey}
	user, key := createTestUser(t, srv, "build-fleet")
	provisionBrowser(t, root, "")
	provisionBrowser(t, key, "")
	operator := browserLogin(t, srv.URL, "@operator", browserPassword)
	owner := browserLogin(t, srv.URL, user.Name, browserPassword)
	const master, fleet = "/console/api/admin/vault", "/console/api/fleet/vault"
	g.mu.Lock()
	g.nodes["worker"] = model.Node{ID: "worker", UserID: user.ID, FleetVaultCheck: true}
	g.nodes["legacy-worker"] = model.Node{ID: "legacy-worker", UserID: legacyUser, FleetVaultCheck: true}
	g.mu.Unlock()

	verify := func(fleetAllows, legacyAllows bool) {
		t.Helper()
		// Without an issuing client, discovery can pass only if both locks
		// are inactive. Each authenticated worker check sees its own fleet.
		for _, test := range []struct {
			client testAdmin
			id     string
			want   bool
		}{{key, "worker", fleetAllows}, {root, "legacy-worker", legacyAllows}} {
			var decision struct{ Allowed *bool }
			err := test.client.JSON(t.Context(), "POST", "/v1/fleet/vault/check", map[string]any{"machineIds": []string{test.id}}, &decision)
			if err != nil || decision.Allowed == nil || *decision.Allowed != test.want {
				t.Fatalf("wanted access %t; decision=%v, err=%v", test.want, decision.Allowed, err)
			}
		}
	}
	verify(true, true)
	owner.request(t, "PUT", fleet, map[string]any{"revision": 0, "enabled": true}, http.StatusOK)
	verify(false, true)
	operator.request(t, "PUT", master, map[string]any{"revision": 0, "enabled": true}, http.StatusOK)
	verify(false, false)
	owner.request(t, "PUT", fleet, map[string]any{"revision": 1, "enabled": false}, http.StatusOK)
	verify(false, false)
	operator.request(t, "PUT", master, map[string]any{"revision": 1, "enabled": true, "pauseSeconds": 3600}, http.StatusOK)
	verify(true, true)
	owner.request(t, "PUT", fleet, map[string]any{"revision": 2, "enabled": true}, http.StatusOK)
	verify(false, true)
}
