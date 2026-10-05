package dashboard

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/x/ansi"
)

type renderOptions struct {
	width   int
	details bool
	color   bool
	stale   bool
}

// Higher priorities disappear first. Priority zero columns remain visible,
// with truncated cells when even the essential columns cannot fit.
type column struct {
	label    string
	priority int
	width    int
}

var machineColumns = []column{
	{"Node", 0, 18}, {"State", 0, 14}, {"Seen", 1, 6}, {"Work", 3, 4}, {"OS", 6, 7},
	{"Version", 1, 25},
	{"CPU", 1, 6}, {"RAM", 2, 17}, {"Disk free", 4, 10},
	{"D/R", 8, 5},
}

var activityColumns = []column{
	{"Node", 0, 18}, {"Kind", 5, 10}, {"Op", 0, 22}, {"State", 0, 18},
	{"Age", 1, 9}, {"Progress", 2, 26}, {"Owner", 6, 18}, {"Peer", 7, 18}, {"ID", 8, 16},
}

var recentColumns = []column{
	{"Node", 0, 18}, {"Kind", 5, 10}, {"Op", 0, 22},
	{"Result", 0, 12}, {"Took", 1, 9}, {"ID", 8, 16},
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
			// Choose columns by terminal width, not changing sample values.
			// This keeps headers and cells stationary between refreshes.
			widths[i] = col.width
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
		for total() > o.width {
			drop, priority := -1, 0
			for index, i := range selected {
				if columns[i].priority > priority {
					drop, priority = index, columns[i].priority
				}
			}
			if drop < 0 {
				break
			}
			selected = append(selected[:drop], selected[drop+1:]...)
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
				case "busy", "running":
					style = "36"
				case "failed", "unavailable", "queued", "disabling", "enabling":
					style = "33"
				case "offline", "canceled", "disabled":
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
