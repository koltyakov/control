package dashboard

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/koltyakov/control/internal/enrollment"
)

type Invite func(context.Context, string, string, string) (enrollment.Link, error)
type InvitationStatus func(context.Context, string) (enrollment.Invitation, error)
type Copy func(context.Context, string) error

type registration struct {
	id         int
	step       string
	name       string
	platform   int
	context    int
	link       enrollment.Link
	err        string
	checking   bool
	redeemedID string
}

type invitationResult struct {
	id   int
	link enrollment.Link
	err  error
}
type clipboardResult struct {
	id  int
	err error
}
type invitationStatusResult struct {
	id         int
	invitation enrollment.Invitation
	err        error
}

var registrationName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,62}$`)
var platformNames = []string{"macOS", "Windows", "Linux"}
var platformOS = []string{"darwin", "windows", "linux"}
var startupModes = []string{"user", "system"}

func (m view) createInvitation() tea.Cmd {
	w := *m.wizard
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(m.ctx, 30*time.Second)
		defer cancel()
		link, err := m.options.Invite(ctx, w.name, platformOS[w.platform], startupModes[w.context])
		return invitationResult{w.id, link, err}
	}
}

func (m view) copyInvitation() tea.Cmd {
	w := *m.wizard
	return func() tea.Msg {
		var err error
		if m.options.Copy == nil {
			err = fmt.Errorf("clipboard unavailable")
		} else {
			err = m.options.Copy(m.ctx, w.link.Command)
		}
		return clipboardResult{w.id, err}
	}
}

func (m view) checkInvitation() tea.Cmd {
	w := m.wizard
	if w == nil || (w.step != "done" && w.step != "copying") || w.checking || w.link.ID == "" || m.options.InvitationStatus == nil {
		return nil
	}
	w.checking = true
	id, invitationID := w.id, w.link.ID
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(m.ctx, 10*time.Second)
		defer cancel()
		invitation, err := m.options.InvitationStatus(ctx, invitationID)
		return invitationStatusResult{id, invitation, err}
	}
}

func (m view) updateRegistration(msg tea.Msg) (tea.Model, tea.Cmd) {
	if m.wizard == nil {
		return m, nil
	}
	w := m.wizard
	switch msg := msg.(type) {
	case invitationResult:
		if msg.id != w.id || w.step != "creating" {
			return m, nil
		}
		if msg.err != nil {
			w.step, w.err = "failed", msg.err.Error()
			return m, nil
		}
		w.link, w.step = msg.link, "copying"
		return m, m.copyInvitation()
	case clipboardResult:
		if msg.id != w.id || w.step != "copying" {
			return m, nil
		}
		w.step, w.err = "done", ""
		if msg.err != nil {
			w.err = msg.err.Error()
		}
		return m, m.checkInvitation()
	case invitationStatusResult:
		if msg.id != w.id {
			return m, nil
		}
		w.checking = false
		if msg.err == nil && msg.invitation.ID == w.link.ID && msg.invitation.RedeemedID != "" && !msg.invitation.Revoked {
			first := w.redeemedID == ""
			w.redeemedID = msg.invitation.RedeemedID
			if first && !m.busy {
				m.busy = true
				return m, m.poll()
			}
		} else {
			w.redeemedID = ""
		}
	case tea.PasteMsg:
		if w.step == "name" {
			w.name = registrationInput(w.name, msg.Content)
		}
	case tea.KeyPressMsg:
		key := msg.String()
		if key == "ctrl+c" {
			return m, tea.Quit
		}
		if key == "esc" && w.step != "creating" && w.step != "copying" {
			switch w.step {
			case "context":
				w.step, w.err = "platform", ""
			case "platform":
				w.step, w.err = "name", ""
			default:
				m.wizard = nil
			}
			return m, nil
		}
		switch w.step {
		case "name":
			switch key {
			case "enter":
				if w.name == "" {
					w.name = "auto"
				}
				if !registrationName.MatchString(w.name) {
					w.err = "Use 1–63 letters, digits, dots, hyphens or underscores."
				} else {
					w.step, w.err = "platform", ""
				}
			case "backspace", "ctrl+h":
				if len(w.name) > 0 {
					w.name = w.name[:len(w.name)-1]
				}
			default:
				w.name = registrationInput(w.name, msg.Text)
			}
		case "platform":
			switch key {
			case "up", "k":
				w.platform = (w.platform + 2) % 3
			case "down", "j", "tab":
				w.platform = (w.platform + 1) % 3
			case "1", "2", "3":
				w.platform = int(key[0] - '1')
			case "enter":
				w.step, w.err = "context", ""
			}
		case "context":
			switch key {
			case "up", "down", "k", "j", "tab":
				w.context = (w.context + 1) % len(startupModes)
			case "1", "2":
				w.context = int(key[0] - '1')
			case "enter":
				w.step, w.err = "creating", ""
				return m, m.createInvitation()
			}
		case "done":
			if key == "c" {
				w.step = "copying"
				return m, m.copyInvitation()
			}
			if key == "enter" {
				m.wizard = nil
			}
		}
	}
	return m, nil
}

func registrationInput(name, text string) string {
	for _, r := range text {
		if len(name) >= 63 {
			break
		}
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("._-", r) {
			name += string(r)
		}
	}
	return name
}

func (m view) registrationView() tea.View {
	w := m.wizard
	type styledLine struct{ text, style string }
	content := []styledLine{{"Add machine", "1;37"}, {"", ""}}
	add := func(text, style string) { content = append(content, styledLine{text, style}) }
	hint := func(key, description string) string {
		return paint(key, "37", m.color) + paint(" "+description, "2", m.color)
	}
	separator := paint(" · ", "2", m.color)
	footer := hint("Esc", "close")
	switch w.step {
	case "name":
		add("Machine name, or auto for the target hostname", "37")
		if w.name == "" {
			add(paint("❯ ", "1;35", m.color)+paint("auto (Enter), or e.g. render-01", "2", m.color), "")
		} else {
			add(paint("❯ ", "1;35", m.color)+paint(w.name, "1;36", m.color), "")
		}
		footer = hint("Enter", "next") + separator + footer
	case "platform":
		add(paint("Platform for ", "2", m.color)+paint(w.name, "36", m.color), "")
		add("", "")
		for i, name := range platformNames {
			prefix, style := "  ", "37"
			if i == w.platform {
				prefix, style = paint("❯ ", "1;35", m.color), "1;36"
			}
			add(prefix+paint(fmt.Sprintf("%d  %s", i+1, name), style, m.color), "")
		}
		footer = hint("↑↓", "select") + separator + hint("Enter", "next") + separator + hint("Esc", "back")
	case "context":
		add("Startup context for "+w.name+" on "+platformNames[w.platform], "37")
		add("", "")
		for i, name := range []string{"User context", "System context"} {
			prefix, style := "  ", "37"
			if i == w.context {
				prefix, style = paint("❯ ", "1;35", m.color), "1;36"
			}
			add(prefix+paint(fmt.Sprintf("%d  %s", i+1, name), style, m.color), "")
		}
		add("", "")
		if w.context == 0 {
			add("Runs as your user, with access to your files and credentials.", "2")
			add("May go offline after logout. Linux needs linger to stay online.", "33")
		} else {
			add("Starts at boot and stays online after logout.", "2")
			if w.platform == 1 {
				add("Requires Administrator PowerShell. Runs as LocalService.", "33")
			} else {
				add("Requires sudo. Runs as root, without your desktop session.", "33")
			}
		}
		footer = hint("↑↓", "select") + separator + hint("Enter", "create and copy") + separator + hint("Esc", "back")
	case "creating":
		add("Creating invitation for "+w.name+"…", "36")
		footer = ""
	case "copying":
		add("Copying installation command…", "36")
		footer = ""
	case "done":
		message, style := "Installation command copied.", "32"
		if w.err != "" {
			message, style = "Invitation created. Copy it manually below.", "33"
		}
		add(message, style)
		shell := "Bash"
		if w.platform == 1 {
			shell = "PowerShell as the intended user"
			if w.context == 1 {
				shell = "Administrator PowerShell"
			}
		}
		add("Paste into "+shell+" on the target machine.", "2")
		add("", "")
		add(clean(w.link.Command), "36")
		add("", "")
		add(paint("Expires ", "2", m.color)+paint(w.link.ExpiresAt.Local().Format("15:04"), "33", m.color), "")
		if m.options.InvitationStatus != nil {
			if w.redeemedID != "" {
				add("Enrolled. Waiting for the machine to come online.", "2")
			} else {
				add("Closes automatically when the machine is online.", "2")
			}
		}
		footer = hint("c", "copy again") + separator + hint("Enter / Esc", "close")
	case "failed":
		add("Could not create the invitation.", "31")
		add("Check control machines invites before trying again.", "2")
	}
	if w.err != "" {
		add("", "")
		add(clean(w.err), "33")
	}
	width := min(76, m.width)
	if width < 8 || m.height < 6 {
		v := tea.NewView(ansi.Truncate("Add machine · Esc closes", max(1, m.width), "…"))
		v.AltScreen = true
		return v
	}
	inner := width - 4
	var lines []string
	for _, line := range content {
		for _, wrapped := range strings.Split(ansi.Wrap(line.text, inner, ""), "\n") {
			lines = append(lines, paint(wrapped, line.style, m.color))
		}
	}
	maxLines := m.height - 4
	if len(lines) > maxLines {
		lines = append(lines[:maxLines-1], "…")
	}
	border := func(text string) string { return paint(text, "90", m.color) }
	box := []string{border("┌" + strings.Repeat("─", width-2) + "┐")}
	row := func(text string) string {
		return border("│") + " " + text + strings.Repeat(" ", max(0, inner-ansi.StringWidth(text))) + " " + border("│")
	}
	for _, line := range lines {
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
