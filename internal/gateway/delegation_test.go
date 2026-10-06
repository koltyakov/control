package gateway

import (
	"context"
	"testing"

	"github.com/koltyakov/control/internal/identity"
	"github.com/koltyakov/control/internal/model"
)

func TestExecutionAuthorityComesFromAccountAuthentication(t *testing.T) {
	g, s := installationFixture(t)
	user, account := createTestUser(t, s, "authority-owner")
	var issued struct{ Token string }
	if err := account.JSON(context.Background(), "POST", "/v1/fleet/keys", map[string]string{"name": "worker-key"}, &issued); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		key            string
		supplied, want bool
	}{
		{issued.Token, true, false},
		{account.Key, false, true},
	} {
		id, err := identity.Generate()
		if err != nil {
			t.Fatal(err)
		}
		if ws := rawClientSession(t, s.URL, test.key, id, model.Node{Name: "cli-" + id.ID[:16], ExecutionAuthority: test.supplied}, true); ws == nil {
			t.Fatal("client connection rejected")
		}
		g.mu.Lock()
		actual := g.clients[id.ID].ExecutionAuthority
		g.mu.Unlock()
		if actual != test.want {
			t.Fatal("peer supplied its execution authority", actual, test.want)
		}
	}
	id, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	if ws := rawPeer(t, s.URL, account.Key, "account-enrolled-node", user.ID, id); ws == nil {
		t.Fatal("machine enrollment failed")
	}
	g.mu.Lock()
	actual := g.nodes[id.ID].ExecutionAuthority
	g.mu.Unlock()
	if actual {
		t.Fatal("machine enrollment conferred orchestrator authority")
	}
}
