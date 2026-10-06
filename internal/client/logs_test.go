package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/koltyakov/control/internal/gateway"
	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/node"
)

func TestFollowLogsDrainsTerminalChunksAndResumes(t *testing.T) {
	const text = "firstsecond"
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/call" {
			http.NotFound(w, r)
			return
		}
		var q node.APICall
		if err := json.NewDecoder(r.Body).Decode(&q); err != nil {
			t.Error(err)
			return
		}
		var p struct {
			Offset int `json:"offset"`
		}
		if err := json.Unmarshal(q.Params, &p); err != nil {
			t.Error(err)
			return
		}
		end := min(len(text), p.Offset+3)
		_ = json.NewEncoder(w).Encode(model.Response{Result: model.JSON(map[string]any{"text": text[p.Offset:end], "offset": end, "terminal": true})})
	}))
	defer s.Close()
	c := Client{URL: s.URL}
	var out bytes.Buffer
	if err := c.FollowLogs(context.Background(), "worker", "task", 5, &out); err != nil || out.String() != "second" {
		t.Fatal(out.String(), err)
	}
	if err := c.FollowLogs(context.Background(), "worker", "task", -1, io.Discard); err == nil {
		t.Fatal("negative offset accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := c.FollowLogs(ctx, "worker", "task", 0, io.Discard); err == nil {
		t.Fatal("cancelled follower kept reading")
	}
}

type logFeedProvider struct{ chunks <-chan []byte }

func (logFeedProvider) Capability() model.Capability {
	return model.Capability{Name: "test.logfeed", InputSchema: json.RawMessage(`{"type":"object"}`)}
}
func (p logFeedProvider) Run(ctx context.Context, _ json.RawMessage, e node.Execution) (any, error) {
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case data, ok := <-p.chunks:
			if !ok {
				return "done", nil
			}
			if _, err := e.Log.Write(data); err != nil {
				return nil, err
			}
		}
	}
}

type chunkSink struct{ chunks chan []byte }

func (s chunkSink) Write(b []byte) (int, error) {
	if len(b) != 0 {
		s.chunks <- append([]byte(nil), b...)
	}
	return len(b), nil
}

func TestLogStreamingBinaryOffsetsCancellationAndOwnership(t *testing.T) {
	for _, standalone := range []bool{false, true} {
		t.Run(fmt.Sprintf("standalone=%v", standalone), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			const token = "log-stream-test-token-1234567890"
			const account = "log-stream-account-key-1234567890"
			g, err := gateway.New(t.TempDir(), token, gateway.Options{SuperuserKey: account})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = g.Close() }()
			s := httptest.NewServer(g.Handler())
			defer s.Close()
			w, err := node.New(node.Config{Name: "worker", Gateway: s.URL, Token: token, DataDir: t.TempDir(), WorkDir: t.TempDir(), RelayOnly: true})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = w.Close() }()
			feed := make(chan []byte)
			if err := w.Register(logFeedProvider{chunks: feed}); err != nil {
				t.Fatal(err)
			}
			if err := w.Start(ctx); err != nil {
				t.Fatal(err)
			}
			api := httptest.NewServer(w.Handler())
			defer api.Close()
			cfg := StandaloneConfig{Gateway: Admin{URL: s.URL, Key: account}, StateDir: t.TempDir(), RelayOnly: true}
			c := (Client{URL: api.URL, Token: token}).WithLifetime(ctx)
			if standalone {
				c = (Client{URL: refusedAPI(t)}).WithStandalone(ctx, cfg)
			}
			defer func() { _ = c.Close() }()
			var task model.Task
			if err := c.Call(ctx, "worker", "tasks.start", model.TaskSpec{ID: "logfeed", Capability: "test.logfeed"}, &task); err != nil {
				t.Fatal(err)
			}
			followCtx, stop := context.WithCancel(ctx)
			sink := chunkSink{chunks: make(chan []byte, 4)}
			done := make(chan error, 1)
			go func() { done <- c.FollowLogs(followCtx, "worker", task.ID, 0, sink) }()
			first := []byte{0xff, 0xe2, 0x82}
			select {
			case feed <- first:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			select {
			case got := <-sink.chunks:
				if !bytes.Equal(got, first) {
					t.Fatal("binary log bytes changed", got)
				}
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			stop()
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("cancelled follower returned success")
				}
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			if err := c.Call(ctx, "worker", "tasks.get", map[string]any{"id": task.ID}, &task); err != nil || task.Terminal() {
				t.Fatal("log cancellation cancelled task", task, err)
			}
			foreignCfg := cfg
			foreignCfg.StateDir = t.TempDir()
			foreign := (Client{URL: refusedAPI(t)}).WithStandalone(ctx, foreignCfg)
			if err := foreign.FollowLogs(ctx, "worker", task.ID, 0, io.Discard); err == nil {
				t.Fatal("log stream bypassed owner check")
			}
			_ = foreign.Close()
			var remainder bytes.Buffer
			go func() { done <- c.FollowLogs(ctx, "worker", task.ID, int64(len(first)), &remainder) }()
			second := []byte("tail")
			select {
			case feed <- second:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			close(feed)
			select {
			case err := <-done:
				if err != nil || !bytes.Equal(remainder.Bytes(), second) {
					t.Fatal(remainder.String(), err)
				}
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
		})
	}
}
