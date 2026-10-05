package node

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/koltyakov/control/internal/gateway"
	"github.com/koltyakov/control/internal/model"
)

func TestOwnerHealthWithoutLocalObserver(t *testing.T) {
	const adminKey = "owner-health-test-superuser-key-123456789"
	g, err := gateway.New(t.TempDir(), testToken, gateway.Options{SuperuserKey: adminKey})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = g.Close() }()
	server := httptest.NewServer(g.Handler())
	defer server.Close()
	n, err := New(Config{Name: "worker", Gateway: server.URL, Token: testToken, DataDir: t.TempDir(), WorkDir: t.TempDir(), MetricsIntervalSeconds: -1, Allow: map[string][]string{}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = n.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := n.Start(ctx); err != nil {
		t.Fatal(err)
	}
	status := func(key string) model.PoolActivitySnapshot {
		t.Helper()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/v1/status", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+key)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		var pool model.PoolActivitySnapshot
		if err := json.NewDecoder(resp.Body).Decode(&pool); err != nil || resp.StatusCode != http.StatusOK || len(pool.Nodes) != 1 {
			t.Fatalf("missing machine: %+v %v", pool, err)
		}
		return pool
	}
	read := func() model.NodeActivitySnapshot {
		t.Helper()
		return status(adminKey).Nodes[0]
	}
	eventually(t, ctx, func() bool { return read().Status == "summary" })
	var sample model.SystemInfo
	eventually(t, ctx, func() bool {
		var err error
		sample, err = n.system.Refresh(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return sample.CPUUsagePercent != nil
	})
	h := n.beginActivity(ctx, "exec", "private-operation", "private-owner", "private-peer")
	defer h.finish(nil)
	var got model.NodeActivitySnapshot
	// Wait for the actual periodic publisher rather than invoking it from the test.
	eventually(t, ctx, func() bool {
		got = read()
		return got.ActiveCount == 1 && got.System != nil && got.System.SampledAt.Equal(sample.SampledAt)
	})
	if got.System.CPUUsagePercent == nil || len(got.Active) != 0 || len(got.Recent) != 0 {
		t.Fatal("owner did not get a resource-only summary")
	}
	text := string(model.JSON(got))
	for _, secret := range []string{"private-operation", "private-owner", "private-peer", n.Config.WorkDir, n.Config.DataDir} {
		if strings.Contains(text, secret) {
			t.Fatal("health exposed private activity or paths", secret)
		}
	}
	if !n.system.Snapshot().SampledAt.Equal(sample.SampledAt) {
		t.Fatal("health reporting triggered resource collection")
	}
	pool := status(testToken)
	if pool.Nodes[0].Status != "online" || pool.Nodes[0].ActiveCount != 0 {
		t.Fatal("common key bypassed peer observation permissions")
	}
	h.finish(nil)
	n.reportHealth(ctx)
	eventually(t, ctx, func() bool { return read().ActiveCount == 0 })
	if err := n.Close(); err != nil {
		t.Fatal(err)
	}
	eventually(t, ctx, func() bool { got = read(); return !got.Online })
	if got.Status != "offline" || got.ActiveCount != 0 {
		t.Fatal("offline node retained live health")
	}
}
