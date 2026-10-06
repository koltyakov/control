package node

import (
	"context"
	"io"
	"net"
	"testing"
	"time"

	"github.com/hashicorp/yamux"
	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/transport"
)

func TestDelegationIdleExpiry(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	n := &Node{delegations: map[string]*delegationState{}}
	now := time.Now()
	s := &delegationState{grant: model.Delegation{ID: "idle"}, ctx: ctx, cancel: cancel, lastActivity: now}
	n.delegations[s.grant.ID] = s
	n.expireDelegations(now.Add(time.Hour - time.Nanosecond))
	if len(n.delegations) != 1 || ctx.Err() != nil {
		t.Fatal("grant expired before an hour")
	}
	n.expireDelegations(now.Add(time.Hour))
	if len(n.delegations) != 0 || ctx.Err() == nil {
		t.Fatal("idle grant remained live")
	}
}

func TestDelegationActiveTaskAndStreamActivity(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	n := &Node{delegations: map[string]*delegationState{}}
	now := time.Now()
	s := &delegationState{grant: model.Delegation{ID: "active"}, ctx: ctx, cancel: cancel, lastActivity: now, activeTasks: 1}
	n.delegations[s.grant.ID] = s
	n.expireDelegations(now.Add(2 * time.Hour))
	if ctx.Err() != nil {
		t.Fatal("accepted active task lost authority")
	}
	s.activeTasks = 0
	a, b := net.Pipe()
	defer func() { _ = a.Close(); _ = b.Close() }()
	c := &delegationConn{Conn: a, node: n, state: s}
	before := s.lastActivity
	done := make(chan error, 1)
	go func() { _, err := b.Write([]byte("traffic")); done <- err }()
	if _, err := io.ReadFull(c, make([]byte, 7)); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if !s.lastActivity.After(before) {
		t.Fatal("stream traffic did not refresh idle deadline")
	}
	n.expireDelegations(s.lastActivity.Add(time.Hour))
	if ctx.Err() == nil {
		t.Fatal("idle stream did not expire")
	}
}

func TestDelegationRevokesEstablishedStream(t *testing.T) {
	source, worker, _ := cluster(t, true, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	go func() {
		conn, err := listener.Accept()
		if err == nil {
			defer func() { _ = conn.Close() }()
			_, _ = io.Copy(conn, conn)
		}
	}()
	var grant model.Delegation
	params := map[string]any{"address": listener.Addr().String(), "duplex": 1}
	if err := testCall(t, source, ctx, "worker", "access.grant", model.Delegation{Subject: "source", Method: "tcp.open", Params: model.JSON(params)}, &grant); err != nil {
		t.Fatal(err)
	}
	delegated := model.WithDelegations(ctx, []model.Delegation{grant})
	conn, err := source.Peer.OpenTCP(delegated, "worker", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	if _, err = conn.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	if _, err = io.ReadFull(conn, make([]byte, 4)); err != nil {
		t.Fatal(err)
	}
	if err := testCall(t, source, ctx, "worker", "access.revoke", map[string]any{"id": grant.ID}, nil); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(time.Second))
	if _, err = conn.Read(make([]byte, 1)); err == nil {
		t.Fatal("revocation left stream open")
	}
	eventually(t, ctx, func() bool { return !worker.updateBusy() })
}

func TestDelegationReverseListenerKeepaliveDoesNotRenew(t *testing.T) {
	source, worker, _ := cluster(t, true, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var grant model.Delegation
	params := map[string]any{"listen": "127.0.0.1:0", "protocol": transport.TCPListenerProtocol}
	if err := testCall(t, source, ctx, "worker", "access.grant", model.Delegation{Subject: "source", Method: "tcp.listen", Params: model.JSON(params)}, &grant); err != nil {
		t.Fatal(err)
	}
	conn, _, err := source.Peer.OpenTCPListener(model.WithDelegations(ctx, []model.Delegation{grant}), "worker", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	worker.delegationMu.Lock()
	state := worker.delegations[grant.ID]
	before := state.lastActivity
	worker.delegationMu.Unlock()
	mux, err := yamux.Client(conn, yamux.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = mux.Close() }()
	if _, err = mux.Ping(); err != nil {
		t.Fatal(err)
	}
	worker.delegationMu.Lock()
	after := state.lastActivity
	worker.delegationMu.Unlock()
	if !after.Equal(before) {
		t.Fatal("reverse-listener keepalive renewed idle access")
	}
	worker.expireDelegations(before.Add(time.Hour))
	select {
	case <-mux.CloseChan():
	case <-ctx.Done():
		t.Fatal("idle reverse listener did not close")
	}
	eventually(t, ctx, func() bool { return !worker.updateBusy() })
}
