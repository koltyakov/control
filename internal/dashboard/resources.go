package dashboard

import (
	"fmt"
	"math"
	"strings"

	"github.com/charmbracelet/x/ansi"
	"github.com/koltyakov/control/internal/model"
)

// resources keeps all three gateway resources on one row. Width, rather than
// changing sample values, determines bar size and whether byte totals fit.
func (o renderOptions) resources(b *strings.Builder, cpu string, s *model.SystemInfo) {
	values := o.width == 0 || o.width >= 106
	barWidth := 12
	if o.width > 0 {
		reserved := 38
		if values {
			reserved = 90
		}
		barWidth = max(0, min(barWidth, (o.width-reserved)/2))
	}
	var diskUsed, diskTotal uint64
	if len(s.Disks) > 0 && s.Disks[0].Error == "" {
		diskUsed, diskTotal = s.Disks[0].UsedBytes, s.Disks[0].TotalBytes
	}
	line := "CPU " + cpu + " · " + o.resource("RAM", s.MemoryUsedBytes, s.MemoryTotalBytes, barWidth, values) + " · " + o.resource("Disk", diskUsed, diskTotal, barWidth, values)
	if o.width > 0 {
		line = ansi.Truncate(line, o.width, "…")
	}
	b.WriteString(line + "\n")
}

func (o renderOptions) resource(label string, used, total uint64, barWidth int, values bool) string {
	percent, usage := "?", "unavailable"
	bar := paint(strings.Repeat("·", barWidth), "2", o.color)
	if total > 0 {
		ratio := float64(min(used, total)) / float64(total)
		filled := int(math.Round(ratio * float64(barWidth)))
		style := "37"
		if ratio >= 0.95 {
			style = "31"
		} else if ratio >= 0.8 {
			style = "33"
		}
		bar = paint(strings.Repeat("█", filled), style, o.color) + paint(strings.Repeat("░", barWidth-filled), "2", o.color)
		percent = fmt.Sprintf("%.0f%%", ratio*100)
		usage = bytes(used) + paint("/", "90", o.color) + paint(bytes(total), "2", o.color)
	}
	line := clean(label) + " "
	if barWidth > 0 {
		line += bar + " "
	}
	line += paint(percent, "36", o.color)
	if values {
		line += " " + usage
	}
	return line
}
