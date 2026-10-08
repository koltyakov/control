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

func (n *Node) reserveConnectionTest() (func(), error) {
	select {
	case n.connectionTests <- struct{}{}:
		return func() { <-n.connectionTests }, nil
	default:
		return nil, errors.New("connection test limit reached")
	}
}

func (n *Node) connectionTest(ctx context.Context, args json.RawMessage) (any, error) {
	var q model.ConnectionTestRequest
	if err := json.Unmarshal(args, &q); err != nil {
		return nil, err
	}
	if q.Target == "" || q.Target == n.Config.Name || q.Target == n.Identity.ID {
		return nil, errors.New("connection test requires a different target worker")
	}
	release, err := n.reserveConnectionTest()
	if err != nil {
		return nil, err
	}
	defer release()
	result, err := n.Peer.TestConnection(ctx, q.Target, q.ConnectionTestOptions)
	result.Source = n.Config.Name
	return result, err
}

func (n *Node) serveConnectionTest(ctx context.Context, caller string, conn net.Conn, params json.RawMessage) {
	ctx, cancel := context.WithTimeout(ctx, model.ConnectionTestTimeout)
	defer cancel()
	stop := context.AfterFunc(ctx, func() { _ = conn.SetDeadline(time.Now()); _ = conn.Close() })
	defer stop()
	var q model.ConnectionOpenRequest
	err := json.Unmarshal(params, &q)
	if err == nil {
		err = n.authorize(ctx, caller, model.ConnectionOpenMethod)
	}
	if err == nil && q.Protocol != model.ConnectionTestProtocol {
		err = errors.New("unsupported connection test protocol")
	}
	if err == nil {
		q.ConnectionTestOptions, err = q.Normalize()
	}
	if err != nil {
		_ = writeFrame(conn, model.Response{Error: err.Error()})
		return
	}
	releaseSlot, err := n.reserveConnectionTest()
	if err != nil {
		_ = writeFrame(conn, model.Response{Error: err.Error()})
		return
	}
	defer releaseSlot()
	ctx, release, err := n.enterWork(ctx)
	if err != nil {
		_ = writeFrame(conn, model.Response{Error: err.Error()})
		return
	}
	defer release()
	activity := n.beginActivity(ctx, "transfer", model.ConnectionOpenMethod, n.executionOwner(ctx, caller), caller)
	defer func() { activity.finish(err) }()
	err = transport.ServeConnectionTest(&activityConn{Conn: conn, activity: activity}, q.ConnectionTestOptions)
}
