package dashboard

import (
	"errors"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/koltyakov/control/internal/model"
)

func TestRenderAvailabilityAndHostileTerminalText(t *testing.T) {
	now := time.Now()
	snapshot := model.PoolActivitySnapshot{ObservedAt: now, Nodes: []model.NodeActivitySnapshot{
		{ID: "a", Name: "worker", Online: true, Status: "ready", ActiveCount: 1, Active: []model.Activity{{ID: "job", Kind: "task", Operation: "exec.run\x1b[2J\nspoof", State: "running", Owner: "a", Started: now}}},
		{Name: "offline-node", Status: "offline", LastSeen: now},
		{Name: "denied-node", Online: true, Status: "unavailable", Error: "not authorized"},
	}}
	text := Render(snapshot, now)
	for _, expected := range []string{"3 registered", "2 online", "1 observed", "1 in flight", "worker", "busy", "offline-node", "unavailable", "denied-node", "exec.run", "not authorized"} {
		if !strings.Contains(text, expected) {
			t.Fatalf("missing %q in:\n%s", expected, text)
		}
	}
	if strings.Contains(text, "\x1b") || strings.Contains(text, "\nspoof") {
		t.Fatal("remote text injected terminal control")
	}
}

func TestViewKeepsFailedSnapshotAndScrolls(t *testing.T) {
	m := view{width: 100, height: 6, options: Options{Interval: time.Second}, snapshot: model.PoolActivitySnapshot{ObservedAt: time.Now(), Nodes: []model.NodeActivitySnapshot{{Name: "remember-me", Status: "offline"}}}}
	updated, _ := m.Update(result{err: errors.New("gateway unreachable")})
	m = updated.(view)
	if !strings.Contains(m.View(), "STALE") || m.snapshot.Nodes[0].Name != "remember-me" {
		t.Fatal("failed refresh erased prior snapshot")
	}
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnd})
	m = updated.(view)
	if m.offset <= 0 {
		t.Fatal("dashboard cannot scroll to hidden rows")
	}
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyRight})
	m = updated.(view)
	if m.xOffset <= 0 {
		t.Fatal("dashboard cannot expose columns beyond the terminal width")
	}
	if _, cmd := m.Update(tick{generation: m.generation - 1}); cmd != nil {
		t.Fatal("stale timer started another poll loop")
	}
}
