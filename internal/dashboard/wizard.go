package dashboard

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/koltyakov/control/internal/enrollment"
	"github.com/koltyakov/control/internal/model"
)

// Invite creates an invitation for a name, OS, and startup context. A non-empty
// proxy selects a worker by stable ID and one of its advertised endpoints.
type Invite func(context.Context, string, string, string, model.GatewayProxy) (enrollment.Link, error)
type InvitationStatus func(context.Context, string) (enrollment.Invitation, error)

// ProxyWorkers lists workers advertising gateway proxy endpoints.
type ProxyWorkers func(context.Context) ([]model.ProxyWorker, error)
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
	force      bool
	// Connection: 0 direct, 1 through a proxy worker.
	connection int
	workers    []model.ProxyWorker
	worker     int
	address    int
	proxy      model.GatewayProxy
	proxyName  string
}

type proxyWorkersResult struct {
	id      int
	workers []model.ProxyWorker
	err     error
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
var startupModes = []string{"user", "system", "hybrid"}

func (m view) createInvitation() tea.Cmd {
	w := *m.wizard
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(m.ctx, 30*time.Second)
		defer cancel()
		invite := m.options.Invite
		if w.force {
			invite = m.options.ForceInvite
		}
		link, err := invite(ctx, w.name, platformOS[w.platform], startupModes[w.context], w.proxy)
		return invitationResult{w.id, link, err}
	}
}

func (m view) loadProxyWorkers() tea.Cmd {
	id := m.wizard.id
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(m.ctx, 10*time.Second)
		defer cancel()
		workers, err := m.options.ProxyWorkers(ctx)
		return proxyWorkersResult{id, workers, err}
	}
}

// readyProxyWorkers keeps workers with at least one ready endpoint first, so
// selection never lands on a stale, offline, or unavailable proxy.
func readyProxyWorkers(workers []model.ProxyWorker) []model.ProxyWorker {
	var ready []model.ProxyWorker
	for _, worker := range workers {
		if len(worker.ReadyEndpoints()) != 0 {
			ready = append(ready, worker)
		}
	}
	return ready
}

