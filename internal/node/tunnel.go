package node

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"time"

	"github.com/koltyakov/control/internal/model"
)

func (n *Node) serveTCP(ctx context.Context, caller string, stream net.Conn, args json.RawMessage) {
	var q struct {
		Address string `json:"address"`
	}
	err := json.Unmarshal(args, &q)
	if err == nil {
		err = n.authorize(ctx, caller, "tcp.open")
	}
	if err != nil {
		_ = writeFrame(stream, model.Response{Error: err.Error()})
		return
	}
	activity := n.beginActivity(ctx, "tunnel", "tcp.accept", caller, caller)
	workCtx, release, gateErr := n.enterWork(ctx)
	if gateErr != nil {
		activity.finish(gateErr)
		_ = writeFrame(stream, model.Response{Error: gateErr.Error()})
		return
	}
	defer release()
	ctx = workCtx
	defer func() { activity.finish(err) }()
	activity.phase("connecting")
	var conn net.Conn
	if err == nil {
		conn, err = (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, "tcp", q.Address)
	}
	if err != nil {
		_ = writeFrame(stream, model.Response{Error: err.Error()})
		return
	}
	defer func() { _ = conn.Close() }()
	if err = writeFrame(stream, model.Response{Result: model.JSON(map[string]any{"connected": true})}); err != nil {
		return
	}
	activity.phase("connected")
	Bridge(ctx, &activityConn{Conn: stream, activity: activity}, conn)
}

func (n *Node) OpenTCP(ctx context.Context, target, address string) (net.Conn, error) {
	workCtx, release, gateErr := n.enterWork(ctx)
	if gateErr != nil {
		return nil, gateErr
	}
	ctx = workCtx
	activity := n.beginActivity(ctx, "tunnel", "tcp.open", "", target)
	activity.phase("connecting")
	conn, _, err := n.open(ctx, target, "tcp.open", map[string]any{"address": address})
	if err != nil {
		release()
		activity.finish(err)
		return nil, err
	}
	activity.phase("connected")
	return &workConn{Conn: &activityConn{Conn: conn, activity: activity}, release: release}, nil
}

// Bridge ends both directions when either closes, or when the context expires.
func Bridge(ctx context.Context, a, b net.Conn) {
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(a, b); done <- struct{}{} }()
	go func() { _, _ = io.Copy(b, a); done <- struct{}{} }()
	select {
	case <-ctx.Done():
	case <-done:
	}
	_ = a.Close()
	_ = b.Close()
}
