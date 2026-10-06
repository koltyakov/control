package client

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/koltyakov/control/internal/gateway"
	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/node"
)

func TestRenameRoutesNewAndReusedAliasesWithoutClosingStreams(t *testing.T) {
	for _, relay := range []bool{false, true} {
		t.Run(fmt.Sprintf("relay=%v", relay), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			t.Cleanup(cancel)
			const key = "rename-test-superuser-key-123456789"
			g, err := gateway.New(t.TempDir(), "", gateway.Options{SuperuserKey: key})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = g.Close() })
			s := httptest.NewServer(g.Handler())
			t.Cleanup(s.Close)
			a := Admin{URL: s.URL, Key: key}
			workers := make([]*node.Node, 2)
			for i, name := range []string{"original", "second"} {
				w, err := node.New(node.Config{Name: name, Gateway: s.URL, Token: key, DataDir: t.TempDir(), WorkDir: t.TempDir(), RelayOnly: relay})
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = w.Close() })
				if err := w.Start(ctx); err != nil {
					t.Fatal(err)
				}
				workers[i] = w
			}
			c := (Client{URL: refusedAPI(t)}).WithStandalone(ctx, StandaloneConfig{Gateway: a, StateDir: t.TempDir(), RelayOnly: relay})
			t.Cleanup(func() { _ = c.Close() })
			var described struct{ ID string }
			if err := c.Call(ctx, "original", "node.describe", map[string]any{}, &described); err != nil || described.ID != workers[0].Identity.ID {
				t.Fatal("initial routing", described, err)
			}
			echo, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = echo.Close() })
			finished := make(chan struct{})
			go func() {
				defer close(finished)
				conn, err := echo.Accept()
				if err == nil {
					defer func() { _ = conn.Close() }()
					_, _ = io.Copy(conn, conn)
				}
			}()
			stream, err := c.Tunnel(ctx, "original", echo.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = stream.Close(); <-finished })
			if err := a.RenameMachine(ctx, workers[0].Identity.ID, "alias"); err != nil {
				t.Fatal(err)
			}
			if err := a.RenameMachine(ctx, workers[1].Identity.ID, "original"); err != nil {
				t.Fatal(err)
			}
			for _, target := range []string{"alias", workers[0].Identity.ID} {
				if err := c.Call(ctx, target, "node.describe", map[string]any{}, &described); err != nil || described.ID != workers[0].Identity.ID {
					t.Fatal("renamed routing", target, described, err)
				}
			}
			// The notification is asynchronous. Wait for the authenticated cache
			// invalidation before asserting that a reused name routes elsewhere.
			deadline := time.Now().Add(3 * time.Second)
			for {
				err := c.Call(ctx, "original", "node.describe", map[string]any{}, &described)
				if err == nil && described.ID == workers[1].Identity.ID {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("stale alias routed to previous identity", described, err)
				}
				time.Sleep(10 * time.Millisecond)
			}
			if err := stream.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
				t.Fatal(err)
			}
			if _, err := stream.Write([]byte("still-open")); err != nil {
				t.Fatal("rename closed active stream", err)
			}
			data := make([]byte, len("still-open"))
			if _, err := io.ReadFull(stream, data); err != nil || string(data) != "still-open" {
				t.Fatal("active stream interrupted", string(data), err)
			}
			var nodes []model.Node
			if err := a.JSON(ctx, "GET", "/v1/nodes", nil, &nodes); err != nil || len(nodes) != 2 {
				t.Fatal("rename changed fleet membership", nodes, err)
			}
		})
	}
}
