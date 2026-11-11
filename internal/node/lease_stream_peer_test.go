package node

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/koltyakov/control/internal/clipboard"
	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/transport/transporttest"
	"github.com/koltyakov/control/internal/upload"
)

func TestLeaseSensitiveStreamsPeersAndD010Exemptions(t *testing.T) {
	for _, relay := range []bool{false, true} {
		t.Run(fmt.Sprintf("relay=%v", relay), func(t *testing.T) {
			source, worker, consumer := cluster(t, relay, nil, transporttest.Loopback())
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			var clipboardCalls atomic.Int32
			worker.readClipboard = func(context.Context) (clipboard.Value, error) {
				clipboardCalls.Add(1)
				return clipboard.Value{Text: "fixture"}, nil
			}
			worker.copyClipboard = func(context.Context, string) error { clipboardCalls.Add(1); return nil }
			// A clipboard source admitted first must exclude lease acquisition.
			pending, _, err := testOpen(t, source, ctx, "worker", clipboard.OpenMethod, clipboard.Request{Protocol: clipboard.Protocol})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = pending.Close() }()
			if err := testCall(t, source, ctx, "worker", "leases.acquire", map[string]any{}, nil); err == nil {
				t.Fatal("lease overlapped admitted clipboard access")
			}
			if err := writeFrame(pending, clipboard.Ready{Ready: true}); err != nil {
				t.Fatal(err)
			}
			clipboardFinish(t, pending)
			_ = pending.Close()
			eventually(t, ctx, func() bool { worker.mu.Lock(); defer worker.mu.Unlock(); return worker.activeInvocations == 0 })

			web := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "tcp remains available") }))
			defer web.Close()
			peer := testOrchestrator(t, source).peer
			before, err := peer.OpenTCP(ctx, "worker", strings.TrimPrefix(web.URL, "http://"))
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = before.Close() }()
			// TCP holds maintenance admission, not execution-lease admission.
			var lease model.Lease
			if err := testCall(t, source, ctx, "worker", "leases.acquire", map[string]any{}, &lease); err != nil {
				t.Fatal("existing TCP access blocked execution lease", err)
			}
			for _, caller := range []*Node{source, consumer} {
				for _, q := range []struct {
					method string
					params any
				}{
					{upload.OpenMethod, emptyLeaseUpload()},
					{clipboard.OpenMethod, clipboard.Request{Protocol: clipboard.Protocol}},
					{clipboard.PasteMethod, clipboard.Request{Protocol: clipboard.Protocol, Content: clipboard.Content{Kind: "text", Text: "must not paste"}}},
					{clipboard.PasteMethod, clipboard.Request{Protocol: clipboard.Protocol, Content: clipboard.Content{Kind: "files", Files: []clipboard.File{{Name: "must-not-publish"}}}}},
				} {
					conn, _, err := testOpen(t, caller, ctx, "worker", q.method, q.params)
					if conn != nil {
						_ = conn.Close()
					}
					var rpc *model.RPCError
					if !errors.As(err, &rpc) || rpc.Code != "node_leased" {
						t.Fatal("lease owner or other caller bypassed stream restriction", caller.Config.Name, q.method, err)
					}
				}
			}
			if clipboardCalls.Load() != 1 {
				t.Fatal("leased stream accessed the clipboard", clipboardCalls.Load())
			}
			// Discovery-only worker credentials must fail authority checks before
			// reaching lease admission, rather than learning/bypassing its state.
			for _, method := range []string{upload.OpenMethod, clipboard.OpenMethod} {
				conn, _, err := source.Peer.OpenRPC(ctx, "worker", method, emptyLeaseUpload())
				if conn != nil {
					_ = conn.Close()
				}
				if err == nil || !strings.Contains(err.Error(), "account-authenticated orchestrator") {
					t.Fatal("lease admission replaced execution-authority checks", method, err)
				}
			}
			if records, err := filepath.Glob(filepath.Join(worker.Config.DataDir, "uploads", "*.json")); err != nil || len(records) != 0 {
				t.Fatal("rejected upload reserved durable capacity", records, err)
			}
			for _, name := range []string{"must-not-publish", "uploaded.bin"} {
				if _, err := os.Stat(filepath.Join(worker.Config.WorkDir, name)); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("leased stream published a file", name, err)
				}
			}
			// D010 permits both existing and newly opened TCP streams.
			after, err := peer.OpenTCP(ctx, "worker", strings.TrimPrefix(web.URL, "http://"))
			if err != nil {
				t.Fatal("lease blocked a new TCP stream", err)
			}
			defer func() { _ = after.Close() }()
			for _, conn := range []net.Conn{before, after} {
				request, err := http.NewRequestWithContext(ctx, http.MethodGet, web.URL, nil)
				if err != nil {
					t.Fatal(err)
				}
				if err := request.Write(conn); err != nil {
					t.Fatal(err)
				}
				response, err := http.ReadResponse(bufio.NewReader(conn), request)
				if err != nil {
					t.Fatal(err)
				}
				data, err := io.ReadAll(response.Body)
				_ = response.Body.Close()
				if err != nil || string(data) != "tcp remains available" {
					t.Fatal("lease interrupted TCP access", string(data), err)
				}
			}
			listener, _, err := peer.OpenTCPListener(ctx, "worker", "127.0.0.1:0")
			if err != nil {
				t.Fatal("lease blocked reverse TCP listener", err)
			}
			_ = listener.Close()
			if err := os.WriteFile(filepath.Join(worker.Config.WorkDir, "artifact.txt"), []byte("artifact remains available"), 0600); err != nil {
				t.Fatal(err)
			}
			var artifact model.Artifact
			if err := testCall(t, source, ctx, "worker", "artifacts.export", map[string]string{"path": "artifact.txt"}, &artifact); err != nil {
				t.Fatal("lease blocked artifact export", err)
			}
			stream, _, err := testOpen(t, source, ctx, "worker", "artifacts.open", map[string]any{"id": artifact.ID})
			if err != nil {
				t.Fatal("lease blocked artifact stream", err)
			}
			data, err := io.ReadAll(stream)
			_ = stream.Close()
			if err != nil || string(data) != "artifact remains available" {
				t.Fatal("leased artifact content", string(data), err)
			}
			assertNoLeaseStreams(t, worker)
			if err := testCall(t, source, ctx, "worker", "leases.release", map[string]string{"id": lease.ID}, nil); err != nil {
				t.Fatal(err)
			}
			mode := "webrtc"
			if relay {
				mode = "relay"
			}
			if peer.Connections()[worker.Identity.ID] != mode {
				t.Fatal("wrong actual peer transport", peer.Connections())
			}
		})
	}
}

