package wsio

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/coder/websocket"
)

func TestReaderReuseAndMessageLimit(t *testing.T) {
	for _, limited := range []bool{false, true} {
		t.Run(map[bool]string{false: "reuse", true: "limit"}[limited], func(t *testing.T) {
			messages := [][]byte{bytes.Repeat([]byte("a"), 64<<10), []byte("small"), bytes.Repeat([]byte("b"), 256<<10), []byte("last")}
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				ws, err := websocket.Accept(w, r, nil)
				if err != nil {
					return
				}
				defer func() { _ = ws.CloseNow() }()
				for _, message := range messages {
					if err := ws.Write(t.Context(), websocket.MessageBinary, message); err != nil {
						return
					}
				}
				_, _, _ = ws.Read(r.Context())
			}))
			defer s.Close()
			ws, _, err := websocket.Dial(t.Context(), s.URL, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = ws.CloseNow() }()
			ws.SetReadLimit(1 << 20)
			if limited {
				ws.SetReadLimit(1024)
			}
			var reader Reader
			for _, want := range messages {
				kind, got, err := reader.Read(t.Context(), ws)
				if limited {
					if err == nil {
						t.Fatal("message size limit bypassed")
					}
					return
				}
				if err != nil || kind != websocket.MessageBinary || !bytes.Equal(got, want) {
					t.Fatalf("message length=%d, want=%d: %v", len(got), len(want), err)
				}
			}
			if reader.buffer.Cap() > maxRetainedBytes {
				t.Fatal("large signaling buffer retained after next message")
			}
		})
	}
}
