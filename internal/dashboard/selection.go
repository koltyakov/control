package dashboard

import (
	"context"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

type selectionPoint struct{ x, y int }

type textSelection struct {
	lines      []string
	start, end selectionPoint
}

type selectionCopyResult struct{ err error }

func (m view) View() tea.View {
	v := m.contentView()
	if m.options.Copy != nil {
		v.MouseMode = tea.MouseModeCellMotion
	}
	if m.selection != nil {
		lines := append([]string(nil), m.selection.lines...)
		start, end := m.selection.bounds()
		if start != end {
			for y := start.y; y <= end.y; y++ {
				left, right := m.selection.columns(y, start, end)
				text, left, right := selectedRange(lines[y], left, right)
				lines[y] = ansi.Cut(lines[y], 0, left) + "\x1b[7m" + text + "\x1b[0m" + ansi.TruncateLeft(lines[y], right, "")
			}
		}
		v.Content = strings.Join(lines, "\n")
	}
	if m.selectionNotice != "" && m.height > 0 {
		lines := strings.Split(v.Content, "\n")
		for len(lines) < m.height {
			lines = append(lines, "")
		}
		lines[m.height-1] = ansi.Truncate(m.selectionNotice, max(1, m.width), "…")
		v.Content = strings.Join(lines, "\n")
	}
	return v
}

func (m view) updateSelection(msg tea.MouseMsg) (tea.Model, tea.Cmd) {
	if m.options.Copy == nil || m.selectionCopying {
		return m, nil
	}
	if m.wizard != nil && (m.wizard.step == "creating" || m.wizard.step == "copying") {
		return m, nil
	}
	mouse := msg.Mouse()
	switch msg.(type) {
	case tea.MouseClickMsg:
		if mouse.Button != tea.MouseLeft {
			return m, nil
		}
		lines := strings.Split(m.View().Content, "\n")
		m.selectionNotice = ""
		m.selectedText = ""
		point := selectionPoint{max(0, min(mouse.X, max(0, m.width-1))), max(0, min(mouse.Y, len(lines)-1))}
		m.selection = &textSelection{lines: lines, start: point, end: point}
	case tea.MouseWheelMsg:
		if m.selection != nil || m.wizard != nil || m.manager != nil {
			return m, nil
		}
		switch mouse.Button {
		case tea.MouseWheelUp:
			m.offset -= 3
		case tea.MouseWheelDown:
			m.offset += 3
		}
		lines := strings.Split(m.body(time.Now()), "\n")
		m.offset = max(0, min(m.offset, len(lines)-m.pageSize()))
	case tea.MouseMotionMsg, tea.MouseReleaseMsg:
		if m.selection == nil || (mouse.Button != tea.MouseLeft && mouse.Button != tea.MouseNone) {
			return m, nil
		}
		selection := *m.selection
		selection.end = selectionPoint{max(0, min(mouse.X, max(0, m.width-1))), max(0, min(mouse.Y, len(selection.lines)-1))}
		m.selection = &selection
		if _, released := msg.(tea.MouseReleaseMsg); released {
			m.selectedText = selection.text()
			m.selection = nil
			if m.selectedText != "" {
				return m.copySelection()
			}
		}
	}
	return m, nil
}

func (m view) copySelection() (tea.Model, tea.Cmd) {
	if m.selectionCopying || m.options.Copy == nil {
		return m, nil
	}
	m.selectionCopying = true
	m.selectionNotice = "Copying selection…"
	text := m.selectedText
	return m, func() tea.Msg {
		ctx, cancel := context.WithTimeout(m.ctx, 5*time.Second)
		defer cancel()
		return selectionCopyResult{m.options.Copy(ctx, text)}
	}
}

func (s textSelection) bounds() (selectionPoint, selectionPoint) {
	start, end := s.start, s.end
	if start.y > end.y || (start.y == end.y && start.x > end.x) {
		start, end = end, start
	}
	return start, end
}

func (s textSelection) columns(y int, start, end selectionPoint) (int, int) {
	left, right := 0, ansi.StringWidth(s.lines[y])
	if y == start.y {
		left = start.x
	}
	if y == end.y {
		right = end.x + 1
	}
	return left, right
}

func (s textSelection) text() string {
	start, end := s.bounds()
	if start == end {
		return ""
	}
	lines := make([]string, 0, end.y-start.y+1)
	for y := start.y; y <= end.y; y++ {
		left, right := s.columns(y, start, end)
		lines = append(lines, strings.TrimRight(selectedCells(s.lines[y], left, right), " "))
	}
	return strings.TrimRight(strings.Join(lines, "\n"), "\n")
}

// Select whole graphemes when either cell of a wide character is dragged over.
func selectedCells(line string, left, right int) string {
	text, _, _ := selectedRange(line, left, right)
	return text
}

func selectedRange(line string, left, right int) (string, int, int) {
	var b strings.Builder
	column := 0
	first, last := left, right
	for text := ansi.Strip(line); text != ""; {
		cluster, width := ansi.FirstGraphemeCluster(text, ansi.GraphemeWidth)
		if column < right && column+width > left {
			if b.Len() == 0 {
				first = column
			}
			last = column + width
			b.WriteString(cluster)
		}
		column += width
		text = text[len(cluster):]
	}
	return b.String(), first, last
}
