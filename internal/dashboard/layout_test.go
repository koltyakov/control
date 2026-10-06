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
		if strings.Count(text, "v0.1.0*") != 2 || strings.Contains(text, "gabc1234") {
			t.Fatalf("gateway and machine versions not compact:\n%s", text)
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

func TestMachineColumnsFitFormattedValues(t *testing.T) {
	var b strings.Builder
	row := []string{"tj", "busy/leased", "1000d", "4096", "windows", "v10.2.3*", "100.0%", "1023.9GiB/1023.9GiB", "1023.9GiB", "10/10"}
	renderOptions{width: 120}.table(&b, machineColumns, [][]string{row})
	lines := strings.Split(b.String(), "\n")
	for i, start := range []int{0, 10, 23, 31, 37, 46, 56, 64, 85, 96} {
		if got := strings.Index(lines[0], machineColumns[i].label); got != start {
			t.Fatalf("%s header starts at %d, want %d: %q", machineColumns[i].label, got, start, lines[0])
		}
		if !strings.HasPrefix(lines[1][start:], row[i]) {
			t.Fatalf("%s value is truncated or misaligned: %q", machineColumns[i].label, lines[1])
		}
	}
}
