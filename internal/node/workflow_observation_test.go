package node

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/transport"
	"github.com/koltyakov/control/internal/wire"
)

func TestTransientObservationClassification(t *testing.T) {
	live := context.Background()
	cancelled, cancel := context.WithCancel(live)
	cancel()
	for _, tc := range []struct {
		name string
		ctx  context.Context
		err  error
		want bool
	}{
		{"remote application reply", live, model.Response{Error: "task not found", Code: "operation_failed", Outcome: "unknown"}.Err(), false},
		{"remote unknown-outcome reply", live, model.Response{Error: "delegation is missing", Outcome: "unknown"}.Err(), false},
		{"local dial failure", live, model.OperationError(errors.New("unknown node \"gone\""), "not_sent"), false},
		{"stream closed by peer", live, model.OperationError(io.EOF, "unknown"), true},
		{"truncated reply", live, model.OperationError(fmt.Errorf("read: %w", io.ErrUnexpectedEOF), "unknown"), true},
		{"transient setup failure", live, model.OperationError(fmt.Errorf("open lane: %w", transport.ErrTransientSetup), "not_sent"), true},
		{"remote reply naming a drain", live, model.Response{Error: "gateway relay is draining for maintenance", Outcome: "not_sent"}.Err(), false},
		{"closed connection", live, model.OperationError(net.ErrClosed, "unknown"), true},
		{"caller cancellation", cancelled, model.OperationError(io.EOF, "unknown"), false},
		{"context cause", live, model.OperationError(context.Canceled, "unknown"), false},
		{"oversized frame", live, model.OperationError(wire.ErrFrameTooLarge, "not_sent"), false},
		{"tls alert", live, model.OperationError(tls.AlertError(42), "not_sent"), false},
		{"setup failure after caller cancellation", cancelled, model.OperationError(transport.ErrTransientSetup, "not_sent"), false},
		{"plain error", live, io.EOF, false},
	} {
		if got := transientObservation(tc.ctx, tc.err); got != tc.want {
			t.Errorf("%s: transient = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// statusReply matches the worker's tasks.get reply for one child. Submission
// replies (tasks.start) carry a receipt and are never touched.
func statusReply(r *model.Response, id string) bool {
	if r.Submission != nil || r.Error != "" {
		return false
	}
	var task model.Task
	return json.Unmarshal(r.Result, &task) == nil && task.ID == id
}

// A dropped status reply over a real peer stream is an observation failure,
// not a child failure. The workflow repeats only the read, never the start.
func TestWorkflowRetriesDroppedStatusReadWithoutReplay(t *testing.T) {
	for _, relay := range []bool{false, true} {
		t.Run(fmt.Sprintf("relay=%v", relay), func(t *testing.T) {
			t.Parallel()
			var dropped atomic.Int32
			f := newLifetimeFixture(t, relay, false, func(r *model.Response) bool {
				return !statusReply(r, "child-a") || dropped.Add(1) > 1
			})
			spec := lifetimeWorkflow()
			if err := f.client.Call(f.ctx, "first", "tasks.start", spec, nil); err != nil {
				t.Fatal(err)
			}
			parent, err := f.client.Wait(f.ctx, "first", spec.ID)
			if err != nil || parent.State != "succeeded" {
				t.Fatal("dropped status read failed the workflow", parent.State, parent.Error, err)
			}
			if dropped.Load() < 2 || f.calls.Load() != 2 {
				t.Fatal("status read was not retried exactly around one drop, or a child replayed", dropped.Load(), f.calls.Load())
			}
			var child model.Task
			if err := f.client.Call(f.ctx, "second", "tasks.get", map[string]string{"id": "child-a"}, &child); err != nil || child.State != "succeeded" {
				t.Fatal("observed child did not complete normally", child.State, err)
			}
			f.assertTransport(t)
		})
	}
}

// A permanent observation error leaves the accepted child running. The
// workflow reports it as unresolved, keeps its identity, stops scheduling, and
// does not cancel healthy work.
func TestWorkflowPermanentObservationErrorLeavesChildUnresolved(t *testing.T) {
	for _, relay := range []bool{false, true} {
		t.Run(fmt.Sprintf("relay=%v", relay), func(t *testing.T) {
			t.Parallel()
			var armed atomic.Bool
			armed.Store(true)
			f := newLifetimeFixture(t, relay, false, func(r *model.Response) bool {
				if armed.Load() && statusReply(r, "child-a") {
					*r = model.Response{Error: "status store unavailable", Code: "operation_failed", Outcome: "unknown"}
				}
				return true
			})
			spec := lifetimeWorkflow()
			spec.Args = model.JSON(map[string]any{"steps": []model.WorkflowStep{
				{Name: "a", Target: "second", Task: lifetimeTask("child-a", true)},
				{Name: "b", Target: "second", Task: lifetimeTask("child-b", false), Needs: []string{"a"}},
			}})
			// The parent deadline bounds the worker's blocking status wait (it
			// answers a second early), so the error reply arrives promptly while
			// the workflow itself is still live.
			spec.TimeoutSeconds = 6
			if err := f.client.Call(f.ctx, "first", "tasks.start", spec, nil); err != nil {
				t.Fatal(err)
			}
			f.waitEntered(t)
			parent, err := f.client.Wait(f.ctx, "first", spec.ID)
			if err != nil || parent.State != "failed" || !strings.Contains(parent.Error, "unresolved") || !strings.Contains(parent.Error, "child-a") {
				t.Fatal("permanent observation error was not reported as unresolved", parent.State, parent.Error, err)
			}
			var children map[string]model.Task
			if err := json.Unmarshal(parent.Result, &children); err != nil {
				t.Fatal(err)
			}
			if child, ok := children["a"]; !ok || child.ID != "child-a" || child.Terminal() {
				t.Fatal("unresolved child identity or last observation was lost", children)
			}
			if _, started := children["b"]; started {
				t.Fatal("workflow scheduled a dependent after an unresolved step")
			}
			armed.Store(false)
			var child model.Task
			if err := f.client.Call(f.ctx, "second", "tasks.get", map[string]string{"id": "child-a"}, &child); err != nil || child.Terminal() {
				t.Fatal("unresolved child was cancelled or lost", child.State, err)
			}
			f.release()
			child, err = f.client.Wait(f.ctx, "second", "child-a")
			if err != nil || child.State != "succeeded" || f.calls.Load() != 1 {
				t.Fatal("healthy child did not finish once", child.State, err, f.calls.Load())
			}
			f.assertTransport(t)
		})
	}
}

// A real gateway outage interrupts the parent's in-flight status wait. Over
// the relay, carriers and new setups fail until peers reconnect. The workflow
// repeats only its status read and completes without resubmitting or
// cancelling the accepted child.
func TestWorkflowStatusReadSurvivesGatewayReconnect(t *testing.T) {
	for _, relay := range []bool{false, true} {
		t.Run(fmt.Sprintf("relay=%v", relay), func(t *testing.T) {
			t.Parallel()
			f := newLifetimeFixture(t, relay, false, nil)
			spec := lifetimeWorkflow()
			spec.Args = model.JSON(map[string]any{"steps": []model.WorkflowStep{
				{Name: "a", Target: "second", Task: lifetimeTask("child-a", true)},
				{Name: "b", Target: "second", Task: lifetimeTask("child-b", false), Needs: []string{"a"}},
			}})
			if err := f.client.Call(f.ctx, "first", "tasks.start", spec, nil); err != nil {
				t.Fatal(err)
			}
			f.waitEntered(t)
			// Let the parent's blocking status read reach the worker.
			time.Sleep(300 * time.Millisecond)
			if dropped := f.gateway.drop(); dropped == 0 {
				t.Fatal("fixture dropped no gateway connections")
			}
			// Test-side reads may fail while every party reconnects.
			read := func(target, id string) (model.Task, error) {
				var task model.Task
				err := f.client.Call(f.ctx, target, "tasks.get", map[string]string{"id": id}, &task)
				return task, err
			}
			eventually(t, f.ctx, func() bool { _, err := read("second", "child-a"); return err == nil })
			f.release()
			var parent model.Task
			eventually(t, f.ctx, func() bool {
				task, err := read("first", spec.ID)
				parent = task
				return err == nil && task.Terminal()
			})
			if parent.State != "succeeded" {
				t.Fatal("gateway reconnect failed the workflow", parent.State, parent.Error)
			}
			if f.calls.Load() != 2 {
				t.Fatal("a child was replayed or skipped", f.calls.Load())
			}
			if child, err := read("second", "child-a"); err != nil || child.State != "succeeded" {
				t.Fatal("accepted child was cancelled or lost", child.State, err)
			}
			var logs struct{ Text string }
			if err := f.client.Call(f.ctx, "first", "tasks.logs", map[string]any{"id": spec.ID}, &logs); err != nil {
				t.Fatal(err)
			}
			if strings.Count(logs.Text, "task=child-a") != 1 || strings.Contains(logs.Text, "unresolved") {
				t.Fatalf("child was resubmitted or reported unresolved:\n%s", logs.Text)
			}
			t.Logf("workflow log:\n%s", logs.Text)
			if relay && !strings.Contains(logs.Text, "failed; retrying") {
				t.Fatalf("relay outage did not exercise the status retry:\n%s", logs.Text)
			}
		})
	}
}

// Cancelling a workflow while a child's acceptance reply is in flight still
// cancels that child by its stable ID, without resubmitting it.
func TestWorkflowCancelDuringChildSubmissionCancelsChild(t *testing.T) {
	for _, relay := range []bool{false, true} {
		t.Run(fmt.Sprintf("relay=%v", relay), func(t *testing.T) {
			t.Parallel()
			accepted := make(chan struct{})
			unblock := make(chan struct{})
			var once atomic.Bool
			f := newLifetimeFixture(t, relay, false, func(r *model.Response) bool {
				if r.Submission != nil && r.Submission.TaskID == "child-a" && once.CompareAndSwap(false, true) {
					close(accepted)
					<-unblock
					return false
				}
				return true
			})
			t.Cleanup(func() {
				select {
				case <-unblock:
				default:
					close(unblock)
				}
			})
			spec := lifetimeWorkflow()
			spec.Args = model.JSON(map[string]any{"steps": []model.WorkflowStep{
				{Name: "a", Target: "second", Task: lifetimeTask("child-a", true)},
			}})
			if err := f.client.Call(f.ctx, "first", "tasks.start", spec, nil); err != nil {
				t.Fatal(err)
			}
			select {
			case <-accepted:
			case <-f.ctx.Done():
				t.Fatal("child submission did not reach the worker", f.ctx.Err())
			}
			f.waitEntered(t)
			if err := f.client.Call(f.ctx, "first", "tasks.cancel", map[string]string{"id": spec.ID}, nil); err != nil {
				t.Fatal(err)
			}
			child, err := f.client.Wait(f.ctx, "second", "child-a")
			close(unblock)
			if err != nil || child.State != "cancelled" {
				t.Fatal("in-flight child was not cancelled", child.State, err)
			}
			if f.calls.Load() != 1 {
				t.Fatal("child was resubmitted", f.calls.Load())
			}
		})
	}
}
