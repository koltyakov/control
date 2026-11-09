// Package browser defines the opt-in agent-browser CLI contract.
package browser

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/google/jsonschema-go/jsonschema"
)

const MaxInput = 1 << 20

// Request forwards non-secret input to the installed CLI without a shell.
type Request struct {
	Session string   `json:"session,omitempty"`
	Args    []string `json:"args"`
	Stdin   *string  `json:"stdin,omitempty"`
}

var sessionPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,31}$`)

var resolved = func() *jsonschema.Resolved {
	raw, _ := json.Marshal(Schema())
	var s jsonschema.Schema
	if err := json.Unmarshal(raw, &s); err != nil {
		panic(err)
	}
	r, err := s.Resolve(nil)
	if err != nil {
		panic(err)
	}
	return r
}()

func Schema() map[string]any {
	return map[string]any{
		"type": "object", "additionalProperties": false, "required": []string{"args"},
		"properties": map[string]any{
			"session": map[string]any{"type": "string", "pattern": sessionPattern.String()},
			"args": map[string]any{"type": "array", "minItems": 1, "maxItems": 256,
				"items": map[string]any{"type": "string", "maxLength": MaxInput}},
			"stdin": map[string]any{"type": "string", "maxLength": MaxInput},
		},
	}
}

// Decode reports value-free errors, including for unknown fields and NUL input.
func Decode(raw json.RawMessage) (Request, error) {
	var value any
	if !utf8.Valid(raw) || json.Unmarshal(raw, &value) != nil || resolved.Validate(value) != nil {
		return Request{}, errors.New("invalid browser request")
	}
	var r Request
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	var extra any
	if d.Decode(&r) != nil || d.Decode(&extra) != io.EOF || len(r.Args) < 1 || len(r.Args) > 256 ||
		(r.Session != "" && !sessionPattern.MatchString(r.Session)) {
		return Request{}, errors.New("invalid browser request")
	}
	size := 0
	for _, arg := range r.Args {
		if !validText(arg) {
			return Request{}, errors.New("invalid browser arguments")
		}
		size += len(arg)
	}
	if r.Stdin != nil {
		if !validText(*r.Stdin) {
			return Request{}, errors.New("invalid browser stdin")
		}
		size += len(*r.Stdin)
	}
	if size > MaxInput {
		return Request{}, errors.New("browser input exceeds 1 MiB")
	}
	return r, nil
}

func validText(s string) bool {
	return len(s) <= MaxInput && utf8.ValidString(s) && !strings.ContainsRune(s, 0)
}
