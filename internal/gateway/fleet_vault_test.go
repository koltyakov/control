package gateway

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/koltyakov/control/internal/enrollment"
	"github.com/koltyakov/control/internal/identity"
	"github.com/koltyakov/control/internal/model"
)

// vaultWorker registers a machine and reports whether the gateway accepted it.
func vaultWorker(t *testing.T, serverURL, token, name string, check bool) (*identity.Identity, bool) {
	t.Helper()
	id, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(ctx, serverURL+"/v1/connect", &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer " + token}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ws.CloseNow() })
	_, challenge, err := ws.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	n := model.Node{ID: id.ID, PublicKey: id.Public, Name: name, FleetVaultCheck: check}
	hello := Hello{Version: model.Version, NodeBytes: model.JSON(n)}
	hello.Signature = ed25519.Sign(id.Private, hello.Message(challenge))
	if err = ws.Write(ctx, websocket.MessageBinary, model.JSON(hello)); err != nil {
		t.Fatal(err)
	}
	_, ready, err := ws.Read(ctx)
	return id, err == nil && string(ready) == "ready"
}

// vaultHTTP sends a bearer request and returns its status and body.
func vaultHTTP(t *testing.T, url, key, method, path string, body any) (int, []byte) {
	t.Helper()
	var data []byte
	switch b := body.(type) {
	case nil:
	case string:
		data = []byte(b)
	default:
		data = model.JSON(b)
	}
	r, err := http.NewRequest(method, url+path, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if key != "" {
		r.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	result, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, result
}

func vaultCheck(t *testing.T, url, key string, q model.FleetVaultCheck) model.FleetVaultDecision {
	t.Helper()
	status, body := vaultHTTP(t, url, key, "POST", model.FleetVaultCheckPath, q)
	if status != http.StatusOK {
		t.Fatalf("check %+v: HTTP %d: %s", q, status, body)
	}
	var d model.FleetVaultDecision
	if err := json.Unmarshal(body, &d); err != nil {
		t.Fatal(err)
	}
	return d
}

func vaultView(t *testing.T, data []byte) fleetVaultView {
	t.Helper()
	var v fleetVaultView
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatalf("view %s: %v", data, err)
	}
	return v
}

func accountKeyID(t *testing.T, g *Gateway, user string) string {
	t.Helper()
	g.mu.Lock()
	defer g.mu.Unlock()
	for id, k := range g.keys {
		if k.UserID == user && k.Role == roleUser {
			return id
		}
	}
	t.Fatal("no account key")
	return ""
}

func fleetKey(t *testing.T, a testAdmin, role string) (string, string) {
	t.Helper()
	var issued struct {
		Key   APIKey
		Token string
	}
	if err := a.JSON(context.Background(), "POST", "/v1/fleet/keys", map[string]string{"name": role + " key", "role": role}, &issued); err != nil {
		t.Fatal(err)
	}
	return issued.Key.ID, issued.Token
}

func TestFleetDecisionSelectorsAndCoverage(t *testing.T) {
	now := time.Now()
	p := fleetVaultPolicy{Allowances: []fleetVaultAllowance{
		{ID: "both", KeyID: "k1", MachineIDs: []string{"a", "b"}},
		{ID: "key", KeyID: "k2", AllMachines: true},
		{ID: "expired", KeyID: "k3", AllMachines: true, ExpiresAt: now.Add(-time.Second)},
		{ID: "unscoped", AllMachines: true},
	}}
	for _, c := range []struct {
		key      string
		machines []string
		want     bool
	}{
		{"k1", []string{"a", "b"}, true},
		{"", []string{"a"}, false},
		{"k1", []string{"a", "c"}, false},
		{"k2", []string{"x", "y"}, true},
		{"k3", []string{"a"}, false},
		{"k9", []string{"a"}, false},
		{"k2", nil, false},
	} {
		if got := fleetDecision(p, c.key, c.machines, now); got != c.want {
			t.Errorf("fleetDecision(%q, %v) = %v, want %v", c.key, c.machines, got, c.want)
		}
	}
	if !(fleetVaultPolicy{Enabled: true}).locked(now) || (fleetVaultPolicy{Enabled: true, PausedUntil: now.Add(time.Minute)}).locked(now) || !(fleetVaultPolicy{Enabled: true, PausedUntil: now}).locked(now) {
		t.Fatal("a pause must end on its own")
	}
}

// The console owns the policy: browser cookie, same origin and CSRF only;
// enabling needs every registered worker to enforce it; revisions order
// edits; pause resumes automatically; edits are audited.
func TestFleetVaultConsolePolicy(t *testing.T) {
	g, s := installationFixture(t)
	alice, a := createTestUser(t, s, "alice")
	provisionBrowser(t, a, "")
	browser := browserLogin(t, s.URL, "alice", browserPassword)
	old, ok := vaultWorker(t, s.URL, a.Key, "old", false)
	if !ok {
		t.Fatal("unlocked fleet rejected a worker")
	}
	data, _ := browser.request(t, "GET", "/console/api/fleet/vault", nil, 200)
	v := vaultView(t, data)
	if v.Enabled || v.Revision != 0 || len(v.Allowances) != 0 || len(v.Machines) != 1 || v.Machines[0].ID != old.ID || len(v.Keys) != 1 || v.Keys[0].ID != accountKeyID(t, g, alice.ID) || strings.Contains(string(data), `"hosts"`) {
		t.Fatalf("default view: %s", data)
	}
	// No bearer route reads or changes the lock.
	for _, path := range []string{"/console/api/fleet/vault", "/v1/fleet/vault"} {
		if status, _ := vaultHTTP(t, s.URL, a.Key, "GET", path, nil); status == http.StatusOK {
			t.Fatal("bearer reached lock policy", path)
		}
	}
	forged := browser
	forged.CSRF = "wrong"
	forged.request(t, "PUT", "/console/api/fleet/vault", map[string]any{"revision": 0, "enabled": true}, 403)
	body, _ := browser.request(t, "PUT", "/console/api/fleet/vault", map[string]any{"revision": 0, "enabled": true}, 409)
	if !strings.Contains(string(body), "old") {
		t.Fatalf("refusal must name the old worker: %s", body)
	}
	if err := a.JSON(context.Background(), "DELETE", "/v1/fleet/nodes/"+old.ID+"?stop=true", nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, ok := vaultWorker(t, s.URL, a.Key, "current", true); !ok {
		t.Fatal("supporting worker rejected")
	}
	data, _ = browser.request(t, "PUT", "/console/api/fleet/vault", map[string]any{"revision": 0, "enabled": true}, 200)
	if v = vaultView(t, data); !v.Enabled || v.Revision != 1 || !v.PausedUntil.IsZero() {
		t.Fatalf("enabled view: %s", data)
	}
	browser.request(t, "PUT", "/console/api/fleet/vault", map[string]any{"revision": 0, "enabled": false}, 409)
	if _, ok := vaultWorker(t, s.URL, a.Key, "old2", false); ok {
		t.Fatal("non-supporting worker registered under a lock")
	}
	for _, bad := range []string{
		`{"revision":1,"enabled":false,"pauseSeconds":60}`,
		`{"revision":1,"enabled":true,"pauseSeconds":-1}`,
		`{"revision":1,"enabled":true,"pauseSeconds":1.5}`,
		`{"revision":1,"enabled":true,"pauseSeconds":1e30}`,
		`{"revision":1,"enabled":true,"pauseSeconds":31536001}`,
		`{"revision":1}`,
		`{"revision":1,"enabled":true,"extra":1}`,
	} {
		raw := browser
		r, _ := http.NewRequest("PUT", s.URL+"/console/api/fleet/vault", strings.NewReader(bad))
		r.Header.Set("Origin", raw.URL)
		r.Header.Set("X-CSRF-Token", raw.CSRF)
		r.AddCookie(raw.Cookie)
		resp, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("%s: HTTP %d", bad, resp.StatusCode)
		}
	}
	before := time.Now()
	data, _ = browser.request(t, "PUT", "/console/api/fleet/vault", map[string]any{"revision": 1, "enabled": true, "pauseSeconds": 3600}, 200)
	v = vaultView(t, data)
	if !v.Enabled || v.Revision != 2 || v.PausedUntil.Before(before.Add(3600*time.Second)) || v.PausedUntil.Location() != time.UTC {
		t.Fatalf("paused view: %s", data)
	}
	if _, ok := vaultWorker(t, s.URL, a.Key, "old3", false); ok {
		t.Fatal("a paused lock must still reject non-supporting workers")
	}
	g.mu.Lock()
	enforced := g.fleetVaultEnforcedLocked(alice.ID, time.Now())
	resumed := g.fleetVaultEnforcedLocked(alice.ID, time.Now().Add(3601*time.Second))
	g.mu.Unlock()
	if enforced || !resumed {
		t.Fatal("pause must suspend enforcement and resume on its own", enforced, resumed)
	}
	browser.request(t, "PUT", "/console/api/fleet/vault", map[string]any{"revision": 2, "enabled": false}, 200)
	if countAudit(t, g, "vault.lock") != 1 || countAudit(t, g, "vault.pause") != 1 || countAudit(t, g, "vault.unlock") != 1 {
		t.Fatal("lock changes must be audited")
	}
	page := readAudit(t, a, "category=vault")
	if len(page.Events) != 3 {
		t.Fatal("vault audit category", page.Events)
	}
}

func TestFleetVaultAllowanceValidation(t *testing.T) {
	g, s := installationFixture(t)
	alice, a := createTestUser(t, s, "alice")
	_, b := createTestUser(t, s, "bob")
	provisionBrowser(t, a, "")
	browser := browserLogin(t, s.URL, "alice", browserPassword)
	w1, _ := vaultWorker(t, s.URL, a.Key, "w1", true)
	foreign, _ := vaultWorker(t, s.URL, b.Key, "w1", true)
	owner, _ := vaultClient(t, s.URL, a.Key)
	key := accountKeyID(t, g, alice.ID)
	common, _ := fleetKey(t, a, "common")
	orchestrator, _ := fleetKey(t, a, "orchestrator")
	for _, bad := range []map[string]any{
		{"revision": 0, "allMachines": true},
		{"revision": 0, "keyId": key},
		{"revision": 0, "keyId": key, "allMachines": true, "machineIds": []string{w1.ID}},
		{"revision": 0, "keyId": key, "machineIds": []string{w1.ID, w1.ID}},
		{"revision": 0, "keyId": key, "machineIds": []string{foreign.ID}},
		{"revision": 0, "keyId": common, "allMachines": true},
		{"revision": 0, "keyId": "superuser", "allMachines": true},
		{"revision": 0, "ownerId": owner.ID, "allMachines": true},
		{"revision": 0, "ownerId": owner.ID, "keyId": key, "allMachines": true},
		{"revision": 0, "keyId": key, "allMachines": true, "ttlSeconds": -1},
		{"revision": 0, "keyId": key, "allMachines": true, "ttlSeconds": 31536001},
	} {
		browser.request(t, "POST", "/console/api/fleet/vault/allowances", bad, 400)
	}
	browser.request(t, "POST", "/console/api/fleet/vault/allowances", map[string]any{"revision": 5, "keyId": key, "allMachines": true}, 409)
	data, _ := browser.request(t, "POST", "/console/api/fleet/vault/allowances", map[string]any{"revision": 0, "keyId": orchestrator, "machineIds": []string{w1.ID}, "ttlSeconds": 60}, 201)
	v := vaultView(t, data)
	if v.Revision != 1 || len(v.Allowances) != 1 || v.Allowances[0].KeyID != orchestrator || v.Allowances[0].ExpiresAt.IsZero() || strings.Contains(string(data), owner.ID) || len(v.Keys) != 2 {
		t.Fatalf("allowance view: %s", data)
	}
	id := v.Allowances[0].ID
	browser.request(t, "DELETE", "/console/api/fleet/vault/allowances/"+id+"?revision=0", nil, 409)
	browser.request(t, "DELETE", "/console/api/fleet/vault/allowances/missing?revision=1", nil, 404)
	data, _ = browser.request(t, "DELETE", "/console/api/fleet/vault/allowances/"+id, map[string]any{"revision": 1}, 200)
	if v = vaultView(t, data); v.Revision != 2 || len(v.Allowances) != 0 {
		t.Fatalf("after removal: %s", data)
	}
	if countAudit(t, g, "vault.allow") != 1 || countAudit(t, g, "vault.revoke") != 1 {
		t.Fatal("allowance changes must be audited")
	}
	// The legacy operator fleet selects the superuser key.
	provisionBrowser(t, testAdmin{URL: s.URL, Key: superKey}, "")
	operator := browserLogin(t, s.URL, "@operator", browserPassword)
	data, _ = operator.request(t, "GET", "/console/api/fleet/vault", nil, 200)
	if v = vaultView(t, data); len(v.Keys) != 1 || v.Keys[0].ID != superuserKeyID {
		t.Fatalf("operator keys: %s", data)
	}
	operator.request(t, "POST", "/console/api/fleet/vault/allowances", map[string]any{"revision": 0, "keyId": superuserKeyID, "allMachines": true}, 201)
}

// vaultClient opens an owned account-client session and returns its stable
// owner and transport identities.
func vaultClient(t *testing.T, serverURL, token string) (*identity.Identity, *identity.Identity) {
	t.Helper()
	owner, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	transport, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	if rawOwnedClientSession(t, serverURL, token, transport, model.Node{Name: "cli-" + owner.ID[:16]}, true, owner, nil) == nil {
		t.Fatal("client session rejected")
	}
	return owner, transport
}

func TestFleetVaultCheckDecisions(t *testing.T) {
	g, s := installationFixture(t)
	alice, a := createTestUser(t, s, "alice")
	_, b := createTestUser(t, s, "bob")
	provisionBrowser(t, a, "")
	browser := browserLogin(t, s.URL, "alice", browserPassword)
	w1, _ := vaultWorker(t, s.URL, a.Key, "w1", true)
	w2, _ := vaultWorker(t, s.URL, a.Key, "w2", true)
	foreign, _ := vaultWorker(t, s.URL, b.Key, "w1", true)
	_, worker := fleetKey(t, a, "common")
	common, commonToken := fleetKey(t, a, "common")
	orchestrator, orchestratorToken := fleetKey(t, a, "orchestrator-enroll")
	key := accountKeyID(t, g, alice.ID)
	owner, transport := vaultClient(t, s.URL, a.Key)
	m1, both := []string{w1.ID}, []string{w1.ID, w2.ID}

	d := vaultCheck(t, s.URL, worker, model.FleetVaultCheck{Issuer: transport.ID, MachineIDs: m1})
	if !d.Allowed || d.KeyID != key || d.OwnerID != owner.ID {
		t.Fatalf("unlocked fleet resolves and allows: %+v", d)
	}
	if d = vaultCheck(t, s.URL, worker, model.FleetVaultCheck{MachineIDs: m1}); !d.Allowed {
		t.Fatal("unlocked fleet allows unknown issuers")
	}
	if status, _ := vaultHTTP(t, s.URL, worker, "POST", model.FleetVaultCheckPath, model.FleetVaultCheck{Issuer: transport.ID, KeyID: key, MachineIDs: m1}); status != http.StatusBadRequest {
		t.Fatal("issuer and provenance together are ambiguous", status)
	}
	if status, _ := vaultHTTP(t, s.URL, worker, "POST", model.FleetVaultCheckPath, model.FleetVaultCheck{Issuer: transport.ID}); status != http.StatusBadRequest {
		t.Fatal("machines are required", status)
	}
	if status, _ := vaultHTTP(t, s.URL, "", "POST", model.FleetVaultCheckPath, model.FleetVaultCheck{Issuer: transport.ID, MachineIDs: m1}); status != http.StatusUnauthorized {
		t.Fatal("check requires a fleet credential", status)
	}

	browser.request(t, "PUT", "/console/api/fleet/vault", map[string]any{"revision": 0, "enabled": true}, 200)
	if d = vaultCheck(t, s.URL, worker, model.FleetVaultCheck{Issuer: transport.ID, MachineIDs: m1}); d.Allowed || d.KeyID != key || d.OwnerID != owner.ID {
		t.Fatalf("no allowance denies but resolves: %+v", d)
	}
	browser.request(t, "POST", "/console/api/fleet/vault/allowances", map[string]any{"revision": 1, "keyId": key, "machineIds": m1}, 201)
	for _, c := range []struct {
		q    model.FleetVaultCheck
		want bool
	}{
		{model.FleetVaultCheck{Issuer: transport.ID, MachineIDs: m1}, true},
		{model.FleetVaultCheck{Issuer: transport.ID, MachineIDs: both}, false}, // One rule must cover every machine.
		{model.FleetVaultCheck{Issuer: owner.ID, MachineIDs: m1}, false},       // The owner is not connected itself.
		{model.FleetVaultCheck{Issuer: w1.ID, MachineIDs: m1}, false},          // Machines are never issuers.
		{model.FleetVaultCheck{MachineIDs: m1}, false},                         // No issuer at all.
		{model.FleetVaultCheck{KeyID: key, OwnerID: owner.ID, MachineIDs: m1}, true},
		{model.FleetVaultCheck{OwnerID: owner.ID, MachineIDs: m1}, false},
		{model.FleetVaultCheck{KeyID: key, MachineIDs: m1}, false},
		{model.FleetVaultCheck{KeyID: key, OwnerID: transport.ID, MachineIDs: m1}, false},
		{model.FleetVaultCheck{Issuer: transport.ID, MachineIDs: []string{w1.ID, foreign.ID}}, false},
		{model.FleetVaultCheck{Issuer: transport.ID, MachineIDs: []string{"unknown"}}, false},
	} {
		if d := vaultCheck(t, s.URL, worker, c.q); d.Allowed != c.want {
			t.Errorf("check %+v = %+v, want %v", c.q, d, c.want)
		}
	}
	// Another fleet's worker cannot learn or use this fleet's state.
	if d = vaultCheck(t, s.URL, b.Key, model.FleetVaultCheck{Issuer: transport.ID, MachineIDs: []string{foreign.ID}}); !d.Allowed || d.KeyID != "" || d.OwnerID != "" {
		t.Fatalf("foreign issuer must stay unresolved: %+v", d)
	}

	// A second orchestrator using the same key gets the same allowance.
	otherOwner, otherTransport := vaultClient(t, s.URL, a.Key)
	if d = vaultCheck(t, s.URL, worker, model.FleetVaultCheck{Issuer: otherTransport.ID, MachineIDs: m1}); !d.Allowed || d.OwnerID != otherOwner.ID {
		t.Fatal("shared key did not admit the second orchestrator", d)
	}
	for _, q := range []model.FleetVaultCheck{
		{KeyID: orchestrator, OwnerID: owner.ID, MachineIDs: both},
		{KeyID: common, OwnerID: owner.ID, MachineIDs: both},
		{OwnerID: owner.ID, MachineIDs: both},
	} {
		if d = vaultCheck(t, s.URL, worker, q); d.Allowed {
			t.Errorf("unallowed key %+v = %+v", q, d)
		}
	}
	_, commonTransport := vaultClient(t, s.URL, commonToken)
	if d = vaultCheck(t, s.URL, worker, model.FleetVaultCheck{Issuer: commonTransport.ID, MachineIDs: m1}); d.Allowed || d.KeyID != "" {
		t.Fatalf("discovery-only client allowed: %+v", d)
	}
	// An orchestrator session passes; after revocation its stored provenance does not.
	orchOwner, orchTransport := vaultClient(t, s.URL, orchestratorToken)
	browser.request(t, "POST", "/console/api/fleet/vault/allowances", map[string]any{"revision": 2, "keyId": orchestrator, "allMachines": true}, 201)
	if d = vaultCheck(t, s.URL, worker, model.FleetVaultCheck{Issuer: orchTransport.ID, MachineIDs: both}); !d.Allowed || d.KeyID != orchestrator || d.OwnerID != orchOwner.ID {
		t.Fatalf("orchestrator session: %+v", d)
	}
	if err := a.JSON(context.Background(), "DELETE", "/v1/fleet/keys/"+orchestrator, nil, nil); err != nil {
		t.Fatal(err)
	}
	waitSessionClosed(t, g, orchTransport.ID)
	if d = vaultCheck(t, s.URL, worker, model.FleetVaultCheck{KeyID: orchestrator, OwnerID: orchOwner.ID, MachineIDs: both}); d.Allowed || d.KeyID != "" {
		t.Fatalf("revoked key provenance allowed: %+v", d)
	}
	// A disconnected issuer shows its durable owner but no key, so it is
	// denied until its receiver supplies stored provenance.
	g.mu.Lock()
	c := g.peers[transport.ID]
	g.mu.Unlock()
	_ = c.ws.CloseNow()
	waitSessionClosed(t, g, transport.ID)
	if d = vaultCheck(t, s.URL, worker, model.FleetVaultCheck{Issuer: transport.ID, MachineIDs: m1}); d.Allowed || d.KeyID != "" || d.OwnerID != owner.ID {
		t.Fatalf("disconnected issuer: %+v", d)
	}
	if d = vaultCheck(t, s.URL, worker, model.FleetVaultCheck{KeyID: key, OwnerID: owner.ID, MachineIDs: m1}); !d.Allowed {
		t.Fatal("valid stored provenance denied")
	}
	// Expired rules stop matching without an edit.
	g.mu.Lock()
	p := g.vaultPolicies[alice.ID]
	for i := range p.Allowances {
		p.Allowances[i].ExpiresAt = time.Now().Add(-time.Second)
	}
	g.vaultPolicies[alice.ID] = p
	g.mu.Unlock()
	if d = vaultCheck(t, s.URL, worker, model.FleetVaultCheck{KeyID: key, OwnerID: owner.ID, MachineIDs: m1}); d.Allowed {
		t.Fatal("expired allowance matched")
	}
	// A pause allows everything until it lapses.
	data, _ := browser.request(t, "GET", "/console/api/fleet/vault", nil, 200)
	if v := vaultView(t, data); len(v.Allowances) != 0 {
		t.Fatalf("expired allowances listed: %s", data)
	}
	browser.request(t, "PUT", "/console/api/fleet/vault", map[string]any{"revision": 3, "enabled": true, "pauseSeconds": 600}, 200)
	if d = vaultCheck(t, s.URL, worker, model.FleetVaultCheck{MachineIDs: m1}); !d.Allowed {
		t.Fatal("paused lock denied")
	}
	g.mu.Lock()
	p = g.vaultPolicies[alice.ID]
	p.PausedUntil = time.Now().Add(-time.Second)
	g.vaultPolicies[alice.ID] = p
	g.mu.Unlock()
	if d = vaultCheck(t, s.URL, worker, model.FleetVaultCheck{MachineIDs: m1}); d.Allowed {
		t.Fatal("lapsed pause must resume strict enforcement")
	}
	// The operator's active gateway lock enforces an unlocked fleet too.
	browser.request(t, "PUT", "/console/api/fleet/vault", map[string]any{"revision": 4, "enabled": false}, 200)
	g.mu.Lock()
	g.globalVault = globalVaultState{Enabled: true, Revision: 1}
	g.mu.Unlock()
	if d = vaultCheck(t, s.URL, worker, model.FleetVaultCheck{MachineIDs: m1}); d.Allowed {
		t.Fatal("global lock must enforce fleet rules")
	}
}

func TestFleetVaultMigrationDoesNotWidenHostRules(t *testing.T) {
	dir := t.TempDir()
	g, s := gatewayAt(t, dir)
	user, _ := createTestUser(t, s, "alice")
	s.Close()
	if err := g.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(dir, "gateway.db"))
	if err != nil {
		t.Fatal(err)
	}
	legacy := []byte(`{"userId":"` + user.ID + `","enabled":true,"revision":5,"allowances":[{"id":"one","keyId":"k","allMachines":true,"ownerId":"o"},{"id":"two","keyId":"keep","machineIds":["worker"]},{"id":"three","ownerId":"o","allMachines":true}]}`)
	if _, err := db.Exec(`INSERT INTO fleet_vault VALUES(?,?);`, user.ID, legacy); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`PRAGMA user_version=11`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	g, s = gatewayAt(t, dir)
	defer func() { s.Close(); _ = g.Close() }()
	p := g.vaultPolicies[user.ID]
	if !p.Enabled || p.Revision != 6 || len(p.Allowances) != 1 || p.Allowances[0].ID != "two" || !fleetDecision(p, "keep", []string{"worker"}, time.Now()) || fleetDecision(p, "k", []string{"worker"}, time.Now()) {
		t.Fatalf("migration changed key-only access or widened a restricted rule: %+v", p)
	}
}

// Bearer credentials cannot take over the console of a configured lock or
// operate machines around an enforced one; console sessions still can.
func TestFleetVaultBearerGuards(t *testing.T) {
	g, s := installationFixture(t)
	alice, a := createTestUser(t, s, "alice")
	provisionBrowser(t, a, "")
	browser := browserLogin(t, s.URL, "alice", browserPassword)
	w1, _ := vaultWorker(t, s.URL, a.Key, "w1", true)
	w2, _ := vaultWorker(t, s.URL, a.Key, "w2", true)
	key := accountKeyID(t, g, alice.ID)
	super := testAdmin{URL: s.URL, Key: superKey}
	browser.request(t, "PUT", "/console/api/fleet/vault", map[string]any{"revision": 0, "enabled": true, "pauseSeconds": 600}, 200)

	reset := map[string]string{"password": "another browser password"}
	if status, _ := vaultHTTP(t, s.URL, a.Key, "PUT", "/v1/console/password", reset); status != http.StatusForbidden {
		t.Fatal("account key reset its console password under a paused lock", status)
	}
	if status, _ := vaultHTTP(t, s.URL, superKey, "PUT", "/v1/console/password", map[string]string{"username": "alice", "password": "another browser password"}); status != http.StatusForbidden {
		t.Fatal("superuser key reset a locked fleet's console password", status)
	}
	// A tenant's lock cannot prevent the operator's first web login.
	provisionBrowser(t, super, "")
	if status, _ := vaultHTTP(t, s.URL, superKey, "PUT", "/v1/console/password", reset); status != http.StatusForbidden {
		t.Fatal("superuser key reset the operator login while a fleet lock exists", status)
	}
	operator := browserLogin(t, s.URL, "@operator", browserPassword)
	operator.request(t, "PUT", "/console/api/console/password", map[string]string{"username": "alice", "password": "recovered browser password"}, 204)
	browser = browserLogin(t, s.URL, "alice", "recovered browser password")
	browser.request(t, "PUT", "/console/api/console/password", map[string]string{"password": browserPassword, "currentPassword": "recovered browser password"}, 204)
	browser = browserLogin(t, s.URL, "alice", browserPassword)

	startup := map[string]string{"mode": "system"}
	path := "/v1/fleet/nodes/" + w1.ID
	// Paused: not enforced, so bearer operations proceed.
	if status, body := vaultHTTP(t, s.URL, a.Key, "POST", path+"/startup", startup); status != http.StatusOK {
		t.Fatal("paused lock blocked startup", status, string(body))
	}
	browser.request(t, "PUT", "/console/api/fleet/vault", map[string]any{"revision": 1, "enabled": true}, 200)
	for _, op := range []struct {
		method, path string
		body         any
	}{
		{"POST", path + "/startup", startup}, {"GET", path + "/secrets", nil}, {"DELETE", path + "/secrets/token", nil},
		{"GET", path + "/proxy", nil}, {"PUT", path + "/proxy", map[string]bool{"enabled": false}},
	} {
		if status, body := vaultHTTP(t, s.URL, a.Key, op.method, op.path, op.body); status != http.StatusForbidden {
			t.Fatalf("%s %s under a lock: HTTP %d: %s", op.method, op.path, status, body)
		}
	}
	browser.request(t, "POST", "/console/api/fleet/nodes/"+w1.ID+"/startup", startup, 200)
	browser.request(t, "POST", "/console/api/fleet/vault/allowances", map[string]any{"revision": 2, "keyId": key, "machineIds": []string{w1.ID}}, 201)
	if status, body := vaultHTTP(t, s.URL, a.Key, "POST", path+"/startup", startup); status != http.StatusOK {
		t.Fatal("key-only rule did not admit its bearer", status, string(body))
	}
	if status, _ := vaultHTTP(t, s.URL, a.Key, "POST", "/v1/fleet/nodes/"+w2.ID+"/startup", startup); status != http.StatusForbidden {
		t.Fatal("rule admitted an uncovered machine", status)
	}
	// Wake needs one rule covering the sleeping target and its relay.
	g.mu.Lock()
	p := principal{Role: roleUser, KeyID: key, UserID: alice.ID}
	targetOnly := g.vaultBearerAllowedLocked(p, []string{w1.ID, w2.ID}, time.Now())
	g.mu.Unlock()
	if targetOnly {
		t.Fatal("wake relay outside the rule admitted")
	}
	if status, _ := vaultHTTP(t, s.URL, a.Key, "PUT", "/v1/console/password", reset); status != http.StatusForbidden {
		t.Fatal("allowance must not reopen bearer console password resets")
	}
}

func TestFleetVaultPersistsAndMigrates(t *testing.T) {
	dir := t.TempDir()
	g, s := gatewayAt(t, dir)
	alice, a := createTestUser(t, s, "alice")
	provisionBrowser(t, a, "")
	browser := browserLogin(t, s.URL, "alice", browserPassword)
	w1, _ := vaultWorker(t, s.URL, a.Key, "w1", true)
	key := accountKeyID(t, g, alice.ID)
	browser.request(t, "POST", "/console/api/fleet/vault/allowances", map[string]any{"revision": 0, "keyId": key, "machineIds": []string{w1.ID}}, 201)
	browser.request(t, "PUT", "/console/api/fleet/vault", map[string]any{"revision": 1, "enabled": true, "pauseSeconds": 120}, 200)
	g.mu.Lock()
	want := string(model.JSON(g.vaultPolicies[alice.ID]))
	g.mu.Unlock()
	s.Close()
	if err := g.Close(); err != nil {
		t.Fatal(err)
	}
	g, s = gatewayAt(t, dir)
	g.mu.Lock()
	got := string(model.JSON(g.vaultPolicies[alice.ID]))
	g.mu.Unlock()
	s.Close()
	if err := g.Close(); err != nil {
		t.Fatal(err)
	}
	if got != want || !strings.Contains(got, `"enabled":true`) || !strings.Contains(got, "pausedUntil") {
		t.Fatalf("policy after restart: %s, want %s", got, want)
	}
	// Rewind to schema 9; reopening must add the tables and keep data.
	db, err := sql.Open("sqlite", filepath.Join(dir, "gateway.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`DROP TABLE client_profiles; DROP TABLE fleet_vault; DROP TABLE global_vault; PRAGMA user_version=9;`); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	g, s = gatewayAt(t, dir)
	defer func() { s.Close(); _ = g.Close() }()
	var version int
	if err := g.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != currentSchemaVersion {
		t.Fatal("migrated schema version", version, err)
	}
	g.mu.Lock()
	empty := len(g.vaultPolicies) == 0
	g.vaultPolicies[alice.ID] = fleetVaultPolicy{UserID: alice.ID, Enabled: true, Revision: 1}
	err = g.commit(change{vaultPolicies: []string{alice.ID}})
	g.mu.Unlock()
	if !empty || err != nil {
		t.Fatal("migrated table must start empty and accept policies", empty, err)
	}
}

// vaultNodeEdit changes stored registrations as enrollment would have.
func vaultNodeEdit(t *testing.T, g *Gateway, edit func(n *model.Node), ids ...string) {
	t.Helper()
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, id := range ids {
		n := g.nodes[id]
		edit(&n)
		g.nodes[id] = n
	}
	if err := g.commit(change{nodes: ids}); err != nil {
		t.Fatal(err)
	}
}

// Machine lifecycle mutations by a bearer key need a key-only rule covering
// every machine they affect, checked inside the mutation lock. Personal
// browser sessions remain the recovery path.
func TestFleetVaultBearerMachineLifecycle(t *testing.T) {
	g, s := installationFixture(t)
	alice, a := createTestUser(t, s, "alice")
	provisionBrowser(t, a, "")
	browser := browserLogin(t, s.URL, "alice", browserPassword)
	w1, _ := vaultWorker(t, s.URL, a.Key, "w1", true)
	w2, _ := vaultWorker(t, s.URL, a.Key, "w2", true)
	key := accountKeyID(t, g, alice.ID)
	vaultNodeEdit(t, g, func(n *model.Node) { n.Managed = true }, w1.ID, w2.ID)
	browser.request(t, "PUT", "/console/api/fleet/vault", map[string]any{"revision": 0, "enabled": true}, 200)
	revision := int64(1)
	allow := func(body map[string]any) {
		t.Helper()
		body["revision"] = revision
		body["keyId"] = key
		browser.request(t, "POST", "/console/api/fleet/vault/allowances", body, 201)
		revision++
	}
	bearer := func(method, path string, body any, want int) {
		t.Helper()
		if status, data := vaultHTTP(t, s.URL, a.Key, method, path, body); status != want {
			t.Fatalf("%s %s: HTTP %d, want %d: %s", method, path, status, want, data)
		}
	}
	node := func(id string) model.Node {
		g.mu.Lock()
		defer g.mu.Unlock()
		return g.nodes[id]
	}
	one, two := "/v1/fleet/nodes/"+w1.ID, "/v1/fleet/nodes/"+w2.ID
	labels := map[string]any{"labels": labelPatch(map[string]string{"pool": "a"}, nil, nil)["labels"]}

	// A bare key changes nothing.
	for _, q := range []map[string]any{{"disabled": true}, {"name": "renamed"}, labels} {
		bearer("PATCH", one, q, http.StatusForbidden)
	}
	bearer("DELETE", one+"?stop=true", nil, http.StatusForbidden)
	g.mu.Lock()
	overlays := len(g.labelOverlays)
	g.mu.Unlock()
	if n := node(w1.ID); n.Disabled || n.Name != "w1" || overlays != 0 {
		t.Fatalf("refused mutation applied: %+v", n)
	}

	// A key-scoped rule admits exactly its machine.
	allow(map[string]any{"machineIds": []string{w1.ID}})
	bearer("PATCH", one, map[string]any{"disabled": true}, http.StatusOK)
	bearer("PATCH", one, map[string]any{"disabled": false}, http.StatusOK)
	bearer("PATCH", one, labels, http.StatusOK)
	bearer("PATCH", one, map[string]any{"name": "w1b"}, http.StatusOK)
	bearer("PATCH", two, map[string]any{"disabled": true}, http.StatusForbidden)
	bearer("DELETE", two+"?stop=true", nil, http.StatusForbidden)
	if n := node(w1.ID); n.Name != "w1b" || node(w2.ID).Disabled {
		t.Fatal("scoped mutations", n, node(w2.ID))
	}

	// A paired rename affects both contexts, so one rule must cover both.
	hybrid := identity.NewID()
	vaultNodeEdit(t, g, func(n *model.Node) { n.HybridID, n.HybridRole = hybrid, "user" }, w1.ID)
	vaultNodeEdit(t, g, func(n *model.Node) { n.HybridID, n.HybridRole = hybrid, "system" }, w2.ID)
	bearer("PATCH", one, map[string]any{"name": "pair"}, http.StatusForbidden)
	if node(w1.ID).Name != "w1b" || node(w2.ID).Name != "w2" {
		t.Fatal("refused paired rename applied")
	}
	// Revoking a redeemed paired invitation stops both workers.
	g.mu.Lock()
	for _, inv := range []installation{
		{Invitation: enrollment.Invitation{UserID: alice.ID, ID: "inv-user", Name: "w1b", HybridID: hybrid, HybridRole: "user", RedeemedID: w1.ID, ExpiresAt: time.Now().Add(time.Hour)}},
		{Invitation: enrollment.Invitation{UserID: alice.ID, ID: "inv-system", Name: "w2", HybridID: hybrid, HybridRole: "system", RedeemedID: w2.ID, ExpiresAt: time.Now().Add(time.Hour)}},
	} {
		g.installations["hash-"+inv.ID] = inv
	}
	err := g.commit(change{invitations: []string{"hash-inv-user", "hash-inv-system"}})
	g.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	bearer("DELETE", "/v1/fleet/installations/inv-user", nil, http.StatusForbidden)
	g.mu.Lock()
	revoked := g.installations["hash-inv-user"].Revoked || g.installations["hash-inv-system"].Revoked
	g.mu.Unlock()
	if revoked {
		t.Fatal("refused invitation revocation applied")
	}
	// Unredeemed invitations affect no worker and stay a separate permission.
	link := invitationFor(t, a, s.URL, "spare")
	bearer("DELETE", "/v1/fleet/installations/"+link.ID, nil, http.StatusNoContent)

	allow(map[string]any{"machineIds": []string{w1.ID, w2.ID}})
	bearer("PATCH", one, map[string]any{"name": "pair"}, http.StatusOK)
	if node(w1.ID).Name != "pair" || node(w2.ID).Name != "pair-system" {
		t.Fatal("paired rename", node(w1.ID).Name, node(w2.ID).Name)
	}
	bearer("DELETE", "/v1/fleet/installations/inv-user", nil, http.StatusNoContent)

	// Browser sessions recover without any rule; a covered key can forget.
	browser.request(t, "PATCH", "/console/api/fleet/nodes/"+w2.ID, map[string]any{"disabled": true}, 200)
	browser.request(t, "DELETE", "/console/api/fleet/nodes/"+w2.ID+"?stop=true", nil, 204)
	bearer("DELETE", one+"?stop=true", nil, http.StatusNoContent)
	g.mu.Lock()
	_, kept := g.nodes[w1.ID]
	g.mu.Unlock()
	if kept {
		t.Fatal("forgotten machine kept")
	}
}

// The operator's /v1/admin alias follows the legacy fleet's lock.
func TestFleetVaultAdminForgetAlias(t *testing.T) {
	g, s := installationFixture(t)
	provisionBrowser(t, testAdmin{URL: s.URL, Key: superKey}, "")
	operator := browserLogin(t, s.URL, "@operator", browserPassword)
	w1, _ := vaultWorker(t, s.URL, commonKey, "legacy-1", true)
	w2, _ := vaultWorker(t, s.URL, commonKey, "legacy-2", true)
	operator.request(t, "PUT", "/console/api/fleet/vault", map[string]any{"revision": 0, "enabled": true}, 200)
	for _, path := range []string{"/v1/admin/nodes/" + w1.ID + "?stop=true", "/v1/fleet/nodes/" + w1.ID + "?stop=true"} {
		if status, _ := vaultHTTP(t, s.URL, superKey, "DELETE", path, nil); status != http.StatusForbidden {
			t.Fatal("superuser key forgot a locked machine", path, status)
		}
	}
	operator.request(t, "POST", "/console/api/fleet/vault/allowances", map[string]any{"revision": 1, "keyId": superuserKeyID, "machineIds": []string{w1.ID}}, 201)
	if status, _ := vaultHTTP(t, s.URL, superKey, "DELETE", "/v1/admin/nodes/"+w2.ID+"?stop=true", nil); status != http.StatusForbidden {
		t.Fatal("rule admitted an uncovered machine", status)
	}
	if status, body := vaultHTTP(t, s.URL, superKey, "DELETE", "/v1/admin/nodes/"+w1.ID+"?stop=true", nil); status != http.StatusNoContent {
		t.Fatal("covered superuser key", status, string(body))
	}
	g.mu.Lock()
	_, kept := g.nodes[w1.ID]
	g.mu.Unlock()
	if kept {
		t.Fatal("forgotten machine kept")
	}
}
