package dashboard

import (
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/koltyakov/control/internal/buildinfo"
	"github.com/koltyakov/control/internal/model"
)

func TestCompactVersions(t *testing.T) {
	for _, tt := range []struct {
		version string
		want    string
	}{
		{"", ""},
		{"v0.1.0", "v0.1.0"},
		{"v0.1.0-3-gabc1234-dirty", "v0.1.0*"},
		{"v0.1.0-dirty", "v0.1.0*"},
		{"v10.2.3-rc.1", "v10.2.3*"},
		{"v0.1.0+build.1", "v0.1.0*"},
		{"0.1.0-rc.1", "0.1.0*"},
		{"abc1234-dirty", "abc1234-dirty"},
		{"dev-my-change", "dev-my-change"},
	} {
		t.Run(tt.version, func(t *testing.T) {
			if got := (renderOptions{}).version(tt.version); got != tt.want {
				t.Fatalf("compact version = %q, want %q", got, tt.want)
			}
			if got := (renderOptions{details: true}).version(tt.version); got != tt.version {
				t.Fatalf("details changed version to %q", got)
			}
		})
	}
}

func TestRenderCompactVersionsAndFullDetails(t *testing.T) {
	now := time.Now()
	version := "v0.1.0-3-gabc1234-dirty"
	snapshot := model.PoolActivitySnapshot{
		ObservedAt: now,
		Gateway:    &model.GatewaySnapshot{URL: "https://gateway.example", Software: buildinfo.Info{Version: version}},
		Nodes:      []model.NodeActivitySnapshot{{Name: "tj", Status: "offline", Software: buildinfo.Info{Version: version}}},
	}
	for _, color := range []bool{false, true} {
		text := ansi.Strip(render(snapshot, now, renderOptions{width: 80, color: color}))
		if strings.Count(text, "v0.1.0*") != 1 || strings.Contains(text, "gabc1234") || strings.Contains(text, "Version") {
			t.Fatalf("gateway version not compact or trailing machine version not hidden:\n%s", text)
		}
		text = ansi.Strip(render(snapshot, now, renderOptions{width: 120, color: color}))
		if strings.Count(text, "v0.1.0*") != 2 || strings.Contains(text, "gabc1234") {
			t.Fatalf("wide compact view lost compact gateway or machine version:\n%s", text)
		}
		text = ansi.Strip(render(snapshot, now, renderOptions{width: 80, details: true, color: color}))
		if strings.Count(text, version) != 2 {
			t.Fatalf("details lost full versions:\n%s", text)
		}
	}
	if text := Render(snapshot, now); strings.Count(text, version) != 2 {
		t.Fatalf("plain-text output lost full versions:\n%s", text)
	}
}

func TestTablesHideColumnsFromRightToLeft(t *testing.T) {
	for _, columns := range [][]column{machineColumns, activityColumns, recentColumns} {
		row := make([]string, len(columns))
		row[0] = "worker"
		for keep := 1; keep <= len(columns); keep++ {
			width := 8
			for _, col := range columns[1:keep] {
				width += 2 + col.width
			}
			for _, terminalWidth := range []int{width, width + 1} {
				var b strings.Builder
				renderOptions{width: terminalWidth}.table(&b, columns, [][]string{row})
				header := strings.Split(b.String(), "\n")[0]
				labels := make([]string, keep)
				for i := range keep {
					labels[i] = columns[i].label
				}
				if strings.Join(strings.Fields(header), " ") != strings.Join(labels, " ") {
					t.Fatalf("width %d should retain the first %d columns, got %q", terminalWidth, keep, header)
				}
			}
		}
	}
}

func TestCompactMachineStateUsesEightCells(t *testing.T) {
	now := time.Now()
	snapshot := model.PoolActivitySnapshot{ObservedAt: now, Nodes: []model.NodeActivitySnapshot{{
		Name: "worker", Online: true, Status: "ready", ActiveCount: 1, Leased: true,
	}}}
	compact := render(snapshot, now, renderOptions{width: 140})
	if !strings.Contains(compact, "busy/le…") || strings.Contains(compact, "busy/leased") {
		t.Fatalf("compact state did not fit eight cells:\n%s", compact)
	}
	if details := render(snapshot, now, renderOptions{width: 40, details: true}); !strings.Contains(details, "busy/leased") {
		t.Fatalf("details lost the full state:\n%s", details)
	}
}

func TestMachineColumnsFitFormattedValues(t *testing.T) {
	var b strings.Builder
	row := []string{"tj", "disabled", "1000d", "4096", "10/10", "↑32 ↓32", "100.0%", "1023.9GiB/1023.9GiB", "1023.9GiB", "↑1 ↓1", "windows", "v10.2.3*"}
	renderOptions{width: 140}.table(&b, machineColumns, [][]string{row})
	lines := strings.Split(b.String(), "\n")
	for i, start := range []int{0, 10, 20, 28, 34, 41, 52, 60, 81, 92, 103, 112} {
		if got := strings.Index(lines[0], machineColumns[i].label); got != start {
			t.Fatalf("%s header starts at %d, want %d: %q", machineColumns[i].label, got, start, lines[0])
		}
		if !strings.HasPrefix(ansi.TruncateLeft(lines[1], start, ""), row[i]) {
			t.Fatalf("%s value is truncated or misaligned: %q", machineColumns[i].label, lines[1])
		}
	}
}

func TestRenderConnectionsAfterWork(t *testing.T) {
	now := time.Now()
	snapshot := model.PoolActivitySnapshot{ObservedAt: now, Nodes: []model.NodeActivitySnapshot{{
		Name: "worker", Online: true, Status: "ready", ActiveCount: 7,
		DirectSessions: 2, RelaySessions: 3, OS: "linux",
		Software:        buildinfo.Info{Version: "v1.2.3"},
		Tunnels:         &model.TunnelCounts{Forward: 1, Reverse: 2},
		RetainedTunnels: &model.TunnelCounts{Forward: 3, Reverse: 4},
	}}}
	for _, options := range []renderOptions{{width: 120}, {width: 120, details: true}} {
		text := render(snapshot, now, options)
		lines := strings.Split(text, "\n")
		found := false
		for i, line := range lines {
			if !strings.HasPrefix(line, "Node ") {
				continue
			}
			found = true
			headers := strings.Fields(line)
			values := strings.Fields(lines[i+1])
			if len(headers) < 7 || strings.Join(headers[:7], " ") != "Node State Seen Work D/R Tunnels CPU" || strings.Join(headers[len(headers)-2:], " ") != "OS Version" {
				t.Fatalf("incorrect machine column order: %q", line)
			}
			if len(values) < 8 || strings.Join(values[:8], " ") != "worker busy now 7 2/3 ↑1 ↓2 -" || strings.Join(values[len(values)-2:], " ") != "linux v1.2.3" {
				t.Fatalf("machine values do not match headers: %q", lines[i+1])
			}
		}
		if !found {
			t.Fatalf("missing machine table:\n%s", text)
		}
		if options.details && (!strings.Contains(text, "Saved") || !strings.Contains(text, "↑3 ↓4")) {
			t.Fatalf("details lost saved counts separate from live activity:\n%s", text)
		}
	}
}
