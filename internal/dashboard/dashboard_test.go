package dashboard

import (
	"errors"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/koltyakov/control/internal/buildinfo"
	"github.com/koltyakov/control/internal/model"
)

func TestRenderAvailabilityAndHostileTerminalText(t *testing.T) {
	now := time.Now()
	snapshot := model.PoolActivitySnapshot{ObservedAt: now, Nodes: []model.NodeActivitySnapshot{
		{ID: "a", Name: "worker", Online: true, Status: "ready", ActiveCount: 1, Active: []model.Activity{{ID: "job", Kind: "task", Operation: "exec.run\x1b[2J\nspoof", State: "running", Owner: "a", Started: now}}},
		{Name: "offline-node", OS: "windows", System: &model.SystemInfo{Arch: "amd64"}, Status: "offline", LastSeen: now.Add(-30 * time.Minute)},
		{Name: "denied-node", Online: true, Status: "unavailable", Error: "not authorized"},
	}}
	text := Render(snapshot, now)
	for _, expected := range []string{"3 nodes", "2 online", "1 observed", "1 active", "worker", "busy", "offline-node", "unavailable", "denied-node", "exec.run", "not authorized", "Seen", "now", "30m", "windows"} {
		if !strings.Contains(text, expected) {
			t.Fatalf("missing %q in:\n%s", expected, text)
		}
	}
	if strings.Contains(text, "\x1b") || strings.Contains(text, "\nspoof") {
		t.Fatal("remote text injected terminal control")
	}
	if strings.Contains(text, "last seen:") || strings.Contains(text, "windows/amd64") {
		t.Fatal("machine table still has an extra seen row or architecture in OS")
	}
}

func TestGatewayVisibleWithoutFleetOrLocalNode(t *testing.T) {
	now := time.Now()
	cpu := 12.5
	snapshot := model.PoolActivitySnapshot{ObservedAt: now, Gateway: &model.GatewaySnapshot{
		URL: "https://control.example.com", Software: buildinfo.Info{Version: "v1.2.3"}, StartedAt: now.Add(-time.Hour),
		System: &model.SystemInfo{OS: "linux", Arch: "amd64", LogicalCPUs: 4, CPUUsagePercent: &cpu, MemoryTotalBytes: 8 << 30, MemoryUsedBytes: 2 << 30, MemoryAvailableBytes: 6 << 30, SampledAt: now.Add(-45 * time.Second), IntervalSeconds: 15, Disks: []model.DiskUsage{{TotalBytes: 100 << 30, FreeBytes: 80 << 30, UsedBytes: 20 << 30}}},
	}}
	text := Render(snapshot, now)
	for _, expected := range []string{"Gateway  https://control.example.com  v1.2.3  Up 1h0m0s", "CPU 12.5%", "2.0GiB/8.0GiB", "25%", "No machines registered."} {
		if !strings.Contains(text, expected) {
			t.Fatalf("missing %q in %s", expected, text)
		}
	}
	if strings.Contains(text, "Sample") || strings.Contains(text, "45s") {
		t.Fatal("sample age is still displayed")
	}
	snapshot.Gateway.URL += "\x1b[2J\nspoof"
	if text := Render(snapshot, now); strings.Contains(text, "\x1b") || strings.Contains(text, "\nspoof") {
		t.Fatal("gateway text injected terminal control")
	}
}

func TestViewKeepsFailedSnapshotAndScrolls(t *testing.T) {
	m := view{width: 100, height: 6, options: Options{Interval: time.Second}, snapshot: model.PoolActivitySnapshot{ObservedAt: time.Now(), Nodes: []model.NodeActivitySnapshot{{Name: "remember-me", Status: "offline"}}}}
	m.snapshot.Gateway = &model.GatewaySnapshot{URL: "https://gateway.example"}
	for range 10 {
		m.snapshot.Nodes = append(m.snapshot.Nodes, model.NodeActivitySnapshot{Name: strings.Repeat("long-name-", 20), Status: "offline"})
	}
	updated, _ := m.Update(result{err: errors.New("gateway unreachable")})
	m = updated.(view)
	if !strings.Contains(m.View().Content, "STALE") || m.snapshot.Nodes[0].Name != "remember-me" {
		t.Fatal("failed refresh erased prior snapshot")
	}
	if !m.View().AltScreen {
		t.Fatal("dashboard must use the alternate screen")
	}
	updated, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyEnd})
	m = updated.(view)
	if m.offset <= 0 {
		t.Fatal("dashboard cannot scroll to hidden rows")
	}
	updated, _ = m.Update(tea.KeyPressMsg{Code: 'd', Text: "d"})
	m = updated.(view)
	updated, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyHome})
	m = updated.(view)
	updated, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyRight})
	m = updated.(view)
	if m.xOffset <= 0 {
		t.Fatal("dashboard cannot expose columns beyond the terminal width")
	}
	if _, cmd := m.Update(tick{generation: m.generation - 1}); cmd != nil {
		t.Fatal("stale timer started another poll loop")
	}
}

