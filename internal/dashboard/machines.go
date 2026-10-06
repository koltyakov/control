package dashboard

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/koltyakov/control/internal/model"
)

type ManageMachine func(context.Context, string, string) error
type machineManager struct {
	id                     int
	nodes                  []model.NodeActivitySnapshot
	selected               int
	phase, action, message string
}
type machineActionResult struct {
	id  int
	err error
}

func (m view) manageMachine() tea.Cmd {
	id, action, generation := m.manager.nodes[m.manager.selected].ID, m.manager.action, m.manager.id
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(m.ctx, 15*time.Second)
		defer cancel()
		return machineActionResult{generation, m.options.Manage(ctx, id, action)}
	}
}

func (m view) updateManager(msg tea.Msg) (tea.Model, tea.Cmd) {
	w := m.manager
	if w == nil {
		return m, nil
	}
	switch msg := msg.(type) {
	case machineActionResult:
		if msg.id != w.id || w.phase != "pending" {
			return m, nil
		}
		w.phase = "done"
		if msg.err != nil {
			w.phase, w.message = "error", msg.err.Error()
		}
		if !m.busy {
			m.busy = true
			return m, m.poll()
		}
	case tea.KeyPressMsg:
		key := msg.String()
		if key == "ctrl+c" {
			return m, tea.Quit
		}
		if key == "esc" && w.phase != "pending" {
			switch w.phase {
			case "actions", "confirm":
				w.phase = "list"
			default:
				m.manager = nil
			}
			return m, nil
		}
		switch w.phase {
		case "list":
			switch key {
			case "up", "k":
				w.selected = (w.selected + len(w.nodes) - 1) % len(w.nodes)
			case "down", "j":
				w.selected = (w.selected + 1) % len(w.nodes)
			case "enter":
				w.phase = "actions"
			}
		case "actions":
			switch key {
			case "e":
				w.action = "enable"
			case "d":
				w.action = "disable"
			case "u":
				w.action, w.phase = "unregister", "confirm"
				return m, nil
			default:
				return m, nil
			}
			w.phase = "pending"
			return m, m.manageMachine()
		case "confirm":
			if key == "enter" {
				w.phase = "pending"
				return m, m.manageMachine()
			}
		case "done", "error":
			if key == "enter" {
				m.manager = nil
			}
		}
	}
	return m, nil
}

func (m view) managerView() tea.View {
	w := m.manager
	n := w.nodes[w.selected]
	hint := func(key, description string) string {
		return paint(key, "37", m.color) + paint(" "+description, "2", m.color)
	}
	sep := paint(" · ", "2", m.color)
	lines := []string{paint("Manage machines", "1;37", m.color), ""}
	footer := hint("Esc", "close")
	switch w.phase {
	case "list":
		visible := max(1, m.height-8)
		start := max(0, w.selected-visible+1)
		for i := start; i < min(len(w.nodes), start+visible); i++ {
			prefix, style := "  ", "37"
			if i == w.selected {
				prefix, style = paint("❯ ", "1;35", m.color), "1;36"
			}
			status, statusStyle := "enabled", "32"
			if w.nodes[i].Disabled {
				status, statusStyle = "disabled", "33"
			}
			lines = append(lines, prefix+paint(clean(w.nodes[i].Name), style, m.color)+"  "+paint(status, statusStyle, m.color))
		}
		footer = hint("↑↓", "select") + sep + hint("Enter", "actions") + sep + footer
	case "actions":
		lines = append(lines, paint(clean(n.Name), "1;36", m.color), paint(clean(n.Software.Version), "2", m.color), "", hint("e", "enable new work"), hint("d", "disable new work"), hint("u", "unregister machine"))
		footer = hint("Esc", "back")
	case "confirm":
		lines = append(lines, paint("Unregister "+clean(n.Name)+"?", "33", m.color))
		if n.Online {
			lines = append(lines, "The registration will be removed and disconnected.", "This identity cannot rejoin the fleet.")
		} else {
			lines = append(lines, "The offline registration will be removed immediately.", "This identity cannot rejoin the fleet.")
		}
		lines = append(lines, "Lifecycle-capable nodes stop on their next gateway contact.", "Older nodes must be stopped locally or replaced by an installer.")
		footer = hint("Enter", "unregister") + sep + hint("Esc", "back")
	case "pending":
		lines = append(lines, paint("Requesting "+w.action+" for "+clean(n.Name)+"…", "36", m.color))
		footer = ""
	case "done":
		message := fmt.Sprintf("%s requested for %s.", strings.ToUpper(w.action[:1])+w.action[1:], clean(n.Name))
		lines = append(lines, paint(message, "32", m.color), paint("Nodes apply changes when they contact the gateway.", "2", m.color))
		footer = hint("Enter / Esc", "close")
	case "error":
		lines = append(lines, paint(clean(w.message), "33", m.color), "Check the machine's state before trying again.")
		footer = hint("Enter / Esc", "close")
	}
	width := max(1, min(76, m.width))
	if width < 8 || m.height < 6 {
		v := tea.NewView(ansi.Truncate("Manage machines · Esc close", width, "…"))
		v.AltScreen = true
		return v
	}
	inner := width - 4
	var wrapped []string
	for _, line := range lines {
		wrapped = append(wrapped, strings.Split(ansi.Wrap(line, inner, ""), "\n")...)
	}
	if len(wrapped) > m.height-4 {
		wrapped = append(wrapped[:m.height-5], "…")
	}
	border := func(text string) string { return paint(text, "90", m.color) }
	row := func(text string) string {
		return border("│") + " " + text + strings.Repeat(" ", max(0, inner-ansi.StringWidth(text))) + " " + border("│")
	}
	box := []string{border("┌" + strings.Repeat("─", width-2) + "┐")}
	for _, line := range wrapped {
		box = append(box, row(line))
	}
	if footer != "" {
		box = append(box, border("├"+strings.Repeat("─", width-2)+"┤"), row(ansi.Truncate(footer, inner, "…")))
	}
	box = append(box, border("└"+strings.Repeat("─", width-2)+"┘"))
	left := strings.Repeat(" ", max(0, (m.width-width)/2))
	for i := range box {
		box[i] = left + box[i]
	}
	v := tea.NewView(strings.Repeat("\n", max(0, (m.height-len(box))/2)) + strings.Join(box, "\n"))
	v.AltScreen = true
	return v
}
