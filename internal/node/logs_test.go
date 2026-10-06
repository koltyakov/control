package node

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/koltyakov/control/internal/model"
)

type blockedLogResponse struct {
	header             http.Header
	entered            chan struct{}
	deadline           chan struct{}
	once, deadlineOnce sync.Once
}

func (w *blockedLogResponse) Header() http.Header { return w.header }
func (*blockedLogResponse) WriteHeader(int)       {}
func (*blockedLogResponse) Flush()                {}
func (w *blockedLogResponse) Write([]byte) (int, error) {
	w.once.Do(func() { close(w.entered) })
	<-w.deadline
	return 0, os.ErrDeadlineExceeded
}
func (w *blockedLogResponse) SetWriteDeadline(time.Time) error {
	w.deadlineOnce.Do(func() { close(w.deadline) })
	return nil
}

func TestNodeShutdownInterruptsBlockedHTTPLogFollower(t *testing.T) {
	n, err := New(Config{Name: "worker", Gateway: "http://127.0.0.1:1", Token: "log-shutdown-test-token", DataDir: t.TempDir(), WorkDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	n.ctx, n.cancel = context.WithCancel(context.Background())
	defer func() { _ = n.Close() }()
	n.tasks["finished"] = &model.Task{ID: "finished", Owner: n.Identity.ID, State: "succeeded"}
	w := &blockedLogResponse{header: http.Header{}, entered: make(chan struct{}), deadline: make(chan struct{})}
	r := httptest.NewRequest(http.MethodGet, "/v1/logs?id=finished&offset=0", nil)
	r.Header.Set("Authorization", "Bearer "+n.Config.Token)
	served := make(chan struct{})
	go func() { defer close(served); n.Handler().ServeHTTP(w, r) }()
	select {
	case <-w.entered:
	case <-time.After(time.Second):
		t.Fatal("log response did not start")
	}
	closed := make(chan error, 1)
	go func() { closed <- n.Close() }()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("node shutdown waited indefinitely for blocked HTTP follower")
	}
	select {
	case <-served:
	case <-time.After(time.Second):
		t.Fatal("log handler leaked on shutdown")
	}
}
