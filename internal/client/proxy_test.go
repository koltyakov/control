package client

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/koltyakov/control/internal/enrollment"
	"github.com/koltyakov/control/internal/model"
)

func TestSelectProxyRequiresExplicitAmbiguousAddress(t *testing.T) {
	workers := []model.ProxyWorker{
		{ID: strings.Repeat("a", 64), Name: "office-bridge", Online: true, Endpoints: []model.ProxyEndpointStatus{{URL: "http://10.0.0.5:3128", State: model.ProxyReady}, {URL: "http://[fd00::5]:3128", Interface: "vpn", State: model.ProxyReady}}},
		{ID: strings.Repeat("b", 64), Name: "lab-bridge", Online: true, Endpoints: []model.ProxyEndpointStatus{{URL: "http://10.1.0.5:3128", State: model.ProxyReady}}},
	}
	if _, _, err := SelectProxy(workers, "office-bridge", ""); err == nil || !strings.Contains(err.Error(), "--proxy-url") {
		t.Fatalf("ambiguous selection: %v", err)
	}
	worker, endpoint, err := SelectProxy(workers, "office-bridge", "http://[FD00::5]:3128/")
	if err != nil || worker.ID != workers[0].ID || endpoint != "http://[fd00::5]:3128" {
		t.Fatalf("explicit IPv6 selection: %q %v", endpoint, err)
	}
	if _, endpoint, err = SelectProxy(workers, workers[1].ID, ""); err != nil || endpoint != "http://10.1.0.5:3128" {
		t.Fatalf("single endpoint by ID: %q %v", endpoint, err)
	}
	if _, _, err = SelectProxy(workers, "office-bridge", "http://10.9.9.9:1"); err == nil {
		t.Fatal("unadvertised endpoint accepted")
	}
	if _, _, err = SelectProxy(workers, "missing", ""); err == nil {
		t.Fatal("unknown worker accepted")
	}
}

func TestConfirmProxyRequiresEchoedSelection(t *testing.T) {
	q := enrollment.Request{ProxyWorker: strings.Repeat("a", 64), ProxyURL: "http://10.0.0.5:3128"}
	link := enrollment.Link{}
	if err := confirmProxy(q, link); err == nil {
		t.Fatal("older gateway response without a selection accepted")
	}
	link.GatewayProxy = &model.GatewayProxy{URL: "http://10.0.0.6:3128", Worker: q.ProxyWorker}
	if err := confirmProxy(q, link); err == nil {
		t.Fatal("different endpoint accepted")
	}
	link.GatewayProxy.URL = q.ProxyURL
	if err := confirmProxy(q, link); err != nil {
		t.Fatal(err)
	}
	if err := confirmProxy(enrollment.Request{}, enrollment.Link{}); err != nil {
		t.Fatal(err)
	}
}

func TestWaitProxyActivationRequiresNewOnlineCheck(t *testing.T) {
	for _, state := range []string{"old", "offline", "disabled", "fresh"} {
		t.Run(state, func(t *testing.T) {
			after := time.Now().UTC()
			checked := after.Add(time.Second)
			if state == "old" {
				checked = after.Add(-time.Second)
			}
			var reads atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				reads.Add(1)
				_ = json.NewEncoder(w).Encode(model.PoolActivitySnapshot{Nodes: []model.NodeActivitySnapshot{{
					ID: "worker", Online: state != "offline", Disabled: state == "disabled",
					ProxyEndpoints: []model.ProxyEndpointStatus{{URL: "http://10.0.0.5:3128", State: model.ProxyReady, CheckedAt: checked, Managed: true}},
				}}})
			}))
			defer server.Close()
			ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
			defer cancel()
			_, err := (Admin{URL: server.URL}).WaitProxyActivation(ctx, "worker", "http://10.0.0.5:3128", after, 10*time.Millisecond)
			if (err == nil) != (state == "fresh") {
				t.Fatalf("activation %s returned %v", state, err)
			}
			if reads.Load() == 0 {
				t.Fatal("activation was not observed")
			}
		})
	}
}
