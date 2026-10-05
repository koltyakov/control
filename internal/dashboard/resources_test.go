package dashboard

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/koltyakov/control/internal/model"
)

func TestResourceBarsRemainBoundedAndDistinguishUnknownUsage(t *testing.T) {
	for _, width := range []int{1, 20, 37, 38, 60, 120} {
		for _, sample := range []struct{ used, total uint64 }{{0, 0}, {0, 100}, {25, 100}, {100, 100}, {150, 100}} {
			var b strings.Builder
			(renderOptions{width: width, color: true}).resources(&b, "0.7%", &model.SystemInfo{MemoryUsedBytes: sample.used, MemoryTotalBytes: sample.total, Disks: []model.DiskUsage{{UsedBytes: sample.used, TotalBytes: sample.total}}})
			text := ansi.Strip(strings.TrimSuffix(b.String(), "\n"))
			if strings.Contains(text, "\n") || ansi.StringWidth(text) > width {
				t.Fatalf("resource row overflow at width %d: %q", width, text)
			}
			if width >= 38 {
				if !strings.Contains(text, "CPU") || !strings.Contains(text, "RAM") || !strings.Contains(text, "Disk") {
					t.Fatal("resource summary lost a resource label")
				}
				if sample.total == 0 && (!strings.Contains(text, "?") || strings.Contains(text, "0%")) {
					t.Fatal("unknown capacity displayed as empty usage")
				}
				if sample.total > 0 && sample.used >= sample.total && (!strings.Contains(text, "100%") || strings.Contains(text, "░")) {
					t.Fatal("full capacity did not fill the bar")
				}
			}
		}
	}
	plain := (renderOptions{}).resource("Disk", 25, 100, 12, true)
	if strings.Contains(plain, "\x1b") || !strings.Contains(plain, "25%") || !strings.Contains(plain, "25B/100B") {
		t.Fatal("plain-text bar lost its numerical values")
	}
}
