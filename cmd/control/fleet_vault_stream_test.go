package main

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/koltyakov/control/internal/client"
)

func TestFleetLockKeepsAdmittedStreamAndChecksNextStream(t *testing.T) {
	for _, relay := range []bool{false, true} {
		t.Run(fmt.Sprintf("relay=%t", relay), func(t *testing.T) {
			t.Parallel()
			f := newFleetLockFixture(t, relay)
			c := f.client(f.account, t.TempDir())
			var requests atomic.Int64
			app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				_, _ = io.WriteString(w, r.URL.Path)
			}))
			t.Cleanup(app.Close)
			forward, err := c.StartForward(f.ctx, client.ForwardSpec{Node: f.machine(0), Address: app.Listener.Addr().String()})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(forward.Close)
			addr := forward.Info().Listen
			stream, err := net.DialTimeout("tcp", addr, 5*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = stream.Close() })
			reader := bufio.NewReader(stream)
			exchange := func(path string) {
				t.Helper()
				if err := stream.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
					t.Fatal(err)
				}
				if _, err := fmt.Fprintf(stream, "GET %s HTTP/1.1\r\nHost: fixture\r\n\r\n", path); err != nil {
					t.Fatal(err)
				}
				r, err := http.ReadResponse(reader, nil)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = r.Body.Close() }()
				if body, err := io.ReadAll(r.Body); err != nil || string(body) != path {
					t.Fatalf("stream response %q: %s (%v)", path, body, err)
				}
			}
			exchange("/before")
			f.setLock(true, 0)
			exchange("/after")

			// A fresh socket on the very same listener requires a new worker
			// admission. The downstream server must never see this request.
			h := &http.Client{Timeout: 8 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
			url := "http://" + addr + "/new"
			if r, err := h.Get(url); err == nil {
				_ = r.Body.Close()
				t.Fatal("locked worker admitted a new TCP stream")
			}
			if requests.Load() != 2 {
				t.Fatalf("locked downstream received an extra request (count %d)", requests.Load())
			}
			f.setLock(false, 0)
			r, err := h.Get(url)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = r.Body.Close() }()
			if body, err := io.ReadAll(r.Body); err != nil || strings.TrimSpace(string(body)) != "/new" {
				t.Fatalf("new stream to the original listener after unlock: %s (%v)", body, err)
			}
		})
	}
}
