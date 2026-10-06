package client

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func verifyReverseForward(t *testing.T, ctx context.Context, c Client) {
	t.Helper()
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/ws" {
			ws, err := websocket.Accept(w, r, nil)
			if err != nil {
				return
			}
			defer func() { _ = ws.CloseNow() }()
			ws.SetReadLimit(1 << 20)
			for {
				kind, data, err := ws.Read(r.Context())
				if err != nil {
					return
				}
				if err := ws.Write(r.Context(), kind, data); err != nil {
					return
				}
			}
		}
		_, _ = io.WriteString(w, "local dev application")
	}))
	defer app.Close()
	toolCtx, finishTool := context.WithTimeout(ctx, 10*time.Second)
	f, err := c.StartForward(toolCtx, ForwardSpec{Node: "worker", Address: strings.TrimPrefix(app.URL, "http://"), Reverse: true})
	finishTool()
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if !f.Info().Reverse || !strings.HasPrefix(f.Info().Listen, "127.0.0.1:") {
		t.Fatal(f.Info())
	}
	// Multiple browser connections and WebSocket upgrades use independent streams.
	httpClient := &http.Client{Timeout: 5 * time.Second}
	for range 3 {
		response, err := httpClient.Get("http://" + f.Info().Listen)
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(response.Body)
		_ = response.Body.Close()
		if err != nil || string(body) != "local dev application" {
			t.Fatal(string(body), err)
		}
	}
	ws, _, err := websocket.Dial(ctx, "ws://"+f.Info().Listen+"/ws", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ws.CloseNow() }()
	payload := bytes.Repeat([]byte("hot-reload"), 8192)
	ws.SetReadLimit(int64(len(payload) + 1))
	if err := ws.Write(ctx, websocket.MessageBinary, payload); err != nil {
		t.Fatal(err)
	}
	_, data, err := ws.Read(ctx)
	if err != nil || !bytes.Equal(data, payload) {
		t.Fatal("WebSocket forwarding", len(data), err)
	}
	if _, err := c.StartForward(ctx, ForwardSpec{Node: "worker", Address: strings.TrimPrefix(app.URL, "http://"), Listen: f.Info().Listen, Reverse: true}); err == nil {
		t.Fatal("occupied remote port accepted")
	}
	if err := c.StopForward(f.Info().ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ws.Read(ctx); err == nil {
		t.Fatal("reverse socket retained after stop")
	}
	// Stop propagates asynchronously to the remote listener, even when idle.
	deadline := time.Now().Add(3 * time.Second)
	for {
		conn, err := net.DialTimeout("tcp", f.Info().Listen, time.Second)
		if err != nil {
			break
		}
		_ = conn.Close()
		if time.Now().After(deadline) {
			t.Fatal("remote listener retained after stop")
		}
		time.Sleep(10 * time.Millisecond)
	}
	verifyReverseEOF(t, ctx, c)
}

func verifyReverseEOF(t *testing.T, ctx context.Context, c Client) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	result := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			result <- err
			return
		}
		defer func() { _ = conn.Close() }()
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
		data, err := io.ReadAll(conn)
		if err == nil {
			_, err = conn.Write(data)
		}
		result <- err
	}()
	f, err := c.StartForward(ctx, ForwardSpec{Node: "worker", Address: listener.Addr().String(), Reverse: true})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.StopForward(f.Info().ID) }()
	conn, err := net.DialTimeout("tcp", f.Info().Listen, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	payload := bytes.Repeat([]byte("EOF"), 64*1024)
	if _, err := conn.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := conn.(*net.TCPConn).CloseWrite(); err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(conn)
	if err != nil || !bytes.Equal(data, payload) {
		t.Fatal("reverse half-close", len(data), err)
	}
	if err := <-result; err != nil {
		t.Fatal(err)
	}
}
