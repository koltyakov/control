package gateway

import (
	"encoding/json"
	"net/http"
	"slices"
	"testing"

	"github.com/koltyakov/control/internal/identity"
	"github.com/koltyakov/control/internal/model"
)

func TestAccountKeyRecoveryAndRemoval(t *testing.T) {
	g, s := installationFixture(t)
	alice, a := createTestUser(t, s, "alice")
	_, b := createTestUser(t, s, "bob")
	provisionBrowser(t, a, "")
	provisionBrowser(t, b, "")
	owner := browserLogin(t, s.URL, "alice", browserPassword)
	other := browserLogin(t, s.URL, "bob", browserPassword)
	const issue = "/console/api/fleet/account-key"
	id := accountKeyID(t, g, alice.ID)
	path := "/console/api/fleet/keys/" + id
	owner.request(t, "PATCH", path, map[string]string{"name": "Admin laptop"}, 200)
	if p := g.authenticate(a.Key); p.KeyID != id || p.UserID != alice.ID || p.Username != "alice" {
		t.Fatal("rename changed the account identity")
	}
	owner.request(t, "POST", issue, nil, 409)
	owner.request(t, "DELETE", path+"/record", nil, 409)
	owner.request(t, "DELETE", path, nil, 204)
	if g.authenticate(a.Key).Role != "" {
		t.Fatal("old account key still authenticates")
	}
	other.request(t, "POST", path+"/reactivate", nil, 404)
	other.request(t, "DELETE", path+"/record", nil, 404)
	badCSRF := owner
	badCSRF.CSRF = "invalid"
	badCSRF.request(t, "POST", issue, nil, 403)
	badCSRF.request(t, "POST", path+"/reactivate", nil, 403)
	for _, target := range []string{issue, path + "/reactivate"} {
		if status, _ := vaultHTTP(t, s.URL, b.Key, "POST", target, nil); status != http.StatusUnauthorized {
			t.Fatalf("bearer at browser-only endpoint: %d", status)
		}
	}
	owner.request(t, "POST", path+"/reactivate", nil, 204)
	if p := g.authenticate(a.Key); p.KeyID != id || p.Role != roleUser {
		t.Fatal("reactivation did not restore the original key")
	}
	owner.request(t, "DELETE", path, nil, 204)
	data, _ := owner.request(t, "POST", issue, nil, 200)
	var issued issuedKey
	if err := json.Unmarshal(data, &issued); err != nil || issued.Key.ID == id || issued.Token == a.Key {
		t.Fatal("replacement must have a new ID and secret", err)
	}
	if p := g.authenticate(issued.Token); p.UserID != alice.ID || p.Role != roleUser || p.KeyID != issued.Key.ID {
		t.Fatal("invalid replacement credentials")
	}
	owner.request(t, "POST", path+"/reactivate", nil, 409)
	owner.request(t, "DELETE", path+"/record", nil, 204)
	owner.request(t, "POST", path+"/reactivate", nil, 404)
	var stored, audited int
	if err := g.db.QueryRow("SELECT count(*) FROM credentials WHERE id=?", id).Scan(&stored); err != nil || stored != 0 {
		t.Fatal("revoked record not removed from disk", err)
	}
	if err := g.db.QueryRow(`SELECT count(*) FROM audit_events WHERE user_id=? AND action IN ('key.revoke','key.reactivate','key.remove') AND json_extract(data,'$.targetId')=?`, alice.ID, id).Scan(&audited); err != nil || audited != 4 {
		t.Fatal("missing lifecycle history", audited, err)
	}
}

func TestNamedKeyPermissionLists(t *testing.T) {
	g, s := installationFixture(t)
	owner, a := createTestUser(t, s, "builder")
	const path = "/v1/fleet/keys/issue"
	type listedKey struct {
		ID          string
		Permissions []string
	}
	for _, invalid := range [][]string{nil, {}, {"run"}, {"discover", "discover"}, {"discover", "manage-gateway"}, {"discover", "unknown"}} {
		if code, _ := vaultHTTP(t, s.URL, a.Key, "POST", path, map[string]any{"name": "invalid", "permissions": invalid}); code != 400 {
			t.Fatalf("permissions %v: %d", invalid, code)
		}
	}
	for _, tc := range []struct {
		permissions []string
		role        string
	}{
		{[]string{"discover"}, roleDiscovery},
		{[]string{"discover", "run"}, roleOrchestrator},
		{[]string{"discover", "enroll"}, roleCommon},
		{[]string{"discover", "enroll", "run"}, roleOrchestratorEnroll},
		{[]string{"discover", "manage-fleet"}, "fleet-manager"},
		{[]string{"discover", "enroll", "manage-fleet"}, "fleet-manager-enroll"},
		{[]string{"discover", "run", "manage-fleet"}, "fleet-manager-run"},
		{[]string{"discover", "enroll", "run", "manage-fleet"}, "fleet-manager-enroll-run"},
	} {
		var issued issuedKey
		if err := a.JSON(t.Context(), "POST", path, map[string]any{"name": "test", "permissions": tc.permissions}, &issued); err != nil || issued.Key.Role != tc.role {
			t.Fatal("permission issuance", tc.permissions, err)
		}
		var list []listedKey
		if err := a.JSON(t.Context(), "GET", "/v1/fleet/keys", nil, &list); err != nil || !slices.ContainsFunc(list, func(k listedKey) bool {
			return k.ID == issued.Key.ID && slices.Equal(k.Permissions, tc.permissions)
		}) {
			t.Fatal("listed permissions differ for", tc.permissions, err)
		}
		target := func(path string, want int, permissions ...string) {
			t.Helper()
			code, _ := vaultHTTP(t, s.URL, issued.Token, "GET", path, nil)
			if len(permissions) > 0 && !slices.Contains(tc.permissions, permissions[0]) {
				want = 403
			}
			if code != want {
				t.Fatalf("%s for %s: got %d, want %d", path, tc.role, code, want)
			}
		}
		target("/v1/nodes", 200)
		target("/v1/fleet/keys", 200, "manage-fleet")
		target("/v1/admin/users", 403)
		id, err := identity.Generate()
		if err != nil {
			t.Fatal(err)
		}
		if rawClientSession(t, s.URL, issued.Token, id, model.Node{Name: "cli-" + id.ID[:16]}, true) == nil {
			t.Fatal("client session failed")
		}
		g.mu.Lock()
		exec := g.clients[id.ID].ExecutionAuthority
		g.mu.Unlock()
		if exec != slices.Contains(tc.permissions, "run") {
			t.Fatal("wrong execution permission", tc.role)
		}
		worker, err := identity.Generate()
		if err != nil {
			t.Fatal(err)
		}
		if slices.Contains(tc.permissions, "enroll") {
			if rawPeer(t, s.URL, issued.Token, "worker-"+worker.ID[:12], owner.ID, worker) == nil {
				t.Fatal("registration failed", tc.role)
			}
		} else {
			target("/v1/connect", 403)
		}
	}
}

