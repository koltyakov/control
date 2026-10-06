// Package rpa defines the desktop helper contract shared by nodes and MCP clients.
package rpa

import (
	"encoding/json"
	"fmt"

	"github.com/google/jsonschema-go/jsonschema"
)

// Schema describes one serialized desktop action batch. Selectors are exact
// matches; a helper must reject missing or ambiguous targets before acting.
func Schema() map[string]any {
	text := map[string]any{"type": "string", "maxLength": 4096}
	integer := func(min, max int) map[string]any {
		return map[string]any{"type": "integer", "minimum": min, "maximum": max}
	}
	selector := map[string]any{
		"type": "object", "additionalProperties": false, "minProperties": 1,
		"properties": map[string]any{},
	}
	for _, key := range []string{"app", "name", "role", "automationId"} {
		selector["properties"].(map[string]any)[key] = map[string]any{"type": "string", "minLength": 1, "maxLength": 4096}
	}
	position := map[string]any{"x": integer(-32768, 32767), "y": integer(-32768, 32767)}
	action := func(kind string, properties map[string]any, required ...string) map[string]any {
		p := map[string]any{"type": map[string]any{"const": kind}}
		for k, v := range properties {
			p[k] = v
		}
		return map[string]any{"type": "object", "additionalProperties": false, "properties": p, "required": append([]string{"type"}, required...)}
	}
	click := action("click", map[string]any{
		"target": selector, "x": position["x"], "y": position["y"],
		"button": map[string]any{"enum": []string{"left", "right", "middle"}}, "count": integer(1, 2),
	})
	click["oneOf"] = []any{
		map[string]any{"required": []string{"target"}, "properties": map[string]any{"button": map[string]any{"const": "left"}, "count": map[string]any{"const": 1}}, "not": map[string]any{"anyOf": []any{map[string]any{"required": []string{"x"}}, map[string]any{"required": []string{"y"}}}}},
		map[string]any{"required": []string{"x", "y"}, "not": map[string]any{"required": []string{"target"}}},
	}
	return map[string]any{
		"type": "object", "additionalProperties": false, "required": []string{"actions"},
		"properties": map[string]any{"actions": map[string]any{
			"type": "array", "minItems": 1, "maxItems": 100,
			"items": map[string]any{"oneOf": []any{
				action("inspect", map[string]any{"app": text, "limit": integer(1, 200)}),
				action("screenshot", nil),
				click,
				action("focus", map[string]any{"target": selector}, "target"),
				action("setValue", map[string]any{"target": selector, "text": text}, "target", "text"),
				action("move", position, "x", "y"),
				action("drag", map[string]any{"x": position["x"], "y": position["y"], "durationMs": integer(100, 5000)}, "x", "y"),
				action("scroll", map[string]any{"clicks": integer(-100, 100)}, "clicks"),
				action("type", map[string]any{"text": text}, "text"),
				action("keys", map[string]any{"keys": map[string]any{"type": "array", "minItems": 1, "maxItems": 8, "items": map[string]any{"type": "string", "minLength": 1, "maxLength": 32}}}, "keys"),
				action("wait", map[string]any{"milliseconds": integer(1, 5000)}, "milliseconds"),
			}},
		}},
	}
}

var resolved = func() *jsonschema.Resolved {
	b, err := json.Marshal(Schema())
	if err != nil {
		panic(err)
	}
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

// Validate checks the entire batch before any desktop side effects occur.
func Validate(args json.RawMessage) error {
	var value any
	if err := json.Unmarshal(args, &value); err != nil {
		return err
	}
	if err := resolved.Validate(value); err != nil {
		return fmt.Errorf("invalid RPA batch: %w", err)
	}
	return nil
}
