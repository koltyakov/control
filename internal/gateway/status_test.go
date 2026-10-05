package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/koltyakov/control/internal/buildinfo"
	"github.com/koltyakov/control/internal/identity"
	"github.com/koltyakov/control/internal/model"
)

func TestStatusScopesFleetAndReadsCachedGatewayMetrics(t *testing.T) {
	dir := t.TempDir()
	g, err := New(dir, commonKey, Options{SuperuserKey: superKey, PublicURL: "https://control.example.com", Software: buildinfo.Info{Version: "v-test"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = g.Close() })
	s := httptest.NewServer(g.Handler())
	t.Cleanup(s.Close)
	alice, a := createTestUser(t, s, "alice")
	bob, b := createTestUser(t, s, "bob")
	for _, user := range []struct {
		id     string
		client testAdmin
	}{{alice.ID, a}, {bob.ID, b}} {
		id, err := identity.Load(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		ws := rawPeer(t, s.URL, user.client.Key, "worker", user.id, id)
		t.Cleanup(func() { _ = ws.CloseNow() })
	}
	ctx := context.Background()
	g.metricsCancel()
	g.metricsWG.Wait()
	if _, err := g.metrics.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	before := g.metrics.Snapshot()
	var common struct{ Token string }
	if err := a.JSON(ctx, "POST", "/v1/fleet/keys", map[string]string{"name": "observer"}, &common); err != nil {
		t.Fatal(err)
	}
	for _, c := range []testAdmin{a, b, {URL: s.URL, Key: common.Token}, {URL: s.URL, Key: superKey}} {
		var got model.PoolActivitySnapshot
		if err := c.JSON(ctx, "GET", "/v1/status", nil, &got); err != nil {
			t.Fatal(err)
		}
		if got.Gateway == nil || got.Gateway.URL != "https://control.example.com" || got.Gateway.Software.Version != "v-test" || got.Gateway.StartedAt.IsZero() {
			t.Fatalf("missing gateway identity: %+v", got)
		}
		info := got.Gateway.System
		if info == nil || !info.SampledAt.Equal(before.SampledAt) || info.MemoryTotalBytes == 0 || len(info.Disks) != 1 || info.Disks[0].Path != "state" {
			t.Fatalf("bad cached sample: %+v", info)
		}
		if strings.Contains(string(model.JSON(got)), dir) {
			t.Fatal("gateway state path disclosed")
		}
		want := 1
		if c.Key == superKey {
			want = 0
		}
		if len(got.Nodes) != want {
			t.Fatalf("foreign fleet disclosed: %+v", got.Nodes)
		}
		for _, n := range got.Nodes {
			owner := g.authenticate(c.Key).UserID
			g.mu.Lock()
			actualOwner := g.nodes[n.ID].UserID
			g.mu.Unlock()
			if actualOwner != owner || !n.Online || n.Status != "online" || len(n.Active) != 0 {
				t.Fatalf("incorrect directory observation: %+v", n)
			}
		}
	}
	if g.metrics.Snapshot().Disks[0].Path != dir {
		t.Fatal("status modified the collector cache")
	}
	for _, key := range []string{"", "wrong-key"} {
		req := httptest.NewRequest(http.MethodGet, "/v1/status", nil)
		req.Header.Set("Authorization", "Bearer "+key)
		r := httptest.NewRecorder()
		g.Handler().ServeHTTP(r, req)
		if r.Code != http.StatusUnauthorized {
			t.Fatal("status accepted an invalid key", r.Code)
		}
	}
	if err := (testAdmin{URL: s.URL, Key: superKey}).JSON(ctx, "DELETE", "/v1/admin/users/"+alice.ID, nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := a.JSON(ctx, "GET", "/v1/status", nil, nil); err == nil {
		t.Fatal("disabled account can read status")
	}
}
