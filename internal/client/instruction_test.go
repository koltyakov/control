package client

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/koltyakov/control/internal/model"
)

func TestHybridInstructionBoundary(t *testing.T) {
	t.Parallel()
	for _, relay := range []bool{true, false} {
		t.Run(fmt.Sprintf("relay=%v", relay), func(t *testing.T) {
			t.Parallel()
			ctx, c, workers, _ := delegationClusterWithTimeout(t, relay, 45*time.Second, true)
			peer, err := c.backend(ctx)
			if err != nil {
				t.Fatal(err)
			}
			destination, err := peer.Lookup(ctx, "second")
			if err != nil || !destination.InstructionOnly {
				t.Fatal("missing instruction-only advertisement", destination, err)
			}
			if conn, _, err := peer.OpenRPC(ctx, "second", "test.owner", map[string]any{}); err == nil {
				_ = conn.Close()
				t.Fatal("raw account execution bypassed System grant")
			}
			if _, err = peer.Select(ctx, model.JSON(map[string]any{"labels": map[string]string{"context": "system"}})); err == nil {
				t.Fatal("automatic worker selection chose System execution")
			}
			var owner string
			if err = c.Call(ctx, "second", "test.owner", map[string]any{}, &owner); err != nil {
				t.Fatal(err)
			}
			info, err := c.Session(ctx)
			if err != nil || owner != info.OwnerID {
				t.Fatal("lost account task owner", owner, info, err)
			}
			var grants []model.Delegation
			if err = c.Call(ctx, "second", "access.list", nil, &grants); err != nil || len(grants) != 0 {
				t.Fatal("synchronous grant leaked", grants, err)
			}
			if err = workers[0].Call(ctx, "second", "test.owner", map[string]any{}, nil); err == nil {
				t.Fatal("User worker gained ambient System authority")
			}
			var direct model.Delegation
			allowed := map[string]any{"instruction": "allowed"}
			if err = c.Call(ctx, "second", "access.grant", model.Delegation{Subject: peer.IdentityID(), Method: "test.owner", Params: model.JSON(allowed)}, &direct); err != nil {
				t.Fatal(err)
			}
			forged := direct
			forged.Params = model.JSON(map[string]any{"instruction": "changed"})
			if conn, _, err := peer.OpenRPC(model.WithDelegations(ctx, []model.Delegation{forged}), "second", "test.owner", forged.Params); err == nil {
				_ = conn.Close()
				t.Fatal("direct System grant accepted changed arguments")
			}
			conn, _, err := peer.OpenRPC(model.WithDelegations(ctx, []model.Delegation{direct}), "second", "test.owner", allowed)
			if err != nil {
				t.Fatal(err)
			}
			_ = conn.Close()
			if conn, _, err := peer.OpenRPC(model.WithDelegations(ctx, []model.Delegation{direct}), "second", "test.owner", allowed); err == nil {
				_ = conn.Close()
				t.Fatal("direct System grant replayed")
			}
			if err = c.Call(ctx, "second", "access.revoke", map[string]string{"id": direct.ID}, nil); err != nil {
				t.Fatal(err)
			}
			if err = c.Call(ctx, "first", "peers.call", map[string]any{"target": "second", "method": "test.owner", "params": map[string]any{}}, &owner); err != nil {
				t.Fatal("delegated User to System execution", err)
			}
			var task model.Task
			if err = c.Call(ctx, "second", "tasks.start", model.TaskSpec{ID: "hybrid-long", Capability: "test.long", Args: model.JSON(map[string]any{})}, &task); err != nil {
				t.Fatal(err)
			}
			if err = c.Call(ctx, "second", "tasks.get", map[string]any{"id": task.ID}, &task); err != nil || task.Terminal() {
				t.Fatal("accepted task lost its grant", task, err)
			}
			logsCtx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
			var logs strings.Builder
			_ = c.FollowLogs(logsCtx, "second", task.ID, 0, &logs)
			cancel()
			if err = c.Call(ctx, "second", "tasks.get", map[string]any{"id": task.ID}, &task); err != nil || task.Terminal() {
				t.Fatal("log cancellation cancelled System task", task, err)
			}
			monitor := (Client{}).WithStandalone(ctx, c.routing.cfg)
			defer func() { _ = monitor.Close() }()
			if err = c.Close(); err != nil {
				t.Fatal(err)
			}
			if err = monitor.Call(ctx, "second", "tasks.get", map[string]any{"id": task.ID}, &task); err != nil || task.Terminal() {
				t.Fatal("submitting client disconnect cancelled System task or lost ownership", task, err)
			}
			if err = monitor.Call(ctx, "second", "tasks.cancel", map[string]any{"id": task.ID}, nil); err != nil {
				t.Fatal(err)
			}
			task, err = monitor.Wait(ctx, "second", task.ID)
			if err != nil || !task.Terminal() {
				t.Fatal("System cancellation failed", task, err)
			}
		})
	}
}
