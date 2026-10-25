package gateway

import (
	"path/filepath"
	"testing"

	"github.com/koltyakov/control/internal/enrollment"
	"github.com/koltyakov/control/internal/identity"
)

func TestHybridContextHydrationFromRedeemedInvitations(t *testing.T) {
	g, server := installationFixture(t)
	for _, role := range []string{"user", "system"} {
		id, err := identity.Load(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		ws := rawPeer(t, server.URL, commonKey, "hybrid-"+role, legacyUser, id)
		if ws == nil {
			t.Fatal("registration failed")
		}
		g.mu.Lock()
		g.installations[role] = installation{Invitation: enrollment.Invitation{ID: role, UserID: legacyUser, Name: "hybrid-" + role, RedeemedID: id.ID, HybridID: "pair", HybridRole: role}}
		err = g.commit(change{invitations: []string{role}})
		g.mu.Unlock()
		if err != nil {
			t.Fatal(err)
		}
	}
	server.Close()
	if err := g.Close(); err != nil {
		t.Fatal(err)
	}
	restored, err := New(filepath.Dir(g.path), commonKey, Options{SuperuserKey: superKey})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = restored.Close() }()
	nodes := restored.directory(legacyUser)
	if len(nodes) != 2 {
		t.Fatal("lost independent context records", nodes)
	}
	for _, n := range nodes {
		if n.HybridID != "pair" || (n.HybridRole != "user" && n.HybridRole != "system") {
			t.Fatal("old enrollment pairing was not hydrated", n)
		}
	}
}
