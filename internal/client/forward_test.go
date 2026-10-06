package client

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"testing"
	"time"

	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/node"
	"github.com/koltyakov/control/internal/transport"
)

type longProvider struct{}

func (longProvider) Capability() model.Capability {
	return model.Capability{Name: "test.long", InputSchema: json.RawMessage(`{"type":"object"}`)}
}

func (longProvider) Run(ctx context.Context, _ json.RawMessage, e node.Execution) (any, error) {
	_, _ = io.WriteString(e.Log, "running\n")
	<-ctx.Done()
	return nil, ctx.Err()
}

func verifyForwardAndLongTask(t *testing.T, ctx context.Context, c, monitor Client) {
	t.Helper()
	verifyEOFForward(t, ctx, c)
	echo, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = echo.Close() }()
	go func() {
		for {
			conn, err := echo.Accept()
			if err != nil {
				return
			}
			go func() { defer func() { _ = conn.Close() }(); _, _ = io.Copy(conn, conn) }()
		}
	}()
	toolCtx, finishTool := context.WithCancel(ctx)
	f, err := c.StartForward(toolCtx, ForwardSpec{Node: "worker", Address: echo.Addr().String()})
	finishTool()
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if len(c.Forwards()) != 1 || len(monitor.Forwards()) != 0 {
		t.Fatal("forward ownership")
	}
	idleReverse, err := c.StartForward(ctx, ForwardSpec{Node: "worker", Address: echo.Addr().String(), Reverse: true})
	if err != nil {
		t.Fatal(err)
	}
	conn, err := net.DialTimeout("tcp", f.Info().Listen, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := conn.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	var reply [4]byte
	if _, err := io.ReadFull(conn, reply[:]); err != nil || string(reply[:]) != "ping" {
		t.Fatal("forward ended with tool call", reply, err)
	}
	var lease model.Lease
	if err := c.Call(ctx, "worker", "leases.acquire", map[string]any{}, &lease); err != nil {
		t.Fatal(err)
	}
	if err := monitor.Call(ctx, "worker", "leases.renew", map[string]any{"id": lease.ID}, &lease); err != nil {
		t.Fatal("lease owner differs between clients", err)
	}
	spec := model.TaskSpec{ID: "long-with-forward", Capability: "test.long", Args: model.JSON(map[string]any{}), LeaseID: lease.ID}
	var task model.Task
	if err := c.Call(ctx, "worker", "tasks.start", spec, &task); err != nil {
		t.Fatal(err)
	}
	if err := monitor.Call(ctx, "worker", "tasks.start", spec, &task); err != nil {
		t.Fatal("idempotency across transports", err)
	}
	for {
		var logs struct{ Text string }
		if err := monitor.Call(ctx, "worker", "tasks.logs", map[string]any{"id": spec.ID}, &logs); err != nil {
			t.Fatal(err)
		}
		if logs.Text == "running\n" {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Read(reply[:]); err == nil {
		t.Fatal("client close retained a forward socket")
	}
	if _, err := net.DialTimeout("tcp", f.Info().Listen, time.Second); err == nil {
		t.Fatal("client close retained listener")
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		conn, err := net.DialTimeout("tcp", idleReverse.Info().Listen, time.Second)
		if err != nil {
			break
		}
		_ = conn.Close()
		if time.Now().After(deadline) {
			t.Fatal("client close retained idle reverse listener")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := monitor.Call(ctx, "worker", "tasks.get", map[string]any{"id": spec.ID}, &task); err != nil || task.Terminal() {
		t.Fatal("task stopped with submitter", task, err)
	}
	if err := monitor.Call(ctx, "worker", "tasks.cancel", map[string]any{"id": spec.ID}, &task); err != nil {
		t.Fatal("concurrent cancellation", err)
	}
	if task, err = monitor.Wait(ctx, "worker", spec.ID); err != nil || task.State != "cancelled" {
		t.Fatal(task, err)
	}
	var logs bytes.Buffer
	if err := monitor.FollowLogs(ctx, "worker", spec.ID, 0, &logs); err != nil {
		t.Fatal(err)
	}
	if logs.String() != "running\n" {
		t.Fatal("lost logs across clients", logs.String())
	}
	if err := monitor.Call(ctx, "worker", "leases.release", map[string]any{"id": lease.ID}, nil); err != nil {
		t.Fatal(err)
	}
}

func verifyEOFForward(t *testing.T, ctx context.Context, c Client) {
	t.Helper()
	for _, forwarded := range []bool{false, true} {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		payload := bytes.Repeat([]byte("input"), 20*1024)
		result := make(chan error, 1)
		go func() {
			defer func() { _ = listener.Close() }()
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
		var conn net.Conn
		var f *Forward
		if forwarded {
			f, err = c.StartForward(ctx, ForwardSpec{Node: "worker", Address: listener.Addr().String()})
			if err == nil {
				conn, err = net.DialTimeout("tcp", f.Info().Listen, time.Second)
			}
		} else {
			conn, err = c.Tunnel(ctx, "worker", listener.Addr().String())
		}
		if err != nil {
			_ = listener.Close()
			t.Fatal(err)
		}
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
		if _, err := conn.Write(payload); err != nil {
			t.Fatal(err)
		}
		if err := transport.CloseWrite(conn); err != nil {
			t.Fatal("write EOF", err)
		}
		response, err := io.ReadAll(conn)
		_ = conn.Close()
		if f != nil {
			if err := c.StopForward(f.Info().ID); err != nil {
				t.Fatal(err)
			}
		}
		if err != nil || !bytes.Equal(payload, response) {
			t.Fatal("EOF-driven response truncated", len(response), err)
		}
		if err := <-result; err != nil {
			t.Fatal(err)
		}
	}
}