// selectProxyWorker moves to explicit address selection when ambiguous.
func (m view) selectProxyWorker() (tea.Model, tea.Cmd) {
	w := m.wizard
	ready := readyProxyWorkers(w.workers)
	if w.worker >= len(ready) {
		return m, nil
	}
	worker := ready[w.worker]
	endpoints := worker.ReadyEndpoints()
	w.proxyName = worker.Name
	if len(worker.Endpoints) > 1 {
		w.step, w.address, w.err = "address", 0, ""
		return m, nil
	}
	w.proxy = model.GatewayProxy{Worker: worker.ID, URL: endpoints[0].URL}
	w.step, w.err = "creating", ""
	return m, m.createInvitation()
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
			if !w.force && m.options.ForceInvite != nil && errors.Is(msg.err, enrollment.ErrPendingInvitation) {
				w.step = "conflict"
			}
			return m, nil
		}
		w.link, w.step = msg.link, "copying"
		return m, m.copyInvitation()
	case proxyWorkersResult:
		if msg.id != w.id || w.step != "loading-workers" {
			return m, nil
		}
		if msg.err != nil {
			w.step, w.err = "connection", msg.err.Error()
			return m, nil
		}
		w.workers, w.worker, w.step, w.err = msg.workers, 0, "worker", ""
		return m, nil
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
			case "connection":
				w.step, w.err = "context", ""
			case "worker", "loading-workers":
				w.step, w.err = "connection", ""
			case "address":
				w.step, w.err = "worker", ""
			case "context":
				w.step, w.err = "platform", ""
			case "platform":
				w.step, w.err = "name", ""
			case "conflict", "force-confirm":
				w.step, w.err, w.force = "context", "", false
				if m.options.ProxyWorkers != nil {
					w.step = "connection"
				}
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
			case "up", "k":
				w.context = (w.context + len(startupModes) - 1) % len(startupModes)
			case "down", "j", "tab":
				w.context = (w.context + 1) % len(startupModes)
			case "1", "2", "3":
				w.context = int(key[0] - '1')
			case "enter":
				if m.options.ProxyWorkers != nil {
					w.step, w.err = "connection", ""
					return m, nil
				}
				w.step, w.err = "creating", ""
				return m, m.createInvitation()
			}
		case "connection":
			switch key {
			case "up", "k", "down", "j", "tab":
				w.connection = 1 - w.connection
			case "1", "2":
				w.connection = int(key[0] - '1')
			case "enter":
				w.proxy, w.proxyName = model.GatewayProxy{}, ""
				if w.connection == 0 {
					w.step, w.err = "creating", ""
					return m, m.createInvitation()
				}
				w.step, w.err = "loading-workers", ""
				return m, m.loadProxyWorkers()
			}
		case "worker":
			ready := readyProxyWorkers(w.workers)
			switch key {
			case "up", "k":
				if len(ready) > 0 {
					w.worker = (w.worker + len(ready) - 1) % len(ready)
				}
			case "down", "j", "tab":
				if len(ready) > 0 {
					w.worker = (w.worker + 1) % len(ready)
				}
			case "r":
				w.step, w.err = "loading-workers", ""
				return m, m.loadProxyWorkers()
			case "enter":
				return m.selectProxyWorker()
			}
		case "address":
			ready := readyProxyWorkers(w.workers)
			if w.worker >= len(ready) {
				w.step = "worker"
				return m, nil
			}
			endpoints := ready[w.worker].ReadyEndpoints()
			switch key {
			case "up", "k":
				w.address = (w.address + len(endpoints) - 1) % len(endpoints)
			case "down", "j", "tab":
				w.address = (w.address + 1) % len(endpoints)
			case "enter":
				w.proxy = model.GatewayProxy{Worker: ready[w.worker].ID, URL: endpoints[w.address].URL}
				w.step, w.err = "creating", ""
				return m, m.createInvitation()
			}
		case "conflict":
			if key == "f" {
				w.step, w.err = "force-confirm", ""
			}
		case "force-confirm":
			if key == "enter" {
				w.step, w.force, w.err = "creating", true, ""
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
		for i, name := range []string{"User context", "System context", "Hybrid"} {
			prefix, style := "  ", "37"
			if i == w.context {
				prefix, style = paint("❯ ", "1;35", m.color), "1;36"
			}
			add(prefix+paint(fmt.Sprintf("%d  %s", i+1, name), style, m.color), "")
		}
		add("", "")
		switch w.context {
		case 0:
			add("Runs as your user, with access to your files and credentials.", "2")
			add("May go offline after logout. Linux needs linger to stay online.", "33")
		case 2:
			add("User by default, plus an isolated System companion.", "2")
			add("System work needs an exact instruction grant. No automatic fallback.", "2")
			add("User may stop after logout; System remains an explicit target.", "2")
			if w.platform == 1 {
				add("Requires Administrator PowerShell. System runs as LocalSystem.", "33")
			} else {
				add("Run as your user. Only the System installer uses sudo.", "33")
			}
		default:
			add("Starts at boot and stays online after logout.", "2")
			if w.platform == 1 {
				add("Requires Administrator PowerShell. Runs as LocalService.", "33")
			} else {
				add("Requires sudo. Runs as root, without your desktop session.", "33")
			}
		}
		next := "create and copy"
		if m.options.ProxyWorkers != nil {
			next = "next"
		}
		footer = hint("↑↓", "select") + separator + hint("Enter", next) + separator + hint("Esc", "back")
	case "connection":
		add("How "+w.name+" reaches the gateway", "37")
		add("", "")
		for i, name := range []string{"Direct", "Through worker"} {
			prefix, style := "  ", "37"
			if i == w.connection {
				prefix, style = paint("❯ ", "1;35", m.color), "1;36"
			}
			add(prefix+paint(fmt.Sprintf("%d  %s", i+1, name), style, m.color), "")
		}
		add("", "")
		if w.connection == 1 {
			add("Scripts, binaries, and the node's gateway traffic use a CONNECT proxy on a ready worker in this fleet.", "2")
		} else {
			add("The target reaches the gateway URL directly.", "2")
		}
		footer = hint("↑↓", "select") + separator + hint("Enter", "next") + separator + hint("Esc", "back")
	case "loading-workers":
		add("Loading proxy workers…", "36")
		footer = hint("Esc", "back")
	case "worker":
		add("Proxy worker for "+w.name, "37")
		add("", "")
		ready := readyProxyWorkers(w.workers)
		for i, worker := range ready {
			prefix, style := "  ", "37"
			if i == w.worker {
				prefix, style = paint("❯ ", "1;35", m.color), "1;36"
			}
			add(prefix+paint(worker.Name, style, m.color), "")
			for _, endpoint := range worker.Endpoints {
				add("     "+proxyEndpointLine(endpoint, time.Now()), "2")
			}
		}
		for _, worker := range w.workers {
			if len(worker.ReadyEndpoints()) == 0 {
				add("  "+worker.Name+" — not ready", "90")
				for _, endpoint := range worker.Endpoints {
					add("     "+proxyEndpointLine(endpoint, time.Now()), "90")
				}
			}
		}
		if len(ready) == 0 {
			add("No ready proxy workers in this fleet.", "33")
			add("Configure proxyEndpoints on a worker and wait for a ready check.", "2")
			footer = hint("r", "reload") + separator + hint("Esc", "back")
		} else {
			add("", "")
			add("The target must be able to reach the selected address.", "33")
			footer = hint("↑↓", "select") + separator + hint("Enter", "next") + separator + hint("r", "reload") + separator + hint("Esc", "back")
		}
	case "address":
		ready := readyProxyWorkers(w.workers)
		if w.worker < len(ready) {
			add(ready[w.worker].Name+" advertises several addresses. Select one explicitly.", "37")
			add("", "")
			for i, endpoint := range ready[w.worker].ReadyEndpoints() {
				prefix, style := "  ", "37"
				if i == w.address {
					prefix, style = paint("❯ ", "1;35", m.color), "1;36"
				}
				add(prefix+paint(proxyEndpointLine(endpoint, time.Now()), style, m.color), "")
			}
			add("", "")
			add("The target must be able to reach this LAN or VPN address.", "33")
		}
		footer = hint("↑↓", "select") + separator + hint("Enter", "create and copy") + separator + hint("Esc", "back")
	case "creating":
		add("Creating invitation for "+w.name+"…", "36")
		footer = ""
	case "conflict":
		add("A pending invitation reserves "+w.name+".", "33")
		add("Force add to retract its URL and create a new invitation.", "2")
		footer = hint("f", "force add") + separator + hint("Esc", "back")
	case "force-confirm":
		add("Replace the pending invitation for "+w.name+"?", "33")
		add("The old installation URL will stop working immediately.", "2")
		footer = hint("Enter", "retract and create") + separator + hint("Esc", "cancel")
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
			if w.context != 0 {
				shell = "Administrator PowerShell"
			}
		}
		add("Paste into "+shell+" on the target machine.", "2")
		if proxy := w.link.GatewayProxy; proxy != nil {
			name := w.proxyName
			if name == "" {
				name = proxy.Worker
			}
			add("Through "+name+" at "+proxy.URL+". The target must reach that address.", "33")
		}
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

// proxyEndpointLine shows the real address, its label, readiness, age, and
// a sanitized failure reason.
func proxyEndpointLine(endpoint model.ProxyEndpointStatus, now time.Time) string {
	line := endpoint.URL
	if endpoint.Interface != "" {
		line += " (" + endpoint.Interface + ")"
	}
	line += " · " + endpoint.State
	if !endpoint.CheckedAt.IsZero() {
		line += " " + now.Sub(endpoint.CheckedAt).Round(time.Second).String() + " ago"
	}
	if endpoint.Error != "" {
		line += " · " + endpoint.Error
	}
	return line
}
