package transport

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"time"

	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/wire"
)

// OpenRPC sends one request. The returned stream may contain a bulk payload.
// It never replays a request after writing application bytes.
func (p *Peer) OpenRPC(ctx context.Context, target, method string, params any) (net.Conn, json.RawMessage, error) {
	lane := ControlLane
	if method == "artifacts.open" {
		lane = BulkLane
	}
	if method == "tcp.open" {
		lane = InteractiveLane
	}
	if method == "tasks.logs" {
		var q struct {
			Follow bool `json:"follow"`
		}
		if json.Unmarshal(model.JSON(params), &q) == nil && q.Follow {
			lane = InteractiveLane
		}
	}
	conn, err := p.OpenLane(ctx, target, lane)
	if err != nil {
		return nil, nil, err
	}
	stop := context.AfterFunc(ctx, func() {
		// yamux Close sends FIN but can leave reads waiting for the peer.
		_ = conn.SetDeadline(time.Now())
		_ = conn.Close()
	})
	defer stop()
	deadline, _ := ctx.Deadline()
	if err = wire.WriteFrame(conn, model.Request{Version: model.Version, Method: method, Params: model.JSON(params), Deadline: deadline}); err != nil {
		_ = conn.Close()
		return nil, nil, err
	}
	var response model.Response
	if err = wire.ReadFrame(conn, &response); err != nil {
		_ = conn.Close()
		return nil, nil, err
	}
	if response.Error != "" {
		_ = conn.Close()
		return nil, nil, errors.New(response.Error)
	}
	return conn, response.Result, nil
}

// OpenTCP negotiates duplex records. Older receivers return a raw stream.
func (p *Peer) OpenTCP(ctx context.Context, target, address string) (net.Conn, error) {
	conn, result, err := p.OpenRPC(ctx, target, "tcp.open", map[string]any{"address": address, "duplex": DuplexVersion})
	if err != nil {
		return nil, err
	}
	var ack struct {
		Duplex int `json:"duplex"`
	}
	if err := json.Unmarshal(result, &ack); err != nil {
		_ = conn.Close()
		return nil, err
	}
	if ack.Duplex == DuplexVersion {
		return NewDuplexConn(conn), nil
	}
	if ack.Duplex != 0 {
		_ = conn.Close()
		return nil, errors.New("unsupported TCP duplex version")
	}
	return conn, nil
}

// Select uses the same admission filters for node and standalone callers.
func (p *Peer) Select(ctx context.Context, args json.RawMessage) (model.Node, error) {
	var query struct {
		Labels     map[string]string `json:"labels"`
		Capability string            `json:"capability"`
	}
	if err := json.Unmarshal(args, &query); err != nil {
		return model.Node{}, err
	}
	nodes, err := p.Nodes(ctx)
	if err != nil {
		return model.Node{}, err
	}
	for _, peer := range nodes {
		if !peer.Online || peer.Disabled || peer.ControlPending {
			continue
		}
		matches := true
		for k, v := range query.Labels {
			if peer.Labels[k] != v {
				matches = false
			}
		}
		if query.Capability != "" {
			found := false
			for _, c := range peer.Capabilities {
				if c.Name == query.Capability {
					found = true
				}
			}
			matches = matches && found
		}
		if matches {
			return peer, nil
		}
	}
	return model.Node{}, errors.New("no online enabled node matches the selector")
}