func TestManagerCannotExposeOtherPermissionsOrManageAccounts(t *testing.T) {
	g, s := installationFixture(t)
	alice, a := createTestUser(t, s, "avery")
	bob, _ := createTestUser(t, s, "ben")
	var manager issuedKey
	if err := a.JSON(t.Context(), "POST", "/v1/fleet/keys/issue", map[string]any{"name": "Mari", "permissions": []string{"discover", "manage-fleet"}}, &manager); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		method, target string
		body           any
		want           int
	}{
		{"GET", "/v1/audit", nil, 200},
		{"GET", "/v1/fleet/installations", nil, 200},
		{"POST", "/v1/fleet/installations", nil, 403},
		{"POST", "/v1/fleet/keys", map[string]string{"name": "extra", "role": "orchestrator"}, 403},
		{"POST", "/v1/fleet/keys", map[string]string{"name": "extra"}, 403},
		{"POST", "/v1/fleet/keys/issue", map[string]any{"name": "extra", "permissions": []string{"discover", "run", "manage-fleet"}}, 403},
		{"PATCH", "/v1/fleet/keys/" + accountKeyID(t, g, alice.ID), map[string]string{"name": "overwritten"}, 403},
		{"DELETE", "/v1/fleet/keys/" + accountKeyID(t, g, bob.ID), nil, 404},
		{"PUT", "/v1/console/password", map[string]string{"password": "would otherwise be valid"}, 403},
		{"POST", "/console/api/fleet/account-key", nil, 401},
		{"POST", "/v1/fleet/keys/issue", map[string]any{"name": "sibling", "permissions": []string{"discover", "manage-fleet"}}, 200},
	} {
		code, _ := vaultHTTP(t, s.URL, manager.Token, test.method, test.target, test.body)
		if code != test.want {
			t.Fatalf("%s %s = %d; want %d", test.method, test.target, code, test.want)
		}
	}
}

func TestKeyLastUsedSurvivesRevocationAndRestart(t *testing.T) {
	dir := t.TempDir()
	g, s := gatewayAt(t, dir)
	_, fleet := createTestUser(t, s, "owner")
	key := issueFleetKey(t, fleet, "access test", "orchestrator")
	idURL := "/v1/fleet/keys/" + key.Key.ID
	lastUsed := func(keyID string) int64 {
		t.Helper()
		var rows []APIKey
		if err := fleet.JSON(t.Context(), "GET", "/v1/fleet/keys", nil, &rows); err != nil {
			t.Fatal(err)
		}
		for _, r := range rows {
			if r.ID == keyID {
				if r.LastUsed.IsZero() {
					return 0
				}
				return r.LastUsed.UnixMilli()
			}
		}
		t.Fatal("key missing", keyID)
		return 0
	}
	if lastUsed(key.Key.ID) != 0 {
		t.Fatal("unused key has a last-used timestamp")
	}
	if status, _ := vaultHTTP(t, s.URL, "wrong-secret", "GET", "/v1/status", nil); status != 401 {
		t.Fatal(status)
	}
	if lastUsed(key.Key.ID) != 0 {
		t.Fatal("invalid credential updated the timestamp")
	}
	if status, _ := vaultHTTP(t, s.URL, key.Token, "GET", "/v1/status", nil); status != 200 {
		t.Fatal(status)
	}
	first := lastUsed(key.Key.ID)
	if first == 0 {
		t.Fatal("key timestamp missing")
	}
	if err := fleet.JSON(t.Context(), "DELETE", idURL, nil, nil); err != nil {
		t.Fatal(err)
	}
	if status, _ := vaultHTTP(t, s.URL, key.Token, "GET", "/v1/status", nil); status != 401 {
		t.Fatal("revocation failed")
	}
	if lastUsed(key.Key.ID) != first {
		t.Fatal("authentication with a revoked key updated its timestamp")
	}
	s.Close()
	if err := g.Close(); err != nil {
		t.Fatal(err)
	}
	g2, s2 := gatewayAt(t, dir)
	defer func() { s2.Close(); _ = g2.Close() }()
	fleet.URL = s2.URL
	if lastUsed(key.Key.ID) != first {
		t.Fatal("last-used timestamp changed across restart")
	}
}
