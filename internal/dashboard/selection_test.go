package dashboard

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/koltyakov/control/internal/model"
)

func TestSelectionText(t *testing.T) {
	lines := []string{"\x1b[31malpha beta\x1b[0m", "middle   ", "last line"}
	for _, test := range []struct {
		name       string
		start, end selectionPoint
		want       string
	}{
		{"forward", selectionPoint{0, 0}, selectionPoint{4, 0}, "alpha"},
		{"backward", selectionPoint{4, 0}, selectionPoint{0, 0}, "alpha"},
		{"multiline", selectionPoint{6, 0}, selectionPoint{3, 2}, "beta\nmiddle\nlast"},
		{"reverse multiline", selectionPoint{3, 2}, selectionPoint{6, 0}, "beta\nmiddle\nlast"},
		{"click", selectionPoint{0, 0}, selectionPoint{0, 0}, ""},
		{"empty margin", selectionPoint{20, 0}, selectionPoint{30, 0}, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			selection := textSelection{lines: lines, start: test.start, end: test.end}
			if got := selection.text(); got != test.want {
				t.Fatalf("selected text = %q, want %q", got, test.want)
			}
		})
	}
	for _, test := range []struct {
		line        string
		left, right int
		want        string
	}{
		{"A界B", 2, 3, "界"},
		{"Ae\u0301B", 1, 2, "e\u0301"},
		{"A👩‍💻B", 2, 3, "👩‍💻"},
	} {
		if got := selectedCells(test.line, test.left, test.right); got != test.want {
			t.Fatalf("selected cells in %q = %q, want %q", test.line, got, test.want)
		}
	}
}

func TestDashboardDragCopiesFrozenVisibleText(t *testing.T) {
	var copied string
	m := view{ctx: context.Background(), width: 30, height: 5,
		options: Options{Interval: time.Second, GatewayURL: "https://old.example", Copy: func(ctx context.Context, text string) error {
			if _, ok := ctx.Deadline(); !ok {
				t.Fatal("clipboard command has no deadline")
			}
			copied = text
			return nil
		}},
	}
	original := m.View().Content
	apply := func(msg tea.Msg) tea.Cmd {
		next, cmd := m.Update(msg)
		m = next.(view)
		return cmd
	}
	if cmd := apply(tea.MouseClickMsg{X: 0, Y: 0, Button: tea.MouseLeft}); cmd != nil {
		t.Fatal("press copied before selection was finished")
	}
	apply(result{snapshot: model.PoolActivitySnapshot{ObservedAt: time.Now()}})
	if got := m.View().Content; got != original {
		t.Fatal("polling changed the screen while dragging")
	}
	if cmd := apply(tea.MouseMotionMsg{X: 9, Y: 0, Button: tea.MouseLeft}); cmd != nil {
		t.Fatal("motion copied before release")
	}
	if !strings.Contains(m.View().Content, "\x1b[7mConnecting\x1b[0m") {
		t.Fatal("drag did not highlight selected cells")
	}
	cmd := apply(tea.MouseReleaseMsg{X: 9, Y: 0, Button: tea.MouseNone})
	if cmd == nil || !m.selectionCopying || m.selection != nil {
		t.Fatal("release did not start copy and resume the display")
	}
	if duplicate := apply(tea.MouseClickMsg{Button: tea.MouseLeft}); duplicate != nil || m.selection != nil {
		t.Fatal("clipboard operations must be serialized")
	}
	apply(cmd())
	if copied != "Connecting" || m.selectionCopying || !strings.Contains(m.View().Content, "Selection copied.") {
		t.Fatalf("copy result = %q, notice = %q", copied, m.selectionNotice)
	}
	if !strings.Contains(m.View().Content, "No machines registered.") {
		t.Fatal("display did not resume after release")
	}
}

func TestSelectionCopyFailureCanRetry(t *testing.T) {
	attempts := 0
	m := view{ctx: context.Background(), width: 80, height: 10, selectedText: "selected only",
		options: Options{Copy: func(context.Context, string) error {
			attempts++
			if attempts == 1 {
				return errors.New("clipboard unavailable")
			}
			return nil
		}},
	}
	next, cmd := m.copySelection()
	m = next.(view)
	next, _ = m.Update(cmd())
	m = next.(view)
	if !strings.Contains(m.View().Content, "Copy failed: clipboard unavailable") {
		t.Fatal("clipboard failure was not visible")
	}
	next, cmd = m.Update(tea.KeyPressMsg{Code: 'c', Text: "c"})
	m = next.(view)
	if cmd == nil {
		t.Fatal("c did not retry selection copy")
	}
	next, _ = m.Update(cmd())
	m = next.(view)
	if attempts != 2 || m.selectionNotice != "Selection copied." {
		t.Fatal("retry did not report success")
	}
}

