package dashboard

import (
	"strings"

	"github.com/charmbracelet/x/ansi"
)

// Keep original diagnostics in details and plain text, without exposing terminal
// controls. Compact messages describe observation failures, not machine outages.
func (o renderOptions) errorMessage(message, operation string) string {
	message = clean(message)
	if o.details {
		return message
	}
	lower := strings.ToLower(message)
	switch {
	case strings.Contains(lower, "context deadline exceeded"), strings.Contains(lower, "i/o timeout"), strings.Contains(lower, "client.timeout exceeded"):
		return operation + " timed out"
	case strings.Contains(lower, "context canceled"), strings.Contains(lower, "context cancelled"):
		return operation + " cancelled"
	case strings.Contains(lower, "not authorized"), strings.Contains(lower, "permission denied"), strings.Contains(lower, "unauthorized"), strings.Contains(lower, "forbidden"):
		return operation + " denied"
	case strings.Contains(lower, "connection refused"):
		return "Connection refused"
	default:
		return message
	}
}

func (o renderOptions) warning(b *strings.Builder, node, message string) {
	text := "  " + paint(clean(node), "33", o.color) + paint(" · "+o.errorMessage(message, "Activity request"), "2", o.color)
	if !o.details && o.width > 0 {
		text = ansi.Truncate(text, o.width, "…")
	}
	o.line(b, text)
}
