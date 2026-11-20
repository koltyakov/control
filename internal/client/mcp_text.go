package client

// toolText renders JSON for an MCP text result. Go's encoder, including a
// worker's, escapes <, > and & as six-character \u sequences. Commands, HTML
// and XML use them often, so the escapes inflate agent context without
// changing the decoded value. Only those escapes inside strings are restored;
// the input must be valid JSON, and all other bytes are copied unchanged.
func toolText(data []byte) string {
	out := make([]byte, 0, len(data))
	inString := false
	for i := 0; i < len(data); i++ {
		b := data[i]
		if !inString {
			inString = b == '"'
			out = append(out, b)
			continue
		}
		switch b {
		case '"':
			inString = false
		case '\\':
			if i+5 < len(data) && data[i+1] == 'u' && data[i+2] == '0' && data[i+3] == '0' {
				if r, ok := htmlEscape(data[i+4], data[i+5]); ok {
					out = append(out, r)
					i += 5
					continue
				}
			}
			// Copy the escape pair whole so an escaped backslash or quote is
			// never mistaken for the start of another sequence.
			if i+1 < len(data) {
				out = append(out, b, data[i+1])
				i++
				continue
			}
		}
		out = append(out, b)
	}
	return string(out)
}

func htmlEscape(high, low byte) (byte, bool) {
	switch {
	case high == '3' && (low == 'c' || low == 'C'):
		return '<', true
	case high == '3' && (low == 'e' || low == 'E'):
		return '>', true
	case high == '2' && low == '6':
		return '&', true
	}
	return 0, false
}
