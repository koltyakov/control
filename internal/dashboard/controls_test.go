package dashboard

import (
	"context"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/koltyakov/control/internal/model"
)

func TestDashboardHasNoPauseOrSnapshotCopy(t *testing.T) {
	m := view{ctx: context.Background(), width: 120, height: 20, generation: 3,
		options: Options{Interval: time.Second, Timeout: time.Second,
			Copy: func(context.Context, string) error { t.Fatal("dashboard copied a snapshot"); return nil },
		},
		snapshot: model.PoolActivitySnapshot{ObservedAt: time.Now()},
		fetch: func(context.Context) (model.PoolActivitySnapshot, error) {
			return model.PoolActivitySnapshot{ObservedAt: time.Now(), Nodes: []model.NodeActivitySnapshot{{Name: "fresh", Status: "offline"}}}, nil
		},
	}
	for _, key := range []rune{'p', 'c'} {
		next, cmd := m.Update(tea.KeyPressMsg{Code: key, Text: string(key)})
		m = next.(view)
		if cmd != nil || m.generation != 3 || m.busy {
			t.Fatalf("removed key %q changed polling or started an action", key)
		}
	}
	content := m.View().Content
	for _, unwanted := range []string{"pause", "resume", "copy", "Paused", "Snapshot copied"} {
		if strings.Contains(content, unwanted) {
			t.Fatalf("dashboard still shows %q: %s", unwanted, content)
		}
	}
	for _, expected := range []string{"q quit", "r refresh", "d details"} {
		if !strings.Contains(content, expected) {
			t.Fatalf("dashboard lost %q: %s", expected, content)
		}
	}
	if m.View().MouseMode != tea.MouseModeCellMotion {
		t.Fatal("dashboard must receive drag events to copy selections")
	}
	next, cmd := m.Update(tick{generation: m.generation})
	m = next.(view)
	if cmd == nil || !m.busy {
		t.Fatal("removed keys stopped automatic polling")
	}
	next, cmd = m.Update(cmd())
	m = next.(view)
	if cmd == nil || m.busy || len(m.snapshot.Nodes) != 1 || m.snapshot.Nodes[0].Name != "fresh" {
		t.Fatal("polling did not refresh the snapshot or schedule the next tick")
	}
}
