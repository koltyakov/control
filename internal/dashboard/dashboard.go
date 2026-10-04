// Package dashboard presents pool activity without tying monitoring to a single
// CLI invocation or task owner. Refreshes request snapshots, not event streams.
package dashboard

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"
	"time"
	"unicode"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	"github.com/koltyakov/control/internal/model"
)

type Fetch func(context.Context) (model.PoolActivitySnapshot, error)

type Options struct {
	Interval time.Duration
	Timeout  time.Duration
	Output   io.Writer
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
}

func Run(ctx context.Context, fetch Fetch, options Options) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	m := view{ctx: ctx, fetch: fetch, options: options, width: 120, height: 30, busy: true}
	_, err := tea.NewProgram(m, tea.WithAltScreen(), tea.WithContext(ctx), tea.WithOutput(options.Output)).Run()
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
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
	case tea.KeyMsg:
		switch msg.String() {
		case "q", "ctrl+c", "esc":
			return m, tea.Quit
		case "r":
			if !m.busy {
				m.busy = true
				return m, m.poll()
			}
		case "up", "k":
			m.offset--
		case "down", "j":
			m.offset++
		case "left", "h":
			m.xOffset -= 8
		case "right", "l":
			m.xOffset += 8
		case "pgup":
			m.offset -= max(1, m.height-4)
		case "pgdown":
			m.offset += max(1, m.height-4)
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
	m.offset = max(0, min(m.offset, len(strings.Split(Render(m.snapshot, time.Now()), "\n"))-max(1, m.height-3)))
	widest := 0
	for _, line := range strings.Split(Render(m.snapshot, time.Now()), "\n") {
		widest = max(widest, ansi.StringWidth(line))
	}
	m.xOffset = max(0, min(m.xOffset, max(0, widest-m.width)))
	return m, nil
}

func (m view) View() string {
	status := "Control dashboard"
	if m.busy {
		status += " | refreshing"
	}
	if m.err != nil {
		status += " | STALE: " + clean(m.err.Error())
	}
	lines := strings.Split(Render(m.snapshot, time.Now()), "\n")
	page := max(1, m.height-3)
	offset := max(0, min(m.offset, max(0, len(lines)-page)))
	visible := []string{ansi.Truncate(status, m.width, "...")}
	for _, line := range lines[offset:min(len(lines), offset+page)] {
		visible = append(visible, ansi.Cut(line, m.xOffset, m.xOffset+m.width))
	}
	for len(visible) < max(1, m.height-1) {
		visible = append(visible, "")
	}
	footer := fmt.Sprintf("q quit | r refresh | arrows scroll | PgUp/PgDn | rows %d-%d/%d col %d | metrics cached", offset+1, min(len(lines), offset+page), len(lines), m.xOffset+1)
	visible = append(visible, ansi.Truncate(footer, m.width, "..."))
	return strings.Join(visible, "\n")
}

