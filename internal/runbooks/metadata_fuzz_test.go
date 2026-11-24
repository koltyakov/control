package runbooks

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"
)

// FuzzParseMetadata parses arbitrary Markdown frontmatter and JSON runbooks.
// Accepted metadata must respect the documented text, structure and 16 KiB
// requirement bounds, return the document body unchanged, and survive
// re-encoding as JSON metadata, which is how remote catalogs carry it.
func FuzzParseMetadata(f *testing.F) {
	for _, seed := range []struct {
		json    bool
		content string
	}{
		{false, "---\ntitle: GPU video encoding\ndescription: Encode video.\nrequires:\n  os: [windows]\n  gpu: true\n---\n# Body\n"},
		{false, "\uFEFF---\r\nrequires: Connect the corporate VPN first.\r\n---\r\nbody"},
		{false, "---\nrequires:\n  - a\n  - {b: 1.5, c: null}\n---\n"},
		{false, "---\nrequires: &a [x]\nother: *a\n---\n"},
		{false, "---\n<<: {title: x}\n---\n"},
		{false, "---\nrequires: {1: x}\n---\n"},
		{false, "---\ntitle: a\n---\ntitle: b\n---\n"},
		{false, "---\nunknown: x\n---\n"},
		{false, "---\nrequires: .nan\n---\n"},
		{false, "---\nrequires: [[[[[[[[[[x]]]]]]]]]]\n---\n"},
		{false, "---\ntitle: |\n  a\x01b\n---\n"},
		{false, "---\nno closing line"},
		{false, "# Plain markdown\n"},
		{true, `{"title":"T","description":"D","requires":{"vpn":{"state":"connected"},"n":9007199254740993},"steps":[]}`},
		{true, `{"requires":["a",1e400]}`},
		{true, `{"title":1}`},
		{true, `[1,2,3]`},
		{true, `{"requires":{"":1}}`},
	} {
		f.Add(seed.json, seed.content)
	}
	f.Fuzz(func(t *testing.T, isJSON bool, content string) {
		file := "RUNBOOK.md"
		if isJSON {
			file = "runbook.json"
		}
		m, body, err := parseMetadata(file, content)
		if err != nil {
			return
		}
		if isJSON {
			if body != content {
				t.Fatal("JSON runbook body changed")
			}
		} else if !strings.HasSuffix(content, body) {
			t.Fatalf("body %q is not a suffix of the document", body)
		}
		for _, field := range []struct {
			value string
			limit int
		}{{m.Title, 256}, {m.Description, 512}} {
			if !utf8.ValidString(field.value) || utf8.RuneCountInString(field.value) > field.limit || strings.TrimSpace(field.value) != field.value ||
				strings.ContainsFunc(field.value, func(c rune) bool { return unicode.IsControl(c) && c != '\n' && c != '\t' }) {
				t.Fatalf("accepted out-of-bounds text %q", field.value)
			}
		}
		if err := ValidateRequirements(m.Requires); err != nil {
			t.Fatalf("accepted requirements fail validation: %v", err)
		}
		if b, err := json.Marshal(m.Requires); err != nil || len(b) > maxMetadataBytes {
			t.Fatalf("accepted %d bytes of requirements: %v", len(b), err)
		}
		document, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		again, _, err := parseMetadata("catalog.json", string(document))
		if err != nil {
			t.Fatalf("re-encoded metadata %s rejected: %v", document, err)
		}
		if !reflect.DeepEqual(again, m) {
			t.Fatalf("metadata changed through JSON: %#v != %#v", again, m)
		}
	})
}
