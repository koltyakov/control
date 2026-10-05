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
			{ID: "own", Name: "wrong-name", Online: true, Status: "ready", ActiveCount: 1},
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
