package secrets

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/url"
	"sort"
	"strings"
)

// Redactor masks exact values and common text encodings. Arbitrary transforms,
// screenshots, and application-managed files are outside this protection.
type Redactor struct{ replacer *strings.Replacer }

func NewRedactor(values map[string]string) Redactor {
	seen := map[string]bool{}
	for _, value := range values {
		if value == "" {
			continue
		}
		b, _ := json.Marshal(value)
		for _, p := range []string{value, string(b[1 : len(b)-1]), url.QueryEscape(value), url.PathEscape(value), base64.StdEncoding.EncodeToString([]byte(value)), base64.RawStdEncoding.EncodeToString([]byte(value)), base64.URLEncoding.EncodeToString([]byte(value)), base64.RawURLEncoding.EncodeToString([]byte(value))} {
			seen[p] = true
		}
	}
	patterns := make([]string, 0, len(seen))
	for p := range seen {
		patterns = append(patterns, p)
	}
	sort.Slice(patterns, func(i, j int) bool { return len(patterns[i]) > len(patterns[j]) })
	pairs := make([]string, 0, len(patterns)*2)
	for _, p := range patterns {
		pairs = append(pairs, p, "[REDACTED]")
	}
	if len(pairs) == 0 {
		return Redactor{}
	}
	return Redactor{replacer: strings.NewReplacer(pairs...)}
}

func (r Redactor) Text(text string) string {
	if r.replacer == nil {
		return text
	}
	// A replacer scans once so short credentials cannot corrupt the replacement.
	return r.replacer.Replace(text)
}

// JSON redacts decoded strings, preserving JSON syntax even for short values.
func (r Redactor) JSON(data []byte) ([]byte, error) {
	var value any
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	var clean func(any) any
	clean = func(v any) any {
		switch v := v.(type) {
		case string:
			return r.Text(v)
		case []any:
			for i := range v {
				v[i] = clean(v[i])
			}
		case map[string]any:
			m := make(map[string]any, len(v))
			for key, item := range v {
				m[r.Text(key)] = clean(item)
			}
			return m
		}
		return v
	}
	return json.Marshal(clean(value))
}
