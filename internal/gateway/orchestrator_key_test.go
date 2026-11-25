package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/koltyakov/control/internal/identity"
	"github.com/koltyakov/control/internal/model"
)

type issuedKey struct {
	Key   APIKey `json:"key"`
	Token string `json:"token"`
}

func issueFleetKey(t *testing.T, owner testAdmin, name, role string) issuedKey {
	t.Helper()
	request := map[string]string{"name": name}
	if role != "" {
		request["role"] = role
	}
	var issued issuedKey
	if err := owner.JSON(context.Background(), http.MethodPost, "/v1/fleet/keys", request, &issued); err != nil {
		t.Fatal("issue", name, role, err)
	}
	return issued
}

// Owners issue several execution-only keys in one fleet. Each grants client
// execution authority without fleet administration or machine enrollment.
func TestOrchestratorKeysGrantExecutionOnly(t *testing.T) {
	g, s := installationFixture(t)
	user, account := createTestUser(t, s, "orchestrator-owner")
	ctx := context.Background()

	common := issueFleetKey(t, account, "discovery", "")
	explicitCommon := issueFleetKey(t, account, "discovery-explicit", "common")
	first := issueFleetKey(t, account, "ci", "orchestrator")
	second := issueFleetKey(t, account, "agent", "orchestrator")
	combined := issueFleetKey(t, account, "build-mac", "orchestrator-enroll")
	if common.Key.Role != "common" || explicitCommon.Key.Role != "common" || first.Key.Role != "orchestrator" || second.Key.Role != "orchestrator" || first.Key.UserID != user.ID || first.Key.ID == second.Key.ID {
		t.Fatal("issued key roles", common.Key, explicitCommon.Key, first.Key, second.Key)
	}
	// Owners cannot mint owner authority through the role field.
	for _, role := range []string{"user", "superuser", "Orchestrator", "install", "bogus"} {
		err := account.JSON(ctx, http.MethodPost, "/v1/fleet/keys", map[string]string{"name": "escalate", "role": role}, nil)
		if err == nil || !strings.Contains(err.Error(), "400") {
			t.Fatal("accepted key role", role, err)
		}
	}
	var listed []APIKey
	if err := account.JSON(ctx, http.MethodGet, "/v1/fleet/keys", nil, &listed); err != nil {
		t.Fatal(err)
	}
	roles := map[string]string{}
	for _, key := range listed {
		roles[key.ID] = key.Role
	}
	if roles[first.Key.ID] != "orchestrator" || roles[second.Key.ID] != "orchestrator" || roles[common.Key.ID] != "common" {
		t.Fatal("listed roles", listed)
	}

	orchestrator := testAdmin{URL: s.URL, Key: first.Token}
	var auth struct{ Role, UserID, Username string }
	if err := orchestrator.JSON(ctx, http.MethodGet, "/v1/auth", nil, &auth); err != nil || auth.Role != "orchestrator" || auth.UserID != user.ID || auth.Username != "" {
		t.Fatal("orchestrator authentication", auth, err)
	}
	var nodes []model.Node
	if err := orchestrator.JSON(ctx, http.MethodGet, "/v1/nodes", nil, &nodes); err != nil {
		t.Fatal("orchestrator discovery", err)
	}

	// Execution authority is assigned to client sessions from the credential,
	// never from the proof, for every execution role.
	for _, test := range []struct {
		name, key      string
		supplied, want bool
	}{
		{"common", common.Token, true, false},
		{"orchestrator", first.Token, false, true},
		{"second orchestrator", second.Token, false, true},
		{"orchestrator with enrollment", combined.Token, false, true},
		{"account", account.Key, false, true},
	} {
		id, err := identity.Generate()
		if err != nil {
			t.Fatal(err)
		}
		if ws := rawClientSession(t, s.URL, test.key, id, model.Node{Name: "cli-" + id.ID[:16], ExecutionAuthority: test.supplied}, true); ws == nil {
			t.Fatal(test.name, "client connection rejected")
		}
		g.mu.Lock()
		actual, owner := g.clients[id.ID].ExecutionAuthority, g.clients[id.ID].UserID
		g.mu.Unlock()
		if actual != test.want || owner != user.ID {
			t.Fatal(test.name, "execution authority", actual, test.want, owner)
		}
	}

	// Orchestrator keys never enroll machines, unlike common keys.
	dialCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	ws, resp, err := websocket.Dial(dialCtx, s.URL+"/v1/connect", &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer " + first.Token}}})
	if err == nil {
		_ = ws.CloseNow()
		t.Fatal("orchestrator key opened a machine connection")
	}
	if resp == nil || resp.StatusCode != http.StatusForbidden {
		t.Fatal("orchestrator machine connection", resp, err)
	}
	id, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	if rawPeer(t, s.URL, common.Token, "common-enrolled", user.ID, id) == nil {
		t.Fatal("common key could not enroll a machine")
	}
	g.mu.Lock()
	_, enrolled := g.nodes[id.ID]
	g.mu.Unlock()
	if !enrolled {
		t.Fatal("common key enrollment did not register")
	}
	combinedClient := testAdmin{URL: s.URL, Key: combined.Token}
	combinedID, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	if rawPeer(t, s.URL, combined.Token, "combined-worker", user.ID, combinedID) == nil {
		t.Fatal("combined key could not enroll a machine")
	}
	if err := combinedClient.JSON(ctx, http.MethodGet, "/v1/nodes", nil, &nodes); err != nil {
		t.Fatal("combined key discovery", err)
	}
	g.mu.Lock()
	authority := g.nodes[combinedID.ID].ExecutionAuthority
	g.mu.Unlock()
	if authority {
		t.Fatal("enrolling a worker must not give its worker session execution authority")
	}

	// No fleet or gateway administration, including key issuance.
	forbidden := []struct{ method, path string }{
		{http.MethodPost, "/v1/fleet/keys"},
		{http.MethodGet, "/v1/fleet/keys"},
		{http.MethodPatch, "/v1/fleet/keys/" + second.Key.ID},
		{http.MethodDelete, "/v1/fleet/keys/" + second.Key.ID},
		{http.MethodPost, "/v1/fleet/installations"},
		{http.MethodGet, "/v1/fleet/installations"},
		{http.MethodDelete, "/v1/fleet/installations"},
		{http.MethodPatch, "/v1/fleet/nodes/" + id.ID},
		{http.MethodDelete, "/v1/fleet/nodes/" + id.ID},
		{http.MethodPost, "/v1/fleet/nodes/" + id.ID + "/wake"},
		{http.MethodGet, "/v1/fleet/nodes/" + id.ID + "/secrets"},
		{http.MethodGet, "/v1/audit"},
		{http.MethodDelete, "/v1/audit"},
		{http.MethodPut, "/v1/console/password"},
		{http.MethodPost, "/v1/admin/users"},
		{http.MethodGet, "/v1/admin/keys"},
		{http.MethodPost, "/v1/admin/keys"},
		{http.MethodGet, "/v1/admin/updates"},
		{http.MethodGet, "/v1/admin/metrics"},
	}
	for _, request := range forbidden {
		body := map[string]string{"name": "x", "role": "orchestrator", "os": "linux", "username": "orchestrator-owner", "password": "a long enough password"}
		err := orchestrator.JSON(ctx, request.method, request.path, body, nil)
		if err == nil || !strings.Contains(err.Error(), "403") {
			t.Fatal("orchestrator key reached", request.method, request.path, err)
		}
		if err := combinedClient.JSON(ctx, request.method, request.path, body, nil); err == nil || !strings.Contains(err.Error(), "403") {
			t.Fatal("combined key reached", request.method, request.path, err)
		}
	}
	g.mu.Lock()
	keys, state := len(g.keys), g.machineStates[id.ID]
	g.mu.Unlock()
	if state.Disabled || state.Unregistered {
		t.Fatal("orchestrator key changed machine state", state)
	}

	// Revocation removes authentication from that key only.
	if err := account.JSON(ctx, http.MethodDelete, "/v1/fleet/keys/"+first.Key.ID, nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := orchestrator.JSON(ctx, http.MethodGet, "/v1/auth", nil, nil); err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatal("revoked orchestrator key authenticated", err)
	}
	if err := (testAdmin{URL: s.URL, Key: second.Token}).JSON(ctx, http.MethodGet, "/v1/auth", nil, &auth); err != nil || auth.Role != "orchestrator" {
		t.Fatal("revocation affected another orchestrator key", auth, err)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.keys) != keys {
		t.Fatal("forbidden requests changed keys")
	}
}

// A superuser's key issuance shares the same role contract in the legacy fleet.
func TestOrchestratorKeyRoleSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	g, err := New(dir, commonKey, Options{SuperuserKey: superKey})
	if err != nil {
		t.Fatal(err)
	}
	s := httptest.NewServer(g.Handler())
	var issued issuedKey
	if err := (testAdmin{URL: s.URL, Key: superKey}).JSON(context.Background(), http.MethodPost, "/v1/admin/keys", map[string]string{"name": "ci", "role": "orchestrator"}, &issued); err != nil || issued.Key.Role != "orchestrator" {
		t.Fatal(issued, err)
	}
	s.Close()
	if err := g.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := New(dir, commonKey, Options{SuperuserKey: superKey})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	if role, keyID := reopened.role(issued.Token); role != "orchestrator" || keyID != issued.Key.ID {
		t.Fatal("persisted role", role, keyID)
	}
}
