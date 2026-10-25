package gateway

import (
	"context"
	"fmt"
	"net/url"
	"slices"
	"testing"
	"time"
)

type auditPage struct {
	Events []AuditEvent
	Next   int64
}

func readAudit(t *testing.T, c testAdmin, query string) auditPage {
	t.Helper()
	var page auditPage
	if err := c.JSON(context.Background(), "GET", "/v1/audit?"+query, nil, &page); err != nil {
		t.Fatal(err)
	}
	return page
}

func auditActions(page auditPage) []string {
	actions := []string{}
	for _, e := range page.Events {
		actions = append(actions, e.Action)
	}
	return actions
}

func TestAuditLogAttributesScopesAndSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	g, s := gatewayAt(t, dir)
	ctx := context.Background()
	aliceUser, alice := createTestUser(t, s, "alice")
	bobUser, bob := createTestUser(t, s, "bob")
	var issued struct {
		Key   APIKey
		Token string
	}
	if err := alice.JSON(ctx, "POST", "/v1/fleet/keys", map[string]string{"name": "ci"}, &issued); err != nil {
		t.Fatal(err)
	}
	if err := alice.JSON(ctx, "PATCH", "/v1/fleet/keys/"+issued.Key.ID, map[string]string{"name": "ci east"}, nil); err != nil {
		t.Fatal(err)
	}
	if err := bob.JSON(ctx, "DELETE", "/v1/fleet/keys/"+issued.Key.ID, nil, nil); err == nil {
		t.Fatal("foreign key revoked")
	}
	provisionBrowser(t, alice, "alice")
	browser := browserLogin(t, s.URL, "alice", browserPassword)
	browser.request(t, "DELETE", "/console/api/fleet/keys/"+issued.Key.ID, nil, 204)
	guess := testBrowser{URL: s.URL}
	guess.request(t, "POST", "/console/api/login", map[string]string{"username": "alice", "password": "not the password"}, 401)
	guess.request(t, "POST", "/console/api/login", map[string]string{"username": "nobody", "password": "not the password"}, 401)

	own := readAudit(t, alice, "")
	want := []string{"session.signin_failed", "key.revoke", "session.signin", "account.password", "key.rename", "key.create", "account.create"}
	if got := auditActions(own); !slices.Equal(got, want) {
		t.Fatalf("alice audit = %v, want %v", got, want)
	}
	for _, e := range own.Events {
		if e.UserID != aliceUser.ID || e.SourceIP != "127.0.0.1" || e.At.IsZero() || e.Fleet != "" {
			t.Fatalf("event outside alice's fleet or unattributed: %+v", e)
		}
	}
	byAction := map[string]AuditEvent{}
	for _, e := range own.Events {
		byAction[e.Action] = e
	}
	if e := byAction["key.create"]; e.Actor != "alice" || e.Via != "account key" || e.Target != "ci" || e.TargetID != issued.Key.ID {
		t.Fatalf("key creation misattributed: %+v", e)
	}
	if e := byAction["key.rename"]; e.Target != "ci east" || e.Detail != "was ci" {
		t.Fatalf("rename lost its previous name: %+v", e)
	}
	if e := byAction["key.revoke"]; e.Actor != "alice" || e.Via != "browser" || e.KeyID != "" {
		t.Fatalf("browser revocation misattributed: %+v", e)
	}
	if got := auditActions(readAudit(t, bob, "")); !slices.Equal(got, []string{"account.create"}) {
		t.Fatalf("bob saw another fleet's history: %v", got)
	}

	operator := testAdmin{URL: s.URL, Key: superKey}
	all := readAudit(t, operator, "")
	var unknown, bobCreated bool
	for _, e := range all.Events {
		unknown = unknown || (e.Action == "session.signin_failed" && e.Actor == "nobody" && e.UserID == legacyUser)
		bobCreated = bobCreated || (e.Action == "account.create" && e.UserID == bobUser.ID && e.Fleet == "bob" && e.Actor == "@operator" && e.Via == "superuser key")
	}
	if !unknown || !bobCreated {
		t.Fatal("operator history incomplete", all.Events)
	}

	keys := readAudit(t, alice, "category=key")
	if got := auditActions(keys); !slices.Equal(got, []string{"key.revoke", "key.rename", "key.create"}) {
		t.Fatalf("category filter = %v", got)
	}
	first := readAudit(t, alice, "limit=2")
	if len(first.Events) != 2 || first.Next != first.Events[1].ID {
		t.Fatalf("first page = %+v", first)
	}
	second := readAudit(t, alice, "limit=2&before="+fmt.Sprint(first.Next))
	if len(second.Events) != 2 || second.Events[0].Action != want[2] {
		t.Fatalf("second page = %+v", second)
	}
	if err := alice.JSON(ctx, "GET", "/v1/audit?category="+url.QueryEscape("nodes"), nil, nil); err == nil {
		t.Fatal("unknown category accepted")
	}

	s.Close()
	if err := g.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, server := gatewayAt(t, dir)
	defer func() { server.Close(); _ = reopened.Close() }()
	after := readAudit(t, testAdmin{URL: server.URL, Key: superKey}, "")
	if !slices.Equal(auditActions(after), auditActions(all)) {
		t.Fatal("audit history did not survive restart")
	}
}

func TestAuditRetentionBoundsFailedSignIns(t *testing.T) {
	g, _ := installationFixture(t)
	events := []AuditEvent{{At: time.Now(), UserID: legacyUser, Action: "key.create"}}
	for range maxAuditFailures + 10 {
		events = append(events, AuditEvent{At: time.Now(), UserID: legacyUser, Action: auditSignInFailed})
	}
	g.mu.Lock()
	err := g.commit(change{audit: events})
	g.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	var failed, other int
	if err := g.db.QueryRow(`SELECT COUNT(*) FROM audit_events WHERE action=?`, auditSignInFailed).Scan(&failed); err != nil {
		t.Fatal(err)
	}
	if err := g.db.QueryRow(`SELECT COUNT(*) FROM audit_events WHERE action<>?`, auditSignInFailed).Scan(&other); err != nil {
		t.Fatal(err)
	}
	if failed != maxAuditFailures || other < 1 {
		t.Fatalf("retained %d failed sign-ins and %d other events", failed, other)
	}
}
