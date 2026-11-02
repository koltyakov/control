package runbooks

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"math"
	"path"
	"strings"
	"unicode"
	"unicode/utf8"

	"go.yaml.in/yaml/v3"
)

const maxMetadataBytes = 16 << 10

const ApplicabilityGuidance = "Read requires as agent-friendly guidance for choosing a suitable machine. It can be a plain description, a list, or optional structured details, not a fixed schema or folder classification. Describe only relevant constraints, such as a particular VPN when needed; do not invent mandatory OS, GPU, or generic Control capability restrictions. Interpret the description with the user's task, runbook instructions, and available worker evidence. Unknown facts are not verified facts. Requirements do not run checks, install software, reserve resources, or grant execution authority. "

type metadata struct {
	Title       string `json:"title,omitempty" yaml:"title,omitempty"`
	Description string `json:"description,omitempty" yaml:"description,omitempty"`
	Requires    any    `json:"requires,omitempty" yaml:"requires,omitempty"`
}

// Validate also bounds remote metadata when the full document is omitted.
func ValidateRequirements(r any) error {
	if r == nil {
		return nil
	}
	count := 0
	var inspect func(any, int) error
	inspect = func(value any, depth int) error {
		count++
		if count > 512 || depth > 8 {
			return errors.New("runbook requirements exceed structural limits")
		}
		switch v := value.(type) {
		case map[string]any:
			if len(v) > 64 {
				return errors.New("runbook requirement objects allow at most 64 fields")
			}
			for key, child := range v {
				if key == "" || len(key) > 128 || !utf8.ValidString(key) || strings.TrimSpace(key) != key || strings.ContainsFunc(key, unicode.IsControl) {
					return errors.New("invalid runbook requirement key")
				}
				if err := inspect(child, depth+1); err != nil {
					return err
				}
			}
		case []any:
			if len(v) > 64 {
				return errors.New("runbook requirement lists allow at most 64 items")
			}
			for _, child := range v {
				if err := inspect(child, depth+1); err != nil {
					return err
				}
			}
		case []string:
			if len(v) > 64 {
				return errors.New("runbook requirement lists allow at most 64 items")
			}
			for _, child := range v {
				if err := inspect(child, depth+1); err != nil {
					return err
				}
			}
		case string:
			if len(v) > 2048 || !utf8.ValidString(v) || strings.ContainsFunc(v, func(c rune) bool { return unicode.IsControl(c) && c != '\n' && c != '\t' }) {
				return errors.New("invalid or oversized runbook requirement text")
			}
		case json.Number:
			n, err := v.Float64()
			if err != nil || len(v) > 64 || math.IsNaN(n) || math.IsInf(n, 0) {
				return errors.New("invalid runbook requirement number")
			}
		case float64:
			if math.IsNaN(v) || math.IsInf(v, 0) {
				return errors.New("runbook requirement numbers must be finite")
			}
		case nil, bool, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
		default:
			return errors.New("runbook requirements must contain JSON-compatible values with string keys")
		}
		return nil
	}
	if err := inspect(r, 0); err != nil {
		return err
	}
	b, err := json.Marshal(r)
	if err != nil || len(b) > maxMetadataBytes {
		return errors.New("runbook requirements exceed 16 KiB or contain invalid values")
	}
	return nil
}

