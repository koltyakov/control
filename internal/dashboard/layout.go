package dashboard

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/charmbracelet/x/ansi"
)

type renderOptions struct {
	width   int
	details bool
	recent  bool
	color   bool
	stale   bool
}

// Compact tables hide columns from right to left, keeping a visible prefix.
type column struct {
	label string
	width int
}

var machineColumns = []column{
	{"Node", 18}, {"State", 8}, {"Seen", 6}, {"Work", 4}, {"D/R", 5}, {"Tunnels", 9},
	{"CPU", len("100.0%")}, {"RAM", len("1023.9GiB/1023.9GiB")}, {"Disk free", len("1023.9GiB")},
	{"P.Tun.", 9}, {"OS", len("windows")}, {"Version", len("v10.2.3*")},
}

var versionPrefix = regexp.MustCompile(`^v?[0-9]+\.[0-9]+\.[0-9]+`)

func (o renderOptions) version(version string) string {
	if !o.details {
		if base := versionPrefix.FindString(version); base != "" && base != version {
			return base + "*"
		}
	}
	return version
}

var activityColumns = []column{
	{"Node", 18}, {"Kind", 10}, {"Op", 22}, {"State", 18},
	{"Age", 9}, {"Progress", 26}, {"Owner", 18}, {"Peer", 18}, {"ID", 16},
}

var recentColumns = []column{
	{"Node", 18}, {"Kind", 10}, {"Op", 22},
	{"Result", 12}, {"Took", 9}, {"ID", 16},
}

func (o renderOptions) line(b *strings.Builder, text string) {
	if o.width > 0 {
		text = ansi.Wrap(text, o.width, "")
	}
	b.WriteString(text)
	b.WriteByte('\n')
}

func (o renderOptions) heading(b *strings.Builder, text string) {
	b.WriteByte('\n')
	o.line(b, paint(text, "1", o.color))
}

func (o renderOptions) table(b *strings.Builder, columns []column, rows [][]string) {
	widths := make([]int, len(columns))
	selected := make([]int, len(columns))
	for i, col := range columns {
		selected[i] = i
		widths[i] = ansi.StringWidth(col.label)
		for _, row := range rows {
			widths[i] = max(widths[i], ansi.StringWidth(clean(row[i])))
		}
		if !o.details {
			// Fit node names, but keep sampled values from moving columns
			// between refreshes. Long names retain the compact width limit.
			if col.label == "Node" {
				widths[i] = min(max(8, widths[i]), col.width)
			} else {
				widths[i] = col.width
			}
		}
	}
	total := func() int {
		width := max(0, len(selected)-1) * 2
		for _, i := range selected {
			width += widths[i]
		}
		return width
	}
	if o.width > 0 && !o.details {
		for total() > o.width && len(selected) > 1 {
			selected = selected[:len(selected)-1]
		}
		// Tiny terminals still get a bounded view, including wide Unicode text.
		for total() > o.width && len(selected) > 0 {
			widest := selected[0]
			for _, i := range selected {
				if widths[i] > widths[widest] {
					widest = i
				}
			}
			if widths[widest] > 1 {
				widths[widest]--
			} else {
				selected = selected[:len(selected)-1]
			}
		}
	}
	write := func(row []string, header bool) {
		for index, i := range selected {
			text := clean(row[i])
			if !o.details {
				text = ansi.Truncate(text, widths[i], "…")
			}
			padded := text
			if index < len(selected)-1 {
				padded += strings.Repeat(" ", max(0, widths[i]-ansi.StringWidth(text))+2)
			}
			style := ""
			if header {
				style = "2"
			} else if columns[i].label == "State" || columns[i].label == "Result" {
				switch strings.Split(row[i], "/")[0] {
				case "idle", "succeeded":
					style = "32"
				case "busy", "running", "update", "reserved":
					style = "36"
				case "failed", "unavailable", "queued", "disabling", "enabling":
					style = "33"
				case "offline":
					style = "31"
				case "canceled", "disabled":
					style = "2"
				}
			}
			b.WriteString(paint(padded, style, o.color))
		}
		b.WriteByte('\n')
	}
	headers := make([]string, len(columns))
	for i, col := range columns {
		headers[i] = col.label
	}
	write(headers, true)
	for _, row := range rows {
		write(row, false)
	}
}

func paint(text, style string, enabled bool) string {
	if !enabled || style == "" {
		return text
	}
	return fmt.Sprintf("\x1b[%sm%s\x1b[0m", style, text)
}
