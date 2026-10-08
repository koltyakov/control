package dashboard

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/koltyakov/control/internal/model"
)

func TestCompactErrorMessages(t *testing.T) {
	for _, tt := range []struct{ message, want string }{
		{"context deadline exceeded", "Activity request timed out"},
		{"read tcp 127.0.0.1:7331: i/o timeout", "Activity request timed out"},
		{"request: context canceled", "Activity request cancelled"},
		{"not authorized", "Activity request denied"},
		{"HTTP 403 Forbidden", "Activity request denied"},
		{"dial tcp: connection refused", "Connection refused"},
		{"unsupported activity protocol", "unsupported activity protocol"},
	} {
		t.Run(tt.message, func(t *testing.T) {
			if got := (renderOptions{}).errorMessage(tt.message, "Activity request"); got != tt.want {
				t.Fatalf("compact error = %q, want %q", got, tt.want)
			}
			if got := (renderOptions{details: true}).errorMessage(tt.message, "Activity request"); got != tt.message {
				t.Fatalf("details lost diagnostic: %q", got)
			}
		})
	}
}

func TestMachineWarnings(t *testing.T) {
	now := time.Now()
	snapshot := model.PoolActivitySnapshot{ObservedAt: now, Nodes: []model.NodeActivitySnapshot{
		{Name: "worker", Online: true, Status: "unavailable", Error: "context deadline exceeded"},
		{Name: "other", Online: true, Status: "unavailable", Error: strings.Repeat("long diagnostic ", 30) + "\x1b[2J\nspoof"},
	}}
	for _, color := range []bool{false, true} {
		text := render(snapshot, now, renderOptions{width: 60, color: color})
		plain := ansi.Strip(text)
		if strings.Count(plain, "Warnings") != 1 || !strings.Contains(plain, "\n\nWarnings\n  worker · Activity request timed out\n  other · ") || strings.Contains(plain, "context deadline exceeded") {
			t.Fatalf("warnings are not separated or readable:\n%s", plain)
		}
		for _, line := range strings.Split(text, "\n") {
			if ansi.StringWidth(line) > 60 {
				t.Fatalf("compact warning exceeds terminal width: %q", line)
			}
		}
		if strings.Contains(plain, "\nspoof") || (!color && strings.Contains(text, "\x1b")) {
			t.Fatal("warning injected terminal controls")
		}
		if color && !strings.Contains(text, "\x1b[33mworker") {
			t.Fatal("warning target is not highlighted")
		}
	}
	for _, text := range []string{Render(snapshot, now), render(snapshot, now, renderOptions{details: true})} {
		if !strings.Contains(text, "worker · context deadline exceeded") || !strings.Contains(text, clean(snapshot.Nodes[1].Error)) {
			t.Fatalf("details lost the original diagnostic:\n%s", text)
		}
	}
	for i := range snapshot.Nodes {
		snapshot.Nodes[i].Error = ""
	}
	if text := Render(snapshot, now); strings.Contains(text, "Warnings") {
		t.Fatalf("empty warnings section:\n%s", text)
	}
}

func TestGatewayErrorFooter(t *testing.T) {
	m := view{width: 160, height: 10, err: context.DeadlineExceeded}
	text := m.View().Content
	if !strings.Contains(text, "Gateway refresh timed out") || !strings.Contains(text, "d details") || strings.Contains(text, "context deadline exceeded") {
		t.Fatalf("gateway failure is not readable: %s", text)
	}
	m.details = true
	if text = m.View().Content; !strings.Contains(text, "context deadline exceeded") {
		t.Fatalf("gateway details lost diagnostic: %s", text)
	}
}
