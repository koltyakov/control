package client

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/koltyakov/control/internal/buildinfo"
	"github.com/koltyakov/control/internal/model"
)

func TestDashboardGatewaySurvivesPeerFailureAndScopesEnrichment(t *testing.T) {
	var mu sync.Mutex
	pool := model.PoolActivitySnapshot{ObservedAt: time.Now(), Gateway: &model.GatewaySnapshot{Software: buildinfo.Info{Version: "remote-version"}}, Nodes: []model.NodeActivitySnapshot{
		{ID: "own", Name: "worker", Online: true, Status: "unavailable"},
		{ID: "offline", Name: "old-worker", Status: "offline"},
	}}
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.URL.Path != "/v1/status" || r.Header.Get("Authorization") != "Bearer account-key" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(pool)
	}))
	defer remote.Close()
	admin := Admin{URL: remote.URL, Key: "account-key"}
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"result": model.PoolActivitySnapshot{Nodes: []model.NodeActivitySnapshot{
			{ID: "own", Name: "wrong-name", Online: true, Status: "ready", ActiveCount: 1,
				ConnectionRoutes: &model.ConnectionRouteCounts{Direct: 2, TURN: 1, Relay: 3},
				Active:           []model.Activity{{Kind: "tunnel", Operation: "tcp.listen", Phase: "listening"}},
				Recent:           []model.Activity{{Kind: "tunnel", Operation: "tcp.accept", State: "succeeded"}}},
			{ID: "foreign", Name: "foreign-worker", Online: true, Status: "ready"},
			{ID: "offline", Name: "old-worker", Online: true, Status: "ready"},
		}}})
	}))
	defer peer.Close()
	c := Client{URL: peer.URL, Token: "node-key"}
	got, err := c.Dashboard(context.Background(), admin, model.PoolActivityQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Nodes) != 2 || got.Nodes[0].Name != "worker" || got.Nodes[0].ActiveCount != 1 || got.Nodes[1].Status != "offline" || got.Notice != "" {
		t.Fatalf("bad enrichment: %+v", got)
	}
	if got.Gateway.URL != remote.URL || got.Gateway.Software.Version != "remote-version" {
		t.Fatal("lost remote identity")
	}
	if got.Nodes[0].Tunnels == nil || *got.Nodes[0].Tunnels != (model.TunnelCounts{Reverse: 1}) || got.Nodes[1].Tunnels != nil {
		t.Fatalf("live listener missing, completed socket counted, or offline counts invented: %+v", got.Nodes)
	}
	if routes := got.Nodes[0].ConnectionRoutes; routes == nil || *routes != (model.ConnectionRouteCounts{Direct: 2, TURN: 1, Relay: 3}) || got.Nodes[1].ConnectionRoutes != nil {
		t.Fatalf("route counts lost or invented for offline peer: %+v", got.Nodes)
	}
	mu.Lock()
	pool.Nodes[0].Status = "update"
	mu.Unlock()
	got, err = c.Dashboard(context.Background(), admin, model.PoolActivityQuery{})
	if err != nil || got.Nodes[0].Status != "update" || got.Notice != "" {
		t.Fatalf("peer observation replaced update status: %+v %v", got, err)
	}
	if got.Nodes[0].Tunnels != nil {
		t.Fatal("peer tunnel counts replaced gateway update status")
	}
	mu.Lock()
	pool.Nodes[0].Status = "summary"
	mu.Unlock()
	unreachable := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"result": model.PoolActivitySnapshot{Nodes: []model.NodeActivitySnapshot{
			{ID: "own", Online: true, Status: "unavailable", Error: "context deadline exceeded"},
		}}})
	}))
	defer unreachable.Close()
	got, err = (Client{URL: unreachable.URL, Token: "node-key"}).Dashboard(context.Background(), admin, model.PoolActivityQuery{})
	if err != nil || got.Nodes[0].Status != "summary" || got.Nodes[0].Error != "context deadline exceeded" {
		t.Fatalf("fresh health hid peer reachability failure: %+v %v", got, err)
	}
	mu.Lock()
	pool.Nodes[0].Status = "unavailable"
	mu.Unlock()
	peer.Close()
	got, err = c.Dashboard(context.Background(), admin, model.PoolActivityQuery{Nodes: []string{"worker"}})
	if err != nil || got.Gateway == nil || len(got.Nodes) != 1 || got.Nodes[0].Status != "unavailable" || got.Notice == "" {
		t.Fatalf("peer failure hid gateway: %+v %v", got, err)
	}
	mu.Lock()
	pool.Nodes[0].Status, pool.Nodes[0].ActiveCount = "summary", 2
	mu.Unlock()
	got, err = c.Dashboard(context.Background(), admin, model.PoolActivityQuery{})
	if err != nil || got.Nodes[0].Status != "summary" || got.Nodes[0].ActiveCount != 2 || got.Notice != "" {
		t.Fatalf("peer failure replaced fresh gateway health: %+v %v", got, err)
	}
	if got.Nodes[0].Tunnels != nil {
		t.Fatal("missing peer observation reported zero tunnels")
	}
	if _, err := c.Dashboard(context.Background(), admin, model.PoolActivityQuery{Nodes: []string{"foreign"}}); err == nil {
		t.Fatal("foreign filter accepted")
	}
	if _, err := c.Dashboard(context.Background(), Admin{URL: remote.URL, Key: "wrong"}, model.PoolActivityQuery{}); err == nil {
		t.Fatal("gateway failure was hidden")
	}
	mu.Lock()
	pool.Nodes = nil
	mu.Unlock()
	got, err = (Client{}).Dashboard(context.Background(), admin, model.PoolActivityQuery{})
	if err != nil || got.Gateway == nil || got.Notice != "" {
		t.Fatalf("empty fleet needs a local node: %+v %v", got, err)
	}
}
