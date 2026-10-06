package node

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"time"

	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/transport"
)

func (n *Node) serveTCP(ctx context.Context, caller string, stream net.Conn, args json.RawMessage) {
	var q struct {
		Address string `json:"address"`
		Duplex  int    `json:"duplex,omitempty"`
	}
	err := json.Unmarshal(args, &q)
	if err == nil && q.Duplex != 0 && q.Duplex != transport.DuplexVersion {
		err = errors.New("unsupported TCP duplex version")
	}
	if err == nil {
		err = n.authorize(ctx, caller, "tcp.open")
	}
	if err != nil {
		_ = writeFrame(stream, model.Response{Error: err.Error()})
		return
	}
	activity := n.beginActivity(ctx, "tunnel", "tcp.accept", n.executionOwner(ctx, caller), caller)
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
	if err = writeFrame(stream, model.Response{Result: model.JSON(map[string]any{"connected": true, "duplex": q.Duplex})}); err != nil {
		return
	}
	if q.Duplex == transport.DuplexVersion {
		stream = transport.NewDuplexConn(stream)
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
	conn, err := n.Peer.OpenTCP(ctx, target, address)
	if err != nil {
		release()
		activity.finish(err)
		return nil, err
	}
	activity.phase("connected")
	return &workConn{Conn: &activityConn{Conn: conn, activity: activity, finishOnClose: true}, release: release}, nil
}

// Bridge preserves TCP half-close when supported and joins both copy directions.
func Bridge(ctx context.Context, a, b net.Conn) {
	transport.Bridge(ctx, a, b)
}