func TestStaleTagDoesNotMoveRows(t *testing.T) {
	now := time.Now()
	snapshot := model.PoolActivitySnapshot{ObservedAt: now, Gateway: &model.GatewaySnapshot{
		URL: "https://gateway.example", StartedAt: now.Add(-time.Hour),
		System: &model.SystemInfo{SampledAt: now},
	}, Nodes: []model.NodeActivitySnapshot{{Name: "worker", Online: true, Status: "ready"}}}
	for _, width := range []int{20, 40, 80} {
		m := view{width: width, height: 24, color: true, snapshot: snapshot}
		before := strings.Split(ansi.Strip(m.body(now)), "\n")
		page := m.pageSize()
		updated, _ := m.Update(result{err: errors.New("gateway request failed")})
		m = updated.(view)
		after := strings.Split(ansi.Strip(m.body(now)), "\n")
		if len(before) != len(after) || page != m.pageSize() {
			t.Fatal("failure changed the layout height")
		}
		found := false
		for i := range before {
			if strings.Contains(before[i], "Up ") {
				if !strings.Contains(after[i], "[STALE]") {
					t.Fatalf("missing inline tag: %q", after[i])
				}
				found = true
			} else if before[i] != after[i] {
				t.Fatalf("row %d moved on failure", i)
			}
		}
		if !found {
			t.Fatal("missing uptime row")
		}
		updated, _ = m.Update(result{snapshot: snapshot})
		if strings.Contains(updated.(view).body(now), "[STALE]") {
			t.Fatal("tag survived successful refresh")
		}
	}
}

func TestResponsiveTablesKeepImportantColumnsAndDetails(t *testing.T) {
	now := time.Now()
	cpu := 25.0
	snapshot := model.PoolActivitySnapshot{ObservedAt: now, Nodes: []model.NodeActivitySnapshot{{
		ID: "worker", Name: "worker-東京", OS: "linux", Online: true, Status: "ready", ActiveCount: 1,
		System: &model.SystemInfo{Arch: "amd64", CPUUsagePercent: &cpu, MemoryUsedBytes: 2 << 30, MemoryTotalBytes: 8 << 30, SampledAt: now},
		Active: []model.Activity{{ID: "long-activity-identifier-1234567890", Kind: "task", Operation: "exec.run", State: "running", Started: now}},
	}}}
	for _, width := range []int{1, 20, 40, 80, 120, 240} {
		text := render(snapshot, now, renderOptions{width: width, color: true})
		for _, line := range strings.Split(text, "\n") {
			if ansi.StringWidth(line) > width {
				t.Fatalf("width %d overflow: %q", width, line)
			}
		}
		if width >= 40 && (!strings.Contains(text, "running") || !strings.Contains(text, "exec.run")) {
			t.Fatalf("width %d hid essential activity: %s", width, text)
		}
	}
	narrow := render(snapshot, now, renderOptions{width: 80})
	wide := render(snapshot, now, renderOptions{width: 240})
	if strings.Contains(narrow, "RAM free") || strings.Contains(wide, "RAM free") || !strings.Contains(wide, "2.0GiB/8.0GiB") {
		t.Fatal("machine RAM should appear once as used/total")
	}
	if !strings.Contains(narrow, "CPU") || !strings.Contains(narrow, "Version") {
		t.Fatal("compact machine table lost CPU or version")
	}
	var short, long strings.Builder
	opts := renderOptions{width: 80}
	opts.table(&short, machineColumns, [][]string{{"node", "idle", "now", "0", "linux", "v1", "0.1%", "1/2", "2", "0/0"}})
	opts.table(&long, machineColumns, [][]string{{"longer-node-name", "busy/leased", "100d", "1000", "linux", "v1.2.3-4-gabcd-dirty", "100.0%", "100.0GiB/120.0GiB", "10.0GiB", "10/1"}})
	if strings.Split(short.String(), "\n")[0] != strings.Split(long.String(), "\n")[0] {
		t.Fatal("changing values moved or hid table columns")
	}
	details := render(snapshot, now, renderOptions{width: 80, details: true})
	if !strings.Contains(details, "long-activity-identifier-1234567890") || !strings.Contains(details, "Hardware") {
		t.Fatal("details did not recover full metadata")
	}
	if strings.Contains(narrow, "Hardware") {
		t.Fatal("compact view repeats hardware details")
	}
}

func TestRefreshHasNoBlinkingIndicatorOrDuplicateHeader(t *testing.T) {
	m := view{width: 80, height: 16, color: true, options: Options{GatewayURL: "https://gateway.example"}, snapshot: model.PoolActivitySnapshot{
		ObservedAt: time.Now(), Gateway: &model.GatewaySnapshot{URL: "https://gateway.example", Software: buildinfo.Info{Version: "v1"}},
	}}
	before := m.View().Content
	m.busy = true
	if after := m.View().Content; before != after {
		t.Fatal("starting a refresh changes the display")
	}
	if strings.Count(before, "https://gateway.example") != 1 || strings.Contains(before, "Control dashboard") {
		t.Fatal("gateway header is duplicated")
	}
	for _, unwanted := range []string{"\x1b[5m", "\x1b[6m", "refreshing", "Activity", "Hardware"} {
		if strings.Contains(before, unwanted) {
			t.Fatalf("empty view contains %q", unwanted)
		}
	}
	for _, size := range []tea.WindowSizeMsg{{Width: 40, Height: 5}, {Width: 1, Height: 1}} {
		updated, _ := m.Update(size)
		m = updated.(view)
		lines := strings.Split(m.View().Content, "\n")
		if len(lines) > size.Height {
			t.Fatalf("height overflow: %d > %d", len(lines), size.Height)
		}
		for _, line := range lines {
			if ansi.StringWidth(line) > size.Width {
				t.Fatal("viewport width overflow")
			}
		}
	}
}
