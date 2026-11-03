package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConsoleOverlay(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	dir := filepath.Join(root, "internal", "gateway", "consoleweb")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	source := []byte("// leading comment\n" +
		"/** @param {string} value */\n" +
		"function format(value) {\n" +
		"  const url = 'https://example.com/path'; // trailing comment\n" +
		"  const pattern = /https?:\\/\\//;\n" +
		"  return /* inline comment */ `${value} // literal /* literal */ ${/* expression comment */ url}`;\n" +
		"}\n" +
		"const state = {};\n" +
		"/*! legal comment */\n" +
		"const answer = /* @__PURE__ */ format('hello');\n")
	file := filepath.Join(dir, "console.js")
	if err := os.WriteFile(file, source, 0600); err != nil {
		t.Fatal(err)
	}
	overlay, err := consoleOverlay(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(overlay)
	if err != nil {
		t.Fatal(err)
	}
	var config struct{ Replace map[string]string }
	if err := json.Unmarshal(data, &config); err != nil {
		t.Fatal(err)
	}
	if len(config.Replace) != 1 || config.Replace[file] == "" {
		t.Fatalf("unexpected overlay: %s", data)
	}
	output, err := os.ReadFile(config.Replace[file])
	if err != nil {
		t.Fatal(err)
	}
	for _, comment := range []string{"leading comment", "@param", "trailing comment", "inline comment", "expression comment", "legal comment", "@__PURE__"} {
		if bytes.Contains(output, []byte(comment)) {
			t.Errorf("comment retained: %s", comment)
		}
	}
	for _, literal := range []string{"https://example.com/path", `const pattern=/https?:\/\//`, "// literal /* literal */", "function format(value)", "const state={}"} {
		if !bytes.Contains(output, []byte(literal)) {
			t.Errorf("literal or top-level declaration changed: %s\n%s", literal, output)
		}
	}
	current, err := os.ReadFile(file)
	if err != nil || !bytes.Equal(current, source) {
		t.Fatalf("editable source changed: %v", err)
	}
}

func TestConsoleOverlayRejectsInvalidJavaScript(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	dir := filepath.Join(root, "internal", "gateway", "consoleweb")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "broken.js"), []byte("function {"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := consoleOverlay(t.TempDir()); err == nil || !strings.Contains(err.Error(), "strip console comments") {
		t.Fatalf("invalid JavaScript accepted: %v", err)
	}
}

func TestConsoleOverlayRequiresSources(t *testing.T) {
	t.Chdir(t.TempDir())
	if _, err := consoleOverlay(t.TempDir()); err == nil {
		t.Fatal("missing console sources accepted")
	}
}
