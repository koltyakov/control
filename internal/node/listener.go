package node

import (
	"context"
	"encoding/json"
	"errors"
	"net"

	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/transport"
)

func (n *Node) serveTCPListener(ctx context.Context, caller string, stream net.Conn, args json.RawMessage) {
	var q struct {
		Listen   string `json:"listen"`
		Protocol string `json:"protocol"`
	}
	err := json.Unmarshal(args, &q)
	if err == nil {
		err = n.authorize(ctx, caller, "tcp.listen")
	}
	if err == nil && q.Protocol != transport.TCPListenerProtocol {
		err = errors.New("unsupported TCP listener protocol")
	}
	if err != nil {
		_ = writeFrame(stream, model.Response{Error: err.Error()})
		return
	}
	select {
	case n.tcpListeners <- struct{}{}:
		defer func() { <-n.tcpListeners }()
	default:
		_ = writeFrame(stream, model.Response{Error: "TCP listener limit reached"})
		return
	}
	ctx, release, err := n.enterWork(ctx)
	if err != nil {
		_ = writeFrame(stream, model.Response{Error: err.Error()})
		return
	}
	defer release()
	activity := n.beginActivity(ctx, "tunnel", "tcp.listen", n.executionOwner(ctx, caller), caller)
	defer func() { activity.finish(err) }()
	if q.Listen == "" {
		q.Listen = "127.0.0.1:0"
	}
	listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", q.Listen)
	if err != nil {
		_ = writeFrame(stream, model.Response{Error: err.Error()})
		return
	}
	defer func() { _ = listener.Close() }()
	if err = writeFrame(stream, model.Response{Result: model.JSON(map[string]string{"listen": listener.Addr().String(), "protocol": transport.TCPListenerProtocol})}); err != nil {
		return
	}
	activity.phase("listening")
	err = transport.ServeTCPListener(ctx, stream, listener, func(conn net.Conn) net.Conn {
		if a, ok := ctx.Value(authorityContextKey{}).(authority); ok && a.grant != nil {
			conn = &delegationConn{Conn: conn, node: n, state: a.grant}
		}
		return &activityConn{Conn: conn, activity: activity}
	})
}

// OpenTCPListener keeps the local node out of maintenance while it relays a
// process-owned remote listener, including periods without active sockets.
func (n *Node) OpenTCPListener(ctx context.Context, target, listen string) (net.Conn, string, error) {
	ctx, release, err := n.enterWork(ctx)
	if err != nil {
		return nil, "", err
	}
	activity := n.beginActivity(ctx, "tunnel", "tcp.listen", "", target)
	conn, address, err := n.Peer.OpenTCPListener(ctx, target, listen)
	if err != nil {
		release()
		activity.finish(err)
		return nil, "", err
	}
	activity.phase("listening")
	return &workConn{Conn: &activityConn{Conn: conn, activity: activity}, release: release}, address, nil
}
