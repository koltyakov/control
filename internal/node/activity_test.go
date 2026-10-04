package node

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/koltyakov/control/internal/model"
)

func eventually(t *testing.T, ctx context.Context, check func() bool) {
	t.Helper()
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		if check() {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatal("condition not reached", ctx.Err())
		case <-ticker.C:
		}
	}
}

func TestActivitiesObserveOtherOwnersWithoutTaskAccess(t *testing.T) {
	source, _, consumer := cluster(t, true, func(i int, cfg *Config) {
		cfg.MaxTasks = 1
		if i == 1 {
			cfg.Allow = map[string][]string{"source": {"activities.list"}, "consumer": {"tasks.*", "exec.run"}}
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	command := executable(t, "sleep")
	command.Env["SECRET"] = "do-not-expose-activity-arguments"
	for _, id := range []string{"foreign-running", "foreign-queued"} {
		call(t, consumer, "worker", "tasks.start", model.TaskSpec{ID: id, Capability: "exec.run", Args: model.JSON(command)}, nil)
	}
	var snapshot model.NodeActivitySnapshot
	eventually(t, ctx, func() bool {
		call(t, source, "worker", "activities.list", model.ActivityQuery{Recent: 5}, &snapshot)
		states := map[string]bool{}
		for _, a := range snapshot.Active {
			if a.Kind == "task" {
				states[a.State] = true
				if a.Owner != consumer.Identity.ID {
					t.Fatal("incorrect activity owner")
				}
			}
		}
		return states["running"] && states["queued"]
	})
	if strings.Contains(string(model.JSON(snapshot)), "do-not-expose") {
		t.Fatal("snapshot leaked executable arguments")
	}
	if err := source.Call(ctx, "worker", "tasks.get", map[string]any{"id": "foreign-running"}, nil); err == nil {
		t.Fatal("activity permission granted task access")
	}
	if err := consumer.Call(ctx, "source", "activities.pool", model.PoolActivityQuery{}, nil); err == nil {
		t.Fatal("remote caller used local aggregator authority")
	}
	for _, id := range []string{"foreign-running", "foreign-queued"} {
		call(t, consumer, "worker", "tasks.cancel", map[string]any{"id": id}, nil)
	}
	for _, id := range []string{"foreign-running", "foreign-queued"} {
		if _, err := consumer.WaitTask(ctx, "worker", id); err != nil {
			t.Fatal(err)
		}
	}
	call(t, source, "worker", "activities.list", model.ActivityQuery{Recent: 5}, &snapshot)
	for _, a := range snapshot.Active {
		if a.Kind == "task" {
			t.Fatal("completed task remains in flight")
		}
	}
	found := false
	for _, a := range snapshot.Recent {
		if a.ID == "foreign-running" && a.State == "cancelled" {
			found = true
		}
	}
	if !found {
		t.Fatal("recent cancelled task missing")
	}
}

func TestPoolAvailabilityAndCachedSystemInfo(t *testing.T) {
	source, _, consumer := cluster(t, true, func(i int, cfg *Config) {
		cfg.MetricsIntervalSeconds = -1
		if i == 1 {
			cfg.Allow = map[string][]string{}
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	var before, after model.SystemInfo
	call(t, source, "source", "system.info", map[string]any{}, &before)
	if before.SampledAt.IsZero() || before.MemoryTotalBytes == 0 || before.LogicalCPUs == 0 || len(before.Disks) == 0 {
		t.Fatalf("missing system capabilities: %+v", before)
	}
	for range 3 {
		call(t, source, "source", "system.info", map[string]any{}, &after)
		if !before.SampledAt.Equal(after.SampledAt) {
			t.Fatal("cached reads triggered collection")
		}
	}
	call(t, source, "source", "system.info", map[string]any{"refresh": true}, &after)
	if !after.SampledAt.After(before.SampledAt) {
		t.Fatal("explicit refresh did not sample")
	}
	if err := consumer.Close(); err != nil {
		t.Fatal(err)
	}
	var pool model.PoolActivitySnapshot
	eventually(t, ctx, func() bool {
		call(t, source, "", "activities.pool", model.PoolActivityQuery{}, &pool)
		for _, n := range pool.Nodes {
			if n.Name == "consumer" {
				return n.Status == "offline"
			}
		}
		return false
	})
	if len(pool.Nodes) != 3 {
		t.Fatal("offline or denied machine missing")
	}
	for _, n := range pool.Nodes {
		switch n.Name {
		case "worker":
			if n.Status != "unavailable" || !n.Online || n.Error == "" {
				t.Fatalf("denied node mislabeled: %+v", n)
			}
		case "source":
			if n.Status != "ready" || n.ActiveCount != 0 || n.System == nil {
				t.Fatalf("idle node mislabeled: %+v", n)
			}
		case "consumer":
			if n.Online || n.LastSeen.IsZero() {
				t.Fatalf("offline node mislabeled: %+v", n)
			}
		}
	}
	call(t, source, "", "activities.pool", model.PoolActivityQuery{Nodes: []string{"source"}}, &pool)
	if len(pool.Nodes) != 1 || pool.Nodes[0].Name != "source" {
		t.Fatal("node filter ignored")
	}
}

func TestSynchronousActivityEndsOnDisconnect(t *testing.T) {
	source, _, _ := cluster(t, true, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	requestCtx, stop := context.WithCancel(ctx)
	defer stop()
	done := make(chan error, 1)
	go func() { done <- source.Call(requestCtx, "worker", "exec.run", executable(t, "sleep"), nil) }()
	var snapshot model.NodeActivitySnapshot
	eventually(t, ctx, func() bool {
		call(t, source, "worker", "activities.list", model.ActivityQuery{}, &snapshot)
		for _, a := range snapshot.Active {
			if a.Operation == "exec.run" && a.Kind == "operation" {
				return true
			}
		}
		return false
	})
	stop()
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	eventually(t, ctx, func() bool {
		call(t, source, "worker", "activities.list", model.ActivityQuery{}, &snapshot)
		return len(snapshot.Active) == 0
	})
}