func TestLeaseSensitiveClipboardLocalAPI(t *testing.T) {
	n := localNode(t, nil)
	var calls atomic.Int32
	n.readClipboard = func(context.Context) (clipboard.Value, error) {
		calls.Add(1)
		return clipboard.Value{Text: "fixture"}, nil
	}
	n.copyClipboard = func(context.Context, string) error { calls.Add(1); return nil }
	if _, err := n.leaseMethod(n.Identity.ID, "leases.acquire", model.JSON(map[string]any{})); err != nil {
		t.Fatal(err)
	}
	api := httptest.NewServer(n.Handler())
	defer api.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	for _, method := range []string{clipboard.OpenMethod, clipboard.PasteMethod} {
		address := strings.Replace(api.URL, "http:", "ws:", 1) + "/v1/clipboard?" + url.Values{"method": {method}}.Encode()
		ws, _, err := websocket.Dial(ctx, address, &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer " + testToken}}})
		if err != nil {
			t.Fatal(err)
		}
		conn := websocket.NetConn(ctx, ws, websocket.MessageBinary)
		if err := writeFrame(conn, clipboard.Request{Protocol: clipboard.Protocol, Content: clipboard.Content{Kind: "text", Text: "must not paste"}}); err != nil {
			_ = ws.CloseNow()
			t.Fatal(err)
		}
		leaseStreamResponse(t, conn, "node_leased")
		_ = ws.CloseNow()
	}
	if calls.Load() != 0 {
		t.Fatal("local API bypassed execution lease", calls.Load())
	}
	assertNoLeaseStreams(t, n)
}
