package node

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/koltyakov/control/internal/clipboard"
	"github.com/koltyakov/control/internal/model"
)

func clipboardFinish(t *testing.T, conn net.Conn) {
	t.Helper()
	var response model.Response
	if err := readFrame(conn, &response); err != nil || response.Error != "" {
		t.Fatal("clipboard completion", response.Error, err)
	}
}

func TestClipboardPeers(t *testing.T) {
	for _, relay := range []bool{false, true} {
		t.Run(fmt.Sprintf("relay=%v", relay), func(t *testing.T) {
			source, worker, consumer := cluster(t, relay, func(i int, cfg *Config) {
				if i == 1 {
					cfg.Allow = map[string][]string{"source": {"clipboard.*", "node.describe"}}
				}
			})
			ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
			defer cancel()
			path := filepath.Join(t.TempDir(), "copied file.bin")
			data := bytes.Repeat([]byte("clipboard\x00\xff"), 100000)
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			worker.readClipboard = func(context.Context) (clipboard.Value, error) { return clipboard.Value{Paths: []string{path}}, nil }
			copied := make(chan string, 1)
			worker.copyClipboard = func(_ context.Context, text string) error { copied <- text; return nil }
			// Remote file references can select a desktop file outside workDir, but cannot be supplied by the caller.
			conn, metadata, err := testOpen(t, source, ctx, "worker", clipboard.OpenMethod, clipboard.Request{Protocol: clipboard.Protocol})
			if err != nil {
				t.Fatal(err)
			}
			defer func(conn net.Conn) { _ = conn.Close() }(conn)
			var ack clipboard.Ack
			if err := json.Unmarshal(metadata, &ack); err != nil || ack.Content.Files[0].Name != filepath.Base(path) {
				t.Fatal(ack, err)
			}
			if bytes.Contains(metadata, []byte(filepath.Dir(path))) {
				t.Fatal("source path leaked into metadata")
			}
			if worker.pauseForUpdate() {
				t.Fatal("pending paste did not hold admission")
			}
			_ = conn.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
			var one [1]byte
			if _, err := conn.Read(one[:]); err == nil {
				t.Fatal("file streamed before paste acknowledgement")
			}
			_ = conn.SetReadDeadline(time.Time{})
			if err := writeFrame(conn, clipboard.Ready{Ready: true}); err != nil {
				t.Fatal(err)
			}
			dest := t.TempDir()
			root, err := os.OpenRoot(dest)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = root.Close() }()
			if _, err := clipboard.Receive(ctx, ack.Content, root, conn); err != nil {
				t.Fatal(err)
			}
			clipboardFinish(t, conn)
			_ = conn.Close()
			got, err := os.ReadFile(filepath.Join(dest, filepath.Base(path)))
			if err != nil || !bytes.Equal(got, data) {
				t.Fatal("remote-to-local file paste", err)
			}
			// Forward file paste reads the sender's selected file descriptor only after acceptance.
			batch, err := clipboard.Snapshot(clipboard.Value{Paths: []string{path}})
			if err != nil {
				t.Fatal(err)
			}
			defer batch.Close()
			conn, _, err = testOpen(t, source, ctx, "worker", clipboard.PasteMethod, clipboard.Request{Protocol: clipboard.Protocol, Content: batch.Content, Dir: "."})
			if err != nil {
				t.Fatal(err)
			}
			if err := batch.Send(ctx, conn); err != nil {
				t.Fatal(err)
			}
			clipboardFinish(t, conn)
			_ = conn.Close()
			got, err = os.ReadFile(filepath.Join(worker.Config.WorkDir, filepath.Base(path)))
			if err != nil || !bytes.Equal(got, data) {
				t.Fatal("local-to-remote file paste", err)
			}
			for _, request := range []clipboard.Request{
				{Protocol: "unknown"},
				{Protocol: clipboard.Protocol, Content: batch.Content}, // existing destination
				{Protocol: clipboard.Protocol, Content: batch.Content, Dir: "../"},
				{Protocol: clipboard.Protocol, Content: clipboard.Content{Kind: "files", Files: []clipboard.File{{Name: "../escape"}}}},
			} {
				if conn, _, err := testOpen(t, source, ctx, "worker", clipboard.PasteMethod, request); err == nil {
					_ = conn.Close()
					t.Fatal("invalid paste accepted", request)
				}
			}
			for _, method := range []string{clipboard.OpenMethod, clipboard.PasteMethod} {
				if conn, _, err := testOpen(t, consumer, ctx, "worker", method, clipboard.Request{Protocol: clipboard.Protocol}); err == nil {
					_ = conn.Close()
					t.Fatal("unauthorized clipboard access", method)
				}
			}
			text := "clipboard Unicode: 世界\nsecond line"
			conn, _, err = testOpen(t, source, ctx, "worker", clipboard.PasteMethod, clipboard.Request{Protocol: clipboard.Protocol, Content: clipboard.Content{Kind: "text", Text: text}})
			if err != nil {
				t.Fatal(err)
			}
			clipboardFinish(t, conn)
			_ = conn.Close()
			if got := <-copied; got != text {
				t.Fatal("text paste differed", got)
			}
			eventually(t, ctx, func() bool { return !worker.updateBusy() })
		})
	}
}

