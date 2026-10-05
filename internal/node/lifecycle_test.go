package node

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/koltyakov/control/internal/buildinfo"
	"github.com/koltyakov/control/internal/gateway"
	"github.com/koltyakov/control/internal/model"
)

func TestMachineLifecycleBlocksSchedulingAndStopsAgent(t *testing.T) {
	for _, relay := range []bool{false, true} {
		t.Run(fmt.Sprint("relay=", relay), func(t *testing.T) {
			const adminKey = "machine-lifecycle-superuser-123456789"
			g, err := gateway.New(t.TempDir(), testToken, gateway.Options{SuperuserKey: adminKey})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = g.Close() }()
			server := httptest.NewServer(g.Handler())
			defer server.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
			defer cancel()
			request := func(method, path string, body, out any) int {
				t.Helper()
				req, err := http.NewRequestWithContext(ctx, method, server.URL+path, bytes.NewReader(model.JSON(body)))
				if err != nil {
					t.Fatal(err)
				}
				req.Header.Set("Authorization", "Bearer "+adminKey)
				resp, err := http.DefaultClient.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = resp.Body.Close() }()
				if out != nil && resp.StatusCode < 300 {
					if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
						t.Fatal(err)
					}
				}
				return resp.StatusCode
			}
			newNode := func(name string) *Node {
				n, err := New(Config{Name: name, Gateway: server.URL, Token: testToken, DataDir: t.TempDir(), WorkDir: t.TempDir(), RelayOnly: relay, MetricsIntervalSeconds: -1, Labels: map[string]string{"role": name}})
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = n.Close() })
				n.Peer.SetSoftware(buildinfo.Info{Version: "v-node-test", OS: runtime.GOOS, Arch: runtime.GOARCH})
				return n
			}
			source, worker := newNode("source"), newNode("worker")
			started, release := make(chan struct{}, 1), make(chan struct{})
			if err := worker.Register(provider{cap: model.Capability{Name: "probe", InputSchema: model.JSON(map[string]any{"type": "object"})}, run: func(ctx context.Context, _ json.RawMessage, _ Execution) (any, error) {
				select {
				case started <- struct{}{}:
				default:
				}
				select {
				case <-release:
					return "done", nil
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			}}); err != nil {
				t.Fatal(err)
			}
			stopped := make(chan struct{})
			worker.SetShutdown(func() { close(stopped) })
			for _, n := range []*Node{source, worker} {
				if err := n.Start(ctx); err != nil {
					t.Fatal(err)
				}
			}
			var directory []model.Node
			eventually(t, ctx, func() bool {
				request("GET", "/v1/nodes", nil, &directory)
				for _, n := range directory {
					if n.ID == worker.Identity.ID {
						return n.Managed
					}
				}
				return false
			})
			spec := model.TaskSpec{ID: "durable-probe", Capability: "probe", Args: model.JSON(map[string]any{})}
			var task model.Task
			if err := source.Call(ctx, "worker", "tasks.start", spec, &task); err != nil {
				t.Fatal(err)
			}
			select {
			case <-started:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			path := "/v1/fleet/nodes/" + worker.Identity.ID
			if code := request("PATCH", path, map[string]bool{"disabled": true}, nil); code != 200 {
				t.Fatal("disable", code)
			}
			eventually(t, ctx, func() bool { return worker.currentMachineState().Disabled })
			if err := source.Call(ctx, "", "nodes.select", map[string]any{"labels": map[string]string{"role": "worker"}}, nil); err == nil {
				t.Fatal("disabled worker selected")
			}
			if err := source.Call(ctx, "worker", "probe", map[string]any{}, nil); err == nil || !strings.Contains(err.Error(), "disabled") {
				t.Fatal("cached peer session bypassed disable", err)
			}
			if err := source.Call(ctx, "worker", "tasks.start", spec, &task); err != nil || task.Terminal() {
				t.Fatal("disabled machine lost accepted/idempotent task", err)
			}
			spec.ID = "new-probe"
			if err := source.Call(ctx, "worker", "tasks.start", spec, nil); err == nil {
				t.Fatal("disabled worker accepted another task")
			}
			close(release)
			if completed, err := source.WaitTask(ctx, "worker", "durable-probe"); err != nil || completed.State != "succeeded" {
				t.Fatal("disable interrupted accepted work", err)
			}
			if code := request("PATCH", path, map[string]bool{"disabled": false}, nil); code != 200 {
				t.Fatal("enable", code)
			}
			eventually(t, ctx, func() bool {
				var chosen model.Node
				return source.Call(ctx, "", "nodes.select", map[string]any{"labels": map[string]string{"role": "worker"}}, &chosen) == nil && chosen.ID == worker.Identity.ID
			})
			if err := source.Call(ctx, "worker", "probe", map[string]any{}, nil); err != nil {
				t.Fatal("enabled worker rejected work", err)
			}
			if code := request("DELETE", path+"?stop=true", nil, nil); code != 204 {
				t.Fatal("unregister", code)
			}
			select {
			case <-stopped:
			case <-ctx.Done():
				t.Fatal("unregister did not stop agent")
			}
			if err := worker.Close(); err != nil {
				t.Fatal(err)
			}
			request("GET", "/v1/nodes", nil, &directory)
			for _, n := range directory {
				if n.ID == worker.Identity.ID {
					t.Fatal("disconnect resurrected removed registration")
				}
			}
			restarted, err := New(worker.Config)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = restarted.Close() }()
			if err := restarted.Start(ctx); !errors.Is(err, ErrUnregistered) {
				t.Fatal("unregistered agent restarted", err)
			}
		})
	}
}
