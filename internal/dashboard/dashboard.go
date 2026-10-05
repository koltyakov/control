// Package dashboard presents pool activity without tying monitoring to a single
// CLI invocation or task owner. Refreshes request snapshots, not event streams.
package dashboard

import (
	"context"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"
	"unicode"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/koltyakov/control/internal/model"
)

type Fetch func(context.Context) (model.PoolActivitySnapshot, error)

type Options struct {
	Interval   time.Duration
	Timeout    time.Duration
	Output     io.Writer
	GatewayURL string
	Invite     Invite
	Copy       Copy
	Manage     ManageMachine
}

type result struct {
	snapshot model.PoolActivitySnapshot
	err      error
}
type tick struct{ generation int }

type view struct {
	ctx                   context.Context
	fetch                 Fetch
	options               Options
	snapshot              model.PoolActivitySnapshot
	err                   error
	busy                  bool
	width, height, offset int
	xOffset               int
	generation            int
	details, color        bool
	wizard                *registration
	wizardGeneration      int
	manager               *machineManager
	managerGeneration     int
}

func Run(ctx context.Context, fetch Fetch, options Options) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	m := view{ctx: ctx, fetch: fetch, options: options, width: 120, height: 30, busy: true}
	_, noColor := os.LookupEnv("NO_COLOR")
	m.color = !noColor && os.Getenv("TERM") != "dumb"
	_, err := tea.NewProgram(m, tea.WithContext(ctx), tea.WithOutput(options.Output)).Run()
	if ctx.Err() != nil {
		return nil
	}
	return err
}

func (m view) poll() tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(m.ctx, m.options.Timeout)
		defer cancel()
		snapshot, err := m.fetch(ctx)
		return result{snapshot, err}
	}
}

func (m view) Init() tea.Cmd { return m.poll() }

func (m view) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case machineActionResult:
		return m.updateManager(msg)
	case invitationResult, clipboardResult, tea.PasteMsg:
		return m.updateRegistration(msg)
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.xOffset = 0
	case tea.KeyPressMsg:
		if m.manager != nil {
			return m.updateManager(msg)
		}
		if m.wizard != nil {
			return m.updateRegistration(msg)
		}
		switch msg.String() {
		case "m":
			if m.options.Manage != nil && len(m.snapshot.Nodes) > 0 {
				m.managerGeneration++
				m.manager = &machineManager{id: m.managerGeneration, nodes: append([]model.NodeActivitySnapshot(nil), m.snapshot.Nodes...), phase: "list"}
			}
		case "a":
			if m.options.Invite != nil {
				m.wizardGeneration++
				m.wizard = &registration{id: m.wizardGeneration, step: "name"}
			}
		case "q", "ctrl+c", "esc":
			return m, tea.Quit
		case "r":
			if !m.busy {
				m.busy = true
				return m, m.poll()
			}
		case "d":
			m.details = !m.details
			m.xOffset = 0
		case "up", "k":
			m.offset--
		case "down", "j":
			m.offset++
		case "left", "h":
			m.xOffset -= 8
		case "right", "l":
			m.xOffset += 8
		case "pgup":
			m.offset -= m.pageSize()
		case "pgdown":
			m.offset += m.pageSize()
		case "home", "g":
			m.offset = 0
		case "end", "G":
			m.offset = 1 << 30
		}
	case result:
		m.busy, m.err = false, msg.err
		if msg.err == nil {
			m.snapshot = msg.snapshot
		}
		m.generation++
		generation := m.generation
		return m, tea.Tick(m.options.Interval, func(time.Time) tea.Msg { return tick{generation} })
	case tick:
		if msg.generation != m.generation {
			return m, nil
		}
		if !m.busy {
			m.busy = true
			return m, m.poll()
		}
	}
	lines := strings.Split(m.body(time.Now()), "\n")
	m.offset = max(0, min(m.offset, len(lines)-m.pageSize()))
	widest := 0
	for _, line := range lines {
		widest = max(widest, ansi.StringWidth(line))
	}
	m.xOffset = max(0, min(m.xOffset, max(0, widest-m.width)))
	return m, nil
}

