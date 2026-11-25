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
	_, a := createTestUser(t, s, "builder")
	const path = "/v1/fleet/keys/issue"
	type listedKey struct {
		ID          string
		Permissions []string
	}
	for _, invalid := range [][]string{nil, {}, {"run"}, {"discover", "discover"}, {"discover", "manage-fleet"}, {"discover", "unknown"}} {
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
		if tc.role == roleDiscovery {
			for _, check := range []struct {
				method, path string
				status       int
			}{
				{"GET", "/v1/nodes", 200}, {"GET", "/v1/connect", 403}, {"GET", "/v1/fleet/keys", 403},
			} {
				if code, _ := vaultHTTP(t, s.URL, issued.Token, check.method, check.path, nil); code != check.status {
					t.Fatalf("%s: got %d, want %d", check.path, code, check.status)
				}
			}
			id, err := identity.Generate()
			if err != nil {
				t.Fatal(err)
			}
			if rawClientSession(t, s.URL, issued.Token, id, model.Node{Name: "cli-" + id.ID[:16]}, true) == nil {
				t.Fatal("discovery client failed")
			}
			g.mu.Lock()
			exec := g.clients[id.ID].ExecutionAuthority
			g.mu.Unlock()
			if exec {
				t.Fatal("discovery client has execution permission")
			}
		}
	}
}