func TestClipboardTextOpen(t *testing.T) {
	source, worker, _ := cluster(t, true, nil)
	text := "copied Unicode 世界\n"
	worker.readClipboard = func(context.Context) (clipboard.Value, error) { return clipboard.Value{Text: text}, nil }
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, data, err := testOpen(t, source, ctx, "worker", clipboard.OpenMethod, clipboard.Request{Protocol: clipboard.Protocol})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	var ack clipboard.Ack
	if err := json.Unmarshal(data, &ack); err != nil || ack.Content.Kind != "text" || ack.Content.Text != text {
		t.Fatal(ack, err)
	}
	if err := writeFrame(conn, clipboard.Ready{Ready: true}); err != nil {
		t.Fatal(err)
	}
	clipboardFinish(t, conn)
}

func TestClipboardStreamLimit(t *testing.T) {
	source, worker, _ := cluster(t, true, nil)
	worker.readClipboard = func(context.Context) (clipboard.Value, error) { return clipboard.Value{Text: "pending paste"}, nil }
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var streams []net.Conn
	defer func() {
		for _, conn := range streams {
			_ = conn.Close()
		}
	}()
	for range 8 {
		conn, _, err := testOpen(t, source, ctx, "worker", clipboard.OpenMethod, clipboard.Request{Protocol: clipboard.Protocol})
		if err != nil {
			t.Fatal(err)
		}
		streams = append(streams, conn)
	}
	if conn, _, err := testOpen(t, source, ctx, "worker", clipboard.OpenMethod, clipboard.Request{Protocol: clipboard.Protocol}); err == nil {
		_ = conn.Close()
		t.Fatal("clipboard stream limit bypassed")
	}
	for _, conn := range streams {
		_ = conn.Close()
	}
	eventually(t, ctx, func() bool { return !worker.updateBusy() })
}

func TestClipboardAPIAndCancellation(t *testing.T) {
	source, worker, _ := cluster(t, true, nil)
	path := filepath.Join(t.TempDir(), "source.txt")
	if err := os.WriteFile(path, []byte("clipboard file"), 0600); err != nil {
		t.Fatal(err)
	}
	worker.readClipboard = func(context.Context) (clipboard.Value, error) { return clipboard.Value{Paths: []string{path}}, nil }
	api := httptest.NewServer(worker.Handler())
	defer api.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	u := strings.Replace(api.URL, "http:", "ws:", 1) + "/v1/clipboard?" + url.Values{"target": {"worker"}, "method": {clipboard.OpenMethod}}.Encode()
	if ws, _, err := websocket.Dial(ctx, u, &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer wrong"}}}); err == nil {
		_ = ws.CloseNow()
		t.Fatal("clipboard API accepted wrong token")
	}
	ws, _, err := websocket.Dial(ctx, u, &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer " + testToken}}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ws.CloseNow() }()
	conn := websocket.NetConn(ctx, ws, websocket.MessageBinary)
	if err := writeFrame(conn, clipboard.Request{Protocol: clipboard.Protocol}); err != nil {
		t.Fatal(err)
	}
	var response model.Response
	if err := readFrame(conn, &response); err != nil || response.Error != "" {
		t.Fatal(response.Error, err)
	}
	if worker.pauseForUpdate() {
		t.Fatal("local clipboard API did not hold admission")
	}
	// Closing before Ready must release the relaying node and source node.
	_ = conn.Close()
	eventually(t, ctx, func() bool { return !source.updateBusy() && !worker.updateBusy() })
}
