// Package desktop defines the opt-in native agent-desktop helper contract.
package desktop

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/koltyakov/control/internal/secrets"
)

// Request is reference-only until the destination resolves a Secret.
type Request struct {
	Command  string  `json:"command"`
	App      string  `json:"app,omitempty"`
	Ref      string  `json:"ref,omitempty"`
	Text     *string `json:"text,omitempty"`
	Secret   string  `json:"secret,omitempty"`
	MaxDepth int     `json:"maxDepth,omitempty"`
	// CLI arguments are forwarded verbatim, without a shell or command whitelist.
	Args  []string `json:"args,omitempty"`
	Stdin *string  `json:"stdin,omitempty"`
}

var refPattern = regexp.MustCompile(`^@[a-zA-Z0-9_-]{3,64}:e[1-9][0-9]{0,9}$`)

// Keep desktop CLI input within the existing task argument budget. Platform
// process-argument limits can be lower; stdin is available where upstream supports it.
const maxCLIInput = 1 << 20

var resolved = func() *jsonschema.Resolved {
	b, _ := json.Marshal(Schema())
	var s jsonschema.Schema
	if err := json.Unmarshal(b, &s); err != nil {
		panic(err)
	}
	r, err := s.Resolve(nil)
	if err != nil {
		panic(err)
	}
	return r
}()

func validText(s string, limit int) bool {
	return len(s) <= limit && utf8.ValidString(s) && !strings.ContainsRune(s, 0)
}

// Decode rejects unknown fields and invalid combinations without echoing input.
func Decode(args json.RawMessage) (Request, error) {
	var value any
	if json.Unmarshal(args, &value) != nil || resolved.Validate(value) != nil {
		return Request{}, errors.New("invalid desktop request")
	}
	var r Request
	d := json.NewDecoder(bytes.NewReader(args))
	d.DisallowUnknownFields()
	err := d.Decode(&r)
	var extra any
	if err != nil || d.Decode(&extra) != io.EOF || !validText(r.App, 4096) || !validText(r.Ref, 128) {
		return Request{}, errors.New("invalid desktop request")
	}
	valid := false
	switch r.Command {
	case "cli":
		valid = len(r.Args) > 0 && len(r.Args) <= 256
		size := 0
		for _, arg := range r.Args {
			size += len(arg)
			valid = valid && validText(arg, maxCLIInput)
		}
		if r.Stdin != nil {
			size += len(*r.Stdin)
			valid = valid && validText(*r.Stdin, maxCLIInput)
		}
		valid = valid && size <= maxCLIInput
	case "status":
		valid = r.App == "" && r.Ref == "" && r.Text == nil && r.Secret == "" && r.MaxDepth == 0
	case "snapshot":
		valid = r.App != "" && r.Ref == "" && r.Text == nil && r.Secret == "" && r.MaxDepth >= 0 && r.MaxDepth <= 32
	case "click", "clear":
		valid = refPattern.MatchString(r.Ref) && r.App == "" && r.Text == nil && r.Secret == "" && r.MaxDepth == 0
	case "setValue":
		valid = refPattern.MatchString(r.Ref) && r.App == "" && r.MaxDepth == 0 &&
			((r.Text != nil && r.Secret == "" && validText(*r.Text, secrets.MaxValue)) ||
				(r.Text == nil && secrets.ValidName(r.Secret)))
	}
	if !valid {
		return Request{}, errors.New("invalid desktop command or fields")
	}
	return r, nil
}

// Packet uses an anonymous stdin pipe, never argv, environment, or a spool file.
// All fields are UTF-8 strings prefixed with a big-endian uint32 byte length.
func Packet(r Request, home string) []byte {
	var b bytes.Buffer
	b.WriteString("CDSK0001")
	text := ""
	if r.Text != nil {
		text = *r.Text
	}
	for _, field := range []string{r.Command, r.App, r.Ref, text, home} {
		_ = binary.Write(&b, binary.BigEndian, uint32(len(field)))
		b.WriteString(field)
	}
	depth := r.MaxDepth
	if depth == 0 {
		depth = 10
	}
	_ = binary.Write(&b, binary.BigEndian, uint32(depth))
	return b.Bytes()
}

func Schema() map[string]any {
	return SchemaWithCLI(true, true)
}

// SchemaWithCLI advertises only configured backends on an execution node.
// The client uses Schema so it can route either backend after discovery.
func SchemaWithCLI(native, cli bool) map[string]any {
	text := map[string]any{"type": "string", "maxLength": 4096}
	ref := map[string]any{"type": "string", "pattern": refPattern.String(), "maxLength": 128}
	variant := func(command string, fields map[string]any, required ...string) map[string]any {
		p := map[string]any{"command": map[string]any{"const": command}}
		for k, v := range fields {
			p[k] = v
		}
		return map[string]any{"type": "object", "additionalProperties": false, "properties": p, "required": append([]string{"command"}, required...)}
	}
	set := variant("setValue", map[string]any{"ref": ref, "text": text, "secret": map[string]any{"type": "string", "pattern": "^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,127}$"}}, "ref")
	set["oneOf"] = []any{map[string]any{"required": []string{"text"}}, map[string]any{"required": []string{"secret"}}}
	variants := []any{}
	if native {
		variants = append(variants,
			variant("status", nil),
			variant("snapshot", map[string]any{"app": map[string]any{"type": "string", "minLength": 1, "maxLength": 4096}, "maxDepth": map[string]any{"type": "integer", "minimum": 1, "maximum": 32}}, "app"),
			variant("click", map[string]any{"ref": ref}, "ref"),
			variant("clear", map[string]any{"ref": ref}, "ref"), set,
		)
	}
	if cli {
		variants = append(variants, variant("cli", map[string]any{
			"args":  map[string]any{"type": "array", "minItems": 1, "maxItems": 256, "items": map[string]any{"type": "string", "maxLength": maxCLIInput}},
			"stdin": map[string]any{"type": "string", "maxLength": maxCLIInput},
		}, "args"))
	}
	return map[string]any{"oneOf": variants}
}