func parseMetadata(file, content string) (metadata, string, error) {
	var m metadata
	if strings.EqualFold(path.Ext(file), ".json") {
		var object map[string]json.RawMessage
		// JSON task templates and non-object documents remain valid runbooks.
		if json.Unmarshal([]byte(content), &object) != nil {
			return m, content, nil
		}
		fields := map[string]json.RawMessage{}
		size := 0
		for _, key := range []string{"title", "description", "requires"} {
			if value, ok := object[key]; ok {
				fields[key] = value
				size += len(value)
			}
		}
		if size > maxMetadataBytes {
			return m, "", errors.New("runbook metadata exceeds 16 KiB")
		}
		b, _ := json.Marshal(fields)
		decoder := json.NewDecoder(strings.NewReader(string(b)))
		decoder.DisallowUnknownFields()
		decoder.UseNumber()
		if err := decoder.Decode(&m); err != nil {
			return m, "", errors.New("invalid runbook JSON metadata")
		}
		return checkedMetadata(m, content)
	}
	body := strings.TrimPrefix(content, "\uFEFF")
	first, rest, found := strings.Cut(body, "\n")
	if strings.TrimSpace(first) != "---" {
		return m, body, nil
	}
	if !found {
		return m, "", errors.New("runbook metadata requires a closing --- line")
	}
	headerBytes := 0
	remaining := rest
	for {
		line, next, more := strings.Cut(remaining, "\n")
		if strings.TrimSpace(line) == "---" {
			header := rest[:headerBytes]
			if err := decodeMetadataYAML(header, &m); err != nil {
				return m, "", err
			}
			return checkedMetadata(m, next)
		}
		headerBytes += len(line) + 1
		if headerBytes > maxMetadataBytes {
			return m, "", errors.New("runbook metadata exceeds 16 KiB")
		}
		if !more {
			return m, "", errors.New("runbook metadata requires a closing --- line")
		}
		remaining = next
	}
}

func decodeMetadataYAML(header string, m *metadata) error {
	if strings.TrimSpace(header) == "" {
		return nil
	}
	// Parse the bounded header into nodes before decoding. Reject aliases and
	// merge keys rather than expanding attacker-controlled YAML graphs.
	var document yaml.Node
	decoder := yaml.NewDecoder(strings.NewReader(header))
	if err := decoder.Decode(&document); err != nil {
		return errors.New("invalid runbook YAML metadata")
	}
	count := 0
	var inspect func(*yaml.Node, int) bool
	inspect = func(node *yaml.Node, depth int) bool {
		count++
		if count > 512 || depth > 8 || node.Kind == yaml.AliasNode || node.Tag == "!!merge" {
			return false
		}
		if node.Kind == yaml.MappingNode {
			for i := 0; i < len(node.Content); i += 2 {
				if node.Content[i].Kind != yaml.ScalarNode || node.Content[i].Tag != "!!str" {
					return false
				}
			}
		}
		for _, child := range node.Content {
			if !inspect(child, depth+1) {
				return false
			}
		}
		return true
	}
	if !inspect(&document, 0) {
		return errors.New("runbook metadata exceeds structural limits or contains YAML aliases or merge keys")
	}
	if err := decoder.Decode(new(yaml.Node)); err != io.EOF {
		return errors.New("runbook metadata must contain one YAML document")
	}
	decoder = yaml.NewDecoder(strings.NewReader(header))
	decoder.KnownFields(true)
	if err := decoder.Decode(m); err != nil {
		return errors.New("invalid or unknown runbook metadata fields")
	}
	return nil
}

func checkedMetadata(m metadata, body string) (metadata, string, error) {
	m.Title, m.Description = strings.TrimSpace(m.Title), strings.TrimSpace(m.Description)
	for _, field := range []struct {
		value string
		limit int
	}{{m.Title, 256}, {m.Description, 512}} {
		if !utf8.ValidString(field.value) || utf8.RuneCountInString(field.value) > field.limit || strings.ContainsFunc(field.value, func(c rune) bool { return unicode.IsControl(c) && c != '\n' && c != '\t' }) {
			return m, "", errors.New("runbook title or description exceeds text limits")
		}
	}
	if err := ValidateRequirements(m.Requires); err != nil {
		return m, "", err
	}
	if m.Requires != nil {
		b, _ := json.Marshal(m.Requires)
		decoder := json.NewDecoder(bytes.NewReader(b))
		decoder.UseNumber()
		if err := decoder.Decode(&m.Requires); err != nil {
			return m, "", err
		}
	}
	return m, body, nil
}