func TestSelectionClickResizeAndUnavailableClipboard(t *testing.T) {
	m := view{ctx: context.Background(), width: 80, height: 10}
	if m.View().MouseMode != tea.MouseModeNone {
		t.Fatal("clipboard-less dashboard must preserve native mouse handling")
	}
	next, _ := m.Update(tea.MouseClickMsg{Button: tea.MouseLeft})
	m = next.(view)
	if m.selection != nil {
		t.Fatal("unavailable clipboard started selection")
	}
	m.options.Copy = func(context.Context, string) error { t.Fatal("click copied text"); return nil }
	next, _ = m.Update(tea.MouseClickMsg{Button: tea.MouseLeft})
	m = next.(view)
	next, cmd := m.Update(tea.MouseReleaseMsg{Button: tea.MouseLeft})
	m = next.(view)
	if cmd != nil || m.selectedText != "" {
		t.Fatal("single click copied text")
	}
	next, _ = m.Update(tea.MouseClickMsg{X: -10, Y: -10, Button: tea.MouseLeft})
	m = next.(view)
	next, _ = m.Update(tea.MouseMotionMsg{X: 1000, Y: 1000, Button: tea.MouseLeft})
	m = next.(view)
	_ = m.View() // Out-of-screen coordinates must remain safe.
	next, _ = m.Update(tea.WindowSizeMsg{Width: 30, Height: 5})
	m = next.(view)
	if m.selection != nil {
		t.Fatal("resize retained selection with stale cell coordinates")
	}
}

func TestSelectionHighlightsWholeWideCharacters(t *testing.T) {
	m := view{width: 20, height: 3, selection: &textSelection{
		lines: []string{"A界B"}, start: selectionPoint{2, 0}, end: selectionPoint{3, 0},
	}}
	if got := ansi.Strip(m.View().Content); got != "A界B" {
		t.Fatalf("highlight changed wide-character layout: %q", got)
	}
}

func TestSelectionInDialogsAndScrolledView(t *testing.T) {
	for _, dialog := range []string{"main", "invitation", "manager"} {
		t.Run(dialog, func(t *testing.T) {
			var copied string
			m := view{ctx: context.Background(), width: 80, height: 20, offset: 4, xOffset: 5,
				snapshot: model.PoolActivitySnapshot{ObservedAt: time.Now(), Nodes: []model.NodeActivitySnapshot{{Name: "visible-worker"}}},
				options:  Options{Copy: func(_ context.Context, text string) error { copied = text; return nil }},
			}
			switch dialog {
			case "invitation":
				m.wizard = &registration{step: "done"}
			case "manager":
				m.manager = &machineManager{phase: "list", nodes: m.snapshot.Nodes}
			}
			lines := strings.Split(m.View().Content, "\n")
			y := 0
			for y < len(lines)-1 && ansi.StringWidth(lines[y]) < 2 {
				y++
			}
			next, _ := m.Update(tea.MouseClickMsg{X: 0, Y: y, Button: tea.MouseLeft})
			m = next.(view)
			next, cmd := m.Update(tea.MouseReleaseMsg{X: 79, Y: y, Button: tea.MouseLeft})
			m = next.(view)
			if cmd == nil {
				t.Fatal("visible line was not selected")
			}
			_, _ = m.Update(cmd())
			if want := strings.TrimRight(ansi.Strip(lines[y]), " "); copied != want {
				t.Fatalf("copied %q, want displayed line %q", copied, want)
			}
		})
	}
}

func TestSelectionCancellationAndCopyLifetime(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	m := view{ctx: ctx, width: 80, height: 20,
		options: Options{Copy: func(ctx context.Context, _ string) error { return ctx.Err() }},
	}
	next, _ := m.Update(tea.MouseClickMsg{Button: tea.MouseLeft})
	m = next.(view)
	next, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	m = next.(view)
	if cmd != nil || m.selection != nil {
		t.Fatal("Escape must cancel the drag without quitting or copying")
	}
	m.wizard = &registration{step: "copying"}
	next, _ = m.Update(tea.MouseClickMsg{Button: tea.MouseLeft})
	m = next.(view)
	if m.selection != nil {
		t.Fatal("selection overlapped invitation clipboard copy")
	}
	m.wizard = nil
	m.selectedText = "previous selection"
	next, cmd = m.Update(tea.KeyPressMsg{Code: 'c', Text: "c"})
	m = next.(view)
	cancel()
	if msg := cmd().(selectionCopyResult); !errors.Is(msg.err, context.Canceled) {
		t.Fatalf("copy did not inherit dashboard cancellation: %v", msg.err)
	}
	// Ctrl+C remains quit even after a successful automatic copy.
	m.selectionCopying = false
	_, cmd = m.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Fatal("Ctrl+C must still quit after selecting text")
	}
}

func TestSelectionCapturesDisplayedClipboardNotice(t *testing.T) {
	var copied string
	m := view{ctx: context.Background(), width: 80, height: 10, selectionNotice: "Selection copied.",
		options: Options{Copy: func(_ context.Context, text string) error { copied = text; return nil }},
	}
	next, _ := m.Update(tea.MouseClickMsg{X: 0, Y: 9, Button: tea.MouseLeft})
	m = next.(view)
	next, cmd := m.Update(tea.MouseReleaseMsg{X: 79, Y: 9, Button: tea.MouseLeft})
	m = next.(view)
	if cmd == nil {
		t.Fatal("displayed notice was not selectable")
	}
	_, _ = m.Update(cmd())
	if copied != "Selection copied." {
		t.Fatalf("copied hidden footer instead of displayed text: %q", copied)
	}
}