// Render is also used for plain-text snapshots and keeps remote text out of
// terminal control sequences. JSON output retains the original metadata.
func Render(snapshot model.PoolActivitySnapshot, now time.Time) string {
	var b strings.Builder
	online, active, omitted, observed := 0, 0, 0, 0
	names := map[string]string{}
	for _, n := range snapshot.Nodes {
		names[n.ID] = n.Name
		if n.Online {
			online++
		}
		if n.Status == "ready" {
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
		return "Waiting for the local node and machine directory..."
	}
	fmt.Fprintf(&b, "%s | %d registered | %d online | %d observed | %d in flight\n", snapshot.ObservedAt.Local().Format("2006-01-02 15:04:05"), len(snapshot.Nodes), online, observed, active)
	if omitted > 0 {
		fmt.Fprintf(&b, "Activity detail limit reached: %d active records omitted. Filter with --node.\n", omitted)
	}
	b.WriteString("\nMACHINES\n")
	table := tabwriter.NewWriter(&b, 0, 4, 2, ' ', 0)
	fmt.Fprintln(table, "NAME\tAVAILABILITY\tACTIVE\tOS / ARCH\tCPU USED\tRAM USED / TOTAL\tRAM AVAILABLE\tDISK FREE\tMETRICS AGE\tDIRECT / RELAY")
	for _, n := range snapshot.Nodes {
		status := n.Status
		if n.Status == "ready" {
			status = "idle"
			if n.ActiveCount > 0 {
				status = "busy"
			}
			if n.LeaseOwner != "" {
				status += "/leased"
			}
		}
		count := fmt.Sprint(n.ActiveCount)
		if n.Status != "ready" {
			count = "?"
		}
		osName, cpuUsed, ram, available, free, age := n.OS, "-", "-", "-", "-", "-"
		if s := n.System; s != nil {
			osName += "/" + s.Arch
			if s.CPUUsagePercent != nil {
				cpuUsed = fmt.Sprintf("%.1f%%/%dt", *s.CPUUsagePercent, s.LogicalCPUs)
			} else {
				cpuUsed = fmt.Sprintf("?/%dt", s.LogicalCPUs)
			}
			if s.MemoryTotalBytes > 0 {
				ram = bytes(s.MemoryUsedBytes) + " / " + bytes(s.MemoryTotalBytes)
				available = bytes(s.MemoryAvailableBytes)
			}
			if len(s.Disks) > 0 && s.Disks[0].Error == "" {
				free = bytes(s.Disks[0].FreeBytes)
			}
			if !s.SampledAt.IsZero() {
				age = elapsed(now.Sub(s.SampledAt))
			}
			if n.Status != "ready" {
				age += " (stale)"
			} else if len(s.Errors) > 0 {
				age += " (!)"
			}
		}
		fmt.Fprintf(table, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%d / %d\n", clean(n.Name), status, count, clean(osName), cpuUsed, ram, available, free, age, n.DirectSessions, n.RelaySessions)
	}
	_ = table.Flush()
	for _, n := range snapshot.Nodes {
		if n.Error != "" {
			fmt.Fprintf(&b, "  %s: %s\n", clean(n.Name), clean(n.Error))
		}
		if !n.Online {
			fmt.Fprintf(&b, "  %s last seen: %s\n", clean(n.Name), n.LastSeen.Local().Format(time.RFC3339))
		}
		if n.LeaseOwner != "" {
			fmt.Fprintf(&b, "  %s leased by %s until %s\n", clean(n.Name), name(n.LeaseOwner), n.LeaseExpires.Local().Format(time.RFC3339))
		}
	}
	b.WriteString("\nIN-FLIGHT ACTIVITIES\n")
	table = tabwriter.NewWriter(&b, 0, 4, 2, ' ', 0)
	fmt.Fprintln(table, "NODE\tKIND\tOPERATION\tSTATE / PHASE\tAGE\tPROGRESS\tOWNER\tPEER\tID")
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
			fmt.Fprintf(table, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", clean(n.Name), clean(a.Kind), clean(a.Operation), clean(state), elapsed(now.Sub(a.Started)), progress, name(a.Owner), name(a.Peer), clean(a.ID))
		}
	}
	_ = table.Flush()
	if active == 0 {
		b.WriteString("No observed in-flight activities.\n")
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
		b.WriteString("\nRECENT COMPLETIONS\n")
		table = tabwriter.NewWriter(&b, 0, 4, 2, ' ', 0)
		fmt.Fprintln(table, "NODE\tKIND\tOPERATION\tRESULT\tDURATION\tID")
		for _, row := range recent {
			a := row.activity
			fmt.Fprintf(table, "%s\t%s\t%s\t%s\t%s\t%s\n", clean(row.node), clean(a.Kind), clean(a.Operation), clean(a.State), elapsed(a.Finished.Sub(a.Started)), clean(a.ID))
		}
		_ = table.Flush()
	}
	b.WriteString("\nSYSTEM CAPABILITIES\n")
	for _, n := range snapshot.Nodes {
		if s := n.System; s != nil {
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
