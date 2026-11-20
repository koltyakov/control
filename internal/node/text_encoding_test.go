package node

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTextEncodingIsOptInAndLossless(t *testing.T) {
	t.Parallel()
	n := localNode(t, nil)
	page := "<html>café &amp; 日本</html>"
	binary := []byte{0xff, 0xfe, 0x00, 'x'}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/binary" {
			_, _ = w.Write(binary)
			return
		}
		_, _ = w.Write([]byte(page))
	}))
	defer server.Close()
	type response struct {
		Body, Data, Encoding string
		Offset               int64
	}
	call := func(method string, params map[string]any) response {
		t.Helper()
		var r response
		if err := n.Call(t.Context(), "", method, params, &r); err != nil {
			t.Fatal(method, err)
		}
		return r
	}
	if r := call("http.request", map[string]any{"url": server.URL}); r.Encoding != "base64" || r.Body != base64.StdEncoding.EncodeToString([]byte(page)) {
		t.Fatalf("default HTTP body changed: %+v", r)
	}
	if r := call("http.request", map[string]any{"url": server.URL, "encoding": "text"}); r.Encoding != "text" || r.Body != page {
		t.Fatalf("text HTTP body: %+v", r)
	}
	if r := call("http.request", map[string]any{"url": server.URL + "/binary", "encoding": "text"}); r.Encoding != "base64" || r.Body != base64.StdEncoding.EncodeToString(binary) {
		t.Fatalf("binary HTTP body must fall back to base64: %+v", r)
	}
	if err := n.Call(t.Context(), "", "http.request", map[string]any{"url": server.URL, "encoding": "hex"}, nil); err == nil {
		t.Fatal("unknown encoding accepted")
	}

	if err := os.WriteFile(filepath.Join(n.Config.WorkDir, "page.txt"), []byte(page), 0o600); err != nil {
		t.Fatal(err)
	}
	if r := call("files.read", map[string]any{"path": "page.txt"}); r.Encoding != "base64" || r.Data != base64.StdEncoding.EncodeToString([]byte(page)) {
		t.Fatalf("default file data changed: %+v", r)
	}
	// Chunked text reads never split a multi-byte character and reassemble exactly.
	var text strings.Builder
	offset := int64(0)
	for offset < int64(len(page)) {
		r := call("files.read", map[string]any{"path": "page.txt", "offset": offset, "limit": 5, "encoding": "text"})
		if r.Encoding != "text" || r.Offset <= offset {
			t.Fatalf("chunk at %d: %+v", offset, r)
		}
		text.WriteString(r.Data)
		offset = r.Offset
	}
	if text.String() != page {
		t.Fatalf("reassembled %q, want %q", text.String(), page)
	}
}
