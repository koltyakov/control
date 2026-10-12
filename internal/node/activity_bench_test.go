package node

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/koltyakov/control/internal/model"
)

// Measure observation independently of transport, disk, and resource collection.
func BenchmarkActivitySnapshot(b *testing.B) {
	for _, count := range []int{100, 10000, 100000} {
		b.Run(fmt.Sprintf("history=%d", count), func(b *testing.B) {
			n, err := New(Config{Name: "bench", Gateway: "http://127.0.0.1:1", Token: testToken, DataDir: b.TempDir(), WorkDir: b.TempDir(), MetricsIntervalSeconds: -1})
			if err != nil {
				b.Fatal(err)
			}
			b.Cleanup(func() { _ = n.Close() })
			now := time.Now().UTC()
			for i := range count {
				id := fmt.Sprintf("task-%06d", i)
				n.tasks[id] = &model.Task{ID: id, Owner: "owner", Spec: model.TaskSpec{Capability: "exec.run"}, State: "succeeded", Created: now.Add(-time.Hour), Updated: now.Add(time.Duration(i) * time.Millisecond)}
			}
			n.tasks["active"] = &model.Task{ID: "active", Owner: "owner", Spec: model.TaskSpec{Capability: "exec.run"}, State: "running", Created: now}
			n.cancels["active"] = context.CancelFunc(func() {})
			n.rebuildRecentTasksLocked()
			b.ReportAllocs()
			for b.Loop() {
				snapshot := n.activitySnapshot(model.ActivityQuery{Recent: 5})
				if snapshot.ActiveCount != 1 || len(snapshot.Recent) != 5 {
					b.Fatal("incomplete activity snapshot")
				}
			}
		})
	}
}