func (m view) View() tea.View {
	if m.manager != nil {
		return m.managerView()
	}
	if m.wizard != nil {
		return m.registrationView()
	}
	lines := strings.Split(m.body(time.Now()), "\n")
	page := m.pageSize()
	offset := max(0, min(m.offset, max(0, len(lines)-page)))
	visible := []string{}
	for _, line := range lines[offset:min(len(lines), offset+page)] {
		visible = append(visible, ansi.Cut(line, m.xOffset, m.xOffset+m.width))
	}
	for len(visible) < max(0, m.height-1) {
		visible = append(visible, "")
	}
	hint := func(key, description string) string {
		return paint(key, "37", m.color) + paint(" "+description, "2", m.color)
	}
	separator := paint(" · ", "2", m.color)
	detailLabel := "details"
	if m.details {
		detailLabel = "compact"
	}
	footer := hint("q", "quit") + separator + hint("r", "refresh") + separator + hint("d", detailLabel)
	if m.options.Invite != nil {
		footer += separator + hint("a", "add machine")
	}
	if m.options.Manage != nil && len(m.snapshot.Nodes) > 0 {
		footer += separator + hint("m", "manage")
	}
	if len(lines) > page {
		footer += separator + hint("↑↓", fmt.Sprintf("%d-%d/%d", offset+1, min(len(lines), offset+page), len(lines)))
	}
	for _, line := range lines {
		if ansi.StringWidth(line) > m.width {
			footer += separator + hint("←→", "scroll")
			break
		}
	}
	if m.err != nil {
		footer = hint("q", "quit") + separator + hint("r", "retry") + separator + paint(clean(m.err.Error()), "2", m.color)
	}
	visible = append(visible, ansi.Truncate(footer, m.width, "…"))
	v := tea.NewView(strings.Join(visible, "\n"))
	v.AltScreen = true
	return v
}

func (m view) pageSize() int {
	return max(0, m.height-1)
}

func (m view) body(now time.Time) string {
	if m.snapshot.ObservedAt.IsZero() {
		status := "Connecting to " + clean(m.options.GatewayURL) + "…"
		if m.err != nil {
			status = "Gateway unavailable · " + clean(m.options.GatewayURL)
		}
		return ansi.Truncate(status, max(1, m.width), "…")
	}
	return render(m.snapshot, now, renderOptions{width: max(1, m.width), details: m.details, color: m.color, stale: m.err != nil})
}

// Render is also used for plain-text snapshots and keeps remote text out of
// terminal control sequences. JSON output retains the original metadata.
func Render(snapshot model.PoolActivitySnapshot, now time.Time) string {
	return render(snapshot, now, renderOptions{details: true})
}

