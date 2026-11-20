package client

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/koltyakov/control/internal/model"
)

func TestToolTextRestoresHTMLEscapesWithoutChangingValues(t *testing.T) {
	t.Parallel()
	values := []any{
		map[string]any{"args": []string{"-c", "make && ./run 2>&1 | grep '<ok>'"}},
		map[string]any{"stdout": `<html lang="en"><body>a &amp; b</body></html>`},
		// An escaped backslash followed by literal text must stay intact.
		map[string]any{`k<`: `< \\u0026 "quoted" \`},
		[]any{" ", "tab\tnewline\n", 1.5, nil, true},
	}
	for _, value := range values {
		encoded := model.JSON(value)
		text := toolText(encoded)
		var want, got any
		if err := json.Unmarshal(encoded, &want); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal([]byte(text), &got); err != nil {
			t.Fatalf("%s: %v", text, err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("value changed:\n got %#v\nwant %#v", got, want)
		}
	}
	text := toolText(model.JSON(map[string]string{"cmd": "a && b > c < d"}))
	if text != `{"cmd":"a && b > c < d"}` {
		t.Fatalf("escapes remain: %s", text)
	}
}

func FuzzToolText(f *testing.F) {
	for _, seed := range []string{`"a<b"`, `{"x":"\\u003c"}`, `["&", "\\\\", ">"]`} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		var want any
		if json.Unmarshal([]byte(raw), &want) != nil {
			return
		}
		var got any
		if err := json.Unmarshal([]byte(toolText([]byte(raw))), &got); err != nil {
			t.Fatalf("%q: %v", raw, err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("%q changed: %#v != %#v", raw, got, want)
		}
	})
}
