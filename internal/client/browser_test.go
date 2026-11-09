package client

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/koltyakov/control/internal/transport/transporttest"
)

func TestBrowserViewValidation(t *testing.T) {
	for _, spec := range []BrowserViewSpec{
		{}, {Node: "worker", Port: -1}, {Node: "worker", Port: 65536},
		{Node: "worker", Listen: "0.0.0.0:1234"}, {Node: "worker", Listen: "[::]:1234"},
		{Node: "worker", Listen: ":1234"}, {Node: "worker", Listen: "localhost:1234"},
		{Node: "worker", Listen: "127.0.0.1:-1"}, {Node: "worker", Listen: "127.0.0.1:65536"},
	} {
		if _, _, err := (Client{}).StartBrowserView(t.Context(), spec); err == nil {
			t.Fatal("invalid/public browser view accepted", spec)
		}
	}
}

// A local fixture stands in for the bundled dashboard, not a real browser.
// Real peer transport carries HTTP assets and duplex WebSocket frames/input.
func TestBrowserViewPeerStreaming(t *testing.T) {
	for _, relay := range []bool{false, true} {
		t.Run(fmt.Sprintf("relay=%v", relay), func(t *testing.T) {
			ctx, c, workers, _ := delegationClusterWithTransport(t, relay, 30*time.Second, transporttest.Loopback())
			app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/stream" {
					_, _ = io.WriteString(w, "worker-browser-dashboard")
					return
				}
				ws, err := websocket.Accept(w, r, nil)
				if err != nil {
					return
				}
				defer func() { _ = ws.CloseNow() }()
				if err = ws.Write(r.Context(), websocket.MessageText, []byte(`{"type":"frame","seq":1,"data":"fake-jpeg"}`)); err != nil {
					return
				}
				kind, input, err := ws.Read(r.Context())
				if err == nil {
					_ = ws.Write(r.Context(), kind, input)
				}
			}))
			defer app.Close()
			_, port, _ := net.SplitHostPort(strings.TrimPrefix(app.URL, "http://"))
			p, _ := strconv.Atoi(port)
			toolCtx, finishTool := context.WithCancel(ctx)
			forward, info, err := c.StartBrowserView(toolCtx, BrowserViewSpec{Node: "second", Port: p})
			finishTool()
			if err != nil {
				t.Fatal(err)
			}
			defer forward.Close()
			if info.Node != workers[1].Identity.ID || info.URL != "http://"+info.Listen {
				t.Fatal("browser view did not pin its worker or return a local URL", info)
			}
			httpClient := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{Proxy: nil}}
			defer httpClient.CloseIdleConnections()
			response, err := httpClient.Get(info.URL)
			if err != nil {
				t.Fatal(err)
			}
			body, err := io.ReadAll(response.Body)
			_ = response.Body.Close()
			if err != nil || string(body) != "worker-browser-dashboard" {
				t.Fatal("dashboard HTTP assets did not reach orchestrator", string(body), err)
			}
			ws, _, err := websocket.Dial(ctx, strings.Replace(info.URL, "http://", "ws://", 1)+"/stream", nil)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = ws.CloseNow() }()
			_, frame, err := ws.Read(ctx)
			if err != nil || !strings.Contains(string(frame), `"type":"frame"`) {
				t.Fatal("viewport frame missing", string(frame), err)
			}
			input := `{"type":"input_keyboard","eventType":"keyDown","key":"Enter"}`
			if err := ws.Write(ctx, websocket.MessageText, []byte(input)); err != nil {
				t.Fatal(err)
			}
			_, echo, err := ws.Read(ctx)
			if err != nil || string(echo) != input {
				t.Fatal("remote browser input lost", string(echo), err)
			}
			mode := "webrtc"
			if relay {
				mode = "relay"
			}
			if got := c.routing.peer.Connections()[workers[1].Identity.ID]; got != mode {
				t.Fatalf("expected %s browser stream, got %s", mode, got)
			}
			if err := c.StopForward(info.ID); err != nil || len(c.Forwards()) != 0 {
				t.Fatal("browser view forward cleanup failed", err)
			}
			if conn, err := net.DialTimeout("tcp", info.Listen, time.Second); err == nil {
				_ = conn.Close()
				t.Fatal("browser view listener survived stop")
			}
		})
	}
}
