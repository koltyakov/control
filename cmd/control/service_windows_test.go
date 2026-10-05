package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"golang.org/x/sys/windows/svc"
)

func TestWindowsServiceWaitsForGracefulShutdown(t *testing.T) {
	for _, command := range []svc.Cmd{svc.Stop, svc.Shutdown} {
		t.Run(commandName(command), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			cancelled := make(chan struct{})
			release := make(chan struct{}, 1)
			defer close(release)
			handler := &windowsNodeService{ctx: ctx, run: func(ctx context.Context) error {
				<-ctx.Done()
				close(cancelled)
				select {
				case <-release:
				case <-time.After(4 * time.Second):
				}
				return nil
			}}
			requests := make(chan svc.ChangeRequest, 2)
			statuses := make(chan svc.Status, 16)
			done := make(chan uint32, 1)
			go func() { _, code := handler.Execute(nil, requests, statuses); done <- code }()
			awaitServiceState(t, ctx, statuses, svc.Running)
			requests <- svc.ChangeRequest{Cmd: svc.Interrogate}
			awaitServiceState(t, ctx, statuses, svc.Running)
			requests <- svc.ChangeRequest{Cmd: command}
			awaitServiceState(t, ctx, statuses, svc.StopPending)
			select {
			case <-cancelled:
			case <-ctx.Done():
				t.Fatal("shutdown did not cancel the supervisor")
			}
			select {
			case <-done:
				t.Fatal("SCM handler returned before the supervisor stopped")
			default:
			}
			// Release the callback without closing the channel twice on failure.
			release <- struct{}{}
			select {
			case code := <-done:
				if code != 0 {
					t.Fatalf("graceful stop returned %d", code)
				}
			case <-ctx.Done():
				t.Fatal("service did not finish")
			}
		})
	}
}

func TestWindowsServiceReportsFailureForRecovery(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	handler := &windowsNodeService{ctx: ctx, run: func(context.Context) error {
		return errors.New("supervisor failed")
	}}
	specific, code := handler.Execute(nil, make(chan svc.ChangeRequest), make(chan svc.Status, 16))
	if !specific || code == 0 {
		t.Fatal("service failure must trigger SCM recovery")
	}
}

func awaitServiceState(t *testing.T, ctx context.Context, statuses <-chan svc.Status, want svc.State) {
	t.Helper()
	for {
		select {
		case status := <-statuses:
			if status.State == want {
				return
			}
		case <-ctx.Done():
			t.Fatalf("service did not reach state %d", want)
		}
	}
}

func commandName(cmd svc.Cmd) string {
	if cmd == svc.Stop {
		return "stop"
	}
	return "shutdown"
}