func render(snapshot model.PoolActivitySnapshot, now time.Time, options renderOptions) string {
	var b strings.Builder
	online, active, omitted, observed := 0, 0, 0, 0
	names := map[string]string{}
	for _, n := range snapshot.Nodes {
		names[n.ID] = n.Name
		if n.Online {
			online++
		}
		if n.Status == "ready" || n.Status == "summary" {
			observed++
		}
		active += n.ActiveCount
		omitted += n.Omitted
	}
	name := func(id string) string {
		if id == "" {
			return "-"
		}
		if value := names[id]; value != "" {
			return clean(value)
		}
		return clean(id)
	}
	if snapshot.ObservedAt.IsZero() {
		return "Waiting for the gateway..."
	}
	if g := snapshot.Gateway; g != nil {
		title := paint("Gateway", "1", options.color) + "  " + paint(clean(g.URL), "1;36", options.color) + paint("  "+clean(g.Software.Version), "2", options.color)
		up := "Up ?"
		if !g.StartedAt.IsZero() {
			up = "Up " + elapsed(now.Sub(g.StartedAt))
		}
		status := paint("  "+up, "2", options.color)
		if options.stale {
			status += " " + paint("[STALE]", "33", options.color)
		}
		if options.width > 0 {
			// Keep uptime and its tag on the header row, even on narrow screens.
			title = ansi.Truncate(title, max(0, options.width-ansi.StringWidth(status)), "…")
			status = ansi.Truncate(status, options.width, "…")
		}
		b.WriteString(title + status + "\n")
		if s := g.System; s != nil {
			cpu := "?"
			if s.CPUUsagePercent != nil {
				cpu = fmt.Sprintf("%.1f%%", *s.CPUUsagePercent)
			}
			options.resources(&b, cpu, s)
			for _, err := range s.Errors {
				options.line(&b, "Metric unavailable: "+clean(err))
			}
		}
		b.WriteByte('\n')
	}
	if len(snapshot.Nodes) == 0 {
		options.line(&b, "No machines registered.")
		if snapshot.Notice != "" {
			options.line(&b, clean(snapshot.Notice))
		}
		return strings.TrimRight(b.String(), "\n")
	}
	summary := fmt.Sprintf("Machines  %d nodes · %d online", len(snapshot.Nodes), online)
	if observed < online {
		summary += fmt.Sprintf(" · %d observed", observed)
	}
	if active > 0 {
		summary += fmt.Sprintf(" · %d active", active)
	}
	options.line(&b, paint(summary, "1", options.color))
	if snapshot.Notice != "" {
		options.line(&b, clean(snapshot.Notice))
	}
	if omitted > 0 {
		options.line(&b, fmt.Sprintf("%d active records omitted. Filter with --node.", omitted))
	}
	rows := [][]string{}
	for _, n := range snapshot.Nodes {
		status := n.Status
		if n.Status == "ready" || n.Status == "summary" {
			status = "idle"
			if n.ActiveCount > 0 {
				status = "busy"
			}
			if n.LeaseOwner != "" || n.Leased {
				status += "/leased"
			}
		}
		if n.Disabled {
			status = "disabled"
		}
		if n.ControlPending {
			status = "enabling"
			if n.Disabled {
				status = "disabling"
			}
		}
		count := fmt.Sprint(n.ActiveCount)
		if n.Status != "ready" && n.Status != "summary" {
			count = "?"
		}
		if n.Status == "online" || !n.Online {
			count = "-"
		}
		osName, cpuUsed, ram, free := n.OS, "-", "-", "-"
		if s := n.System; s != nil {
			if s.CPUUsagePercent != nil {
				cpuUsed = fmt.Sprintf("%.1f%%", *s.CPUUsagePercent)
			} else {
				cpuUsed = "?"
			}
			if s.MemoryTotalBytes > 0 {
				ram = bytes(s.MemoryUsedBytes) + "/" + bytes(s.MemoryTotalBytes)
			}
			if len(s.Disks) > 0 && s.Disks[0].Error == "" {
				free = bytes(s.Disks[0].FreeBytes)
			}
		}
		connections := "-"
		if n.Status == "ready" {
			connections = fmt.Sprintf("%d/%d", n.DirectSessions, n.RelaySessions)
		}
		version := n.Software.Version
		if version == "" {
			version = "-"
		}
		seen := "-"
		if n.Online {
			seen = "now"
		} else if !n.LastSeen.IsZero() {
			seen = seenAge(now.Sub(n.LastSeen))
		}
		rows = append(rows, []string{n.Name, status, seen, count, osName, version, cpuUsed, ram, free, connections})
	}
	options.table(&b, machineColumns, rows)
	for _, n := range snapshot.Nodes {
		if n.Error != "" {
			options.line(&b, fmt.Sprintf("  %s: %s", clean(n.Name), clean(n.Error)))
		}
		if n.LeaseOwner != "" {
			options.line(&b, fmt.Sprintf("  %s leased by %s until %s", clean(n.Name), name(n.LeaseOwner), n.LeaseExpires.Local().Format(time.RFC3339)))
		}
	}
	rows = nil
	for _, n := range snapshot.Nodes {
		for _, a := range n.Active {
			state := a.State
			if a.Phase != "" {
				state += "/" + a.Phase
			}
			progress := "-"
			if a.Kind == "transfer" {
				progress = bytes(uint64(max(0, a.BytesDone)))
				if a.TotalBytes > 0 {
					progress += fmt.Sprintf(" / %s (%.0f%%)", bytes(uint64(a.TotalBytes)), float64(a.BytesDone)/float64(a.TotalBytes)*100)
				}
			}
			if a.Kind == "tunnel" {
				progress = "tx " + bytes(uint64(max(0, a.BytesSent))) + " rx " + bytes(uint64(max(0, a.BytesReceived)))
			}
			rows = append(rows, []string{n.Name, a.Kind, a.Operation, state, elapsed(now.Sub(a.Started)), progress, name(a.Owner), name(a.Peer), a.ID})
		}
	}
	if len(rows) > 0 {
		options.heading(&b, "Activity")
		options.table(&b, activityColumns, rows)
	}
	type completion struct {
		node     string
		activity model.Activity
	}
	recent := []completion{}
	for _, n := range snapshot.Nodes {
		for _, a := range n.Recent {
			recent = append(recent, completion{n.Name, a})
		}
	}
	if len(recent) > 0 {
		sort.Slice(recent, func(i, j int) bool { return recent[i].activity.Finished.After(recent[j].activity.Finished) })
		options.heading(&b, "Recent")
		rows = nil
		for _, row := range recent {
			a := row.activity
			rows = append(rows, []string{row.node, a.Kind, a.Operation, a.State, elapsed(a.Finished.Sub(a.Started)), a.ID})
		}
		options.table(&b, recentColumns, rows)
	}
	if !options.details {
		return strings.TrimRight(b.String(), "\n")
	}
	hardwareHeading := false
	for _, n := range snapshot.Nodes {
		if s := n.System; s != nil {
			if !hardwareHeading {
				options.heading(&b, "Hardware")
				hardwareHeading = true
			}
			fmt.Fprintf(&b, "%s: %s %s | %s | %d physical / %d logical CPUs\n", clean(n.Name), clean(s.Platform), clean(s.PlatformVersion), clean(s.CPUModel), s.PhysicalCPUs, s.LogicalCPUs)
			for _, disk := range s.Disks {
				fmt.Fprintf(&b, "  %s: %s used / %s total, %s free", clean(disk.Path), bytes(disk.UsedBytes), bytes(disk.TotalBytes), bytes(disk.FreeBytes))
				if disk.Error != "" {
					fmt.Fprintf(&b, " (%s)", clean(disk.Error))
				}
				b.WriteByte('\n')
			}
			for _, err := range s.Errors {
				fmt.Fprintf(&b, "  metric unavailable: %s\n", clean(err))
			}
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

func clean(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.In(r, unicode.Cf) {
			return ' '
		}
		return r
	}, s)
}

func bytes(value uint64) string {
	units := []string{"B", "KiB", "MiB", "GiB", "TiB"}
	v, unit := float64(value), 0
	for v >= 1024 && unit < len(units)-1 {
		v /= 1024
		unit++
	}
	if unit == 0 {
		return fmt.Sprintf("%dB", value)
	}
	return fmt.Sprintf("%.1f%s", v, units[unit])
}

func elapsed(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	return d.Round(time.Second).String()
}

func seenAge(d time.Duration) string {
	switch {
	case d >= 24*time.Hour:
		return fmt.Sprintf("%dd", d/(24*time.Hour))
	case d >= time.Hour:
		return fmt.Sprintf("%dh", d/time.Hour)
	case d >= time.Minute:
		return fmt.Sprintf("%dm", d/time.Minute)
	default:
		return fmt.Sprintf("%ds", max(0, d/time.Second))
	}
}
