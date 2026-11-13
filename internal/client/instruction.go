package client

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"strings"
	"sync"

	"github.com/koltyakov/control/internal/identity"
	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/transport"
)

// openPeerRPC explicitly authorizes an instruction-only destination before
// sending work. It never retries an uncertain instruction or changes targets.
func openPeerRPC(ctx context.Context, peer *transport.Peer, target, method string, params any) (net.Conn, json.RawMessage, error) {
	conn, result, _, err := openPeerRPCWithReceipt(ctx, peer, target, method, params)
	return conn, result, err
}

func openPeerRPCWithReceipt(ctx context.Context, peer *transport.Peer, target, method string, params any) (net.Conn, json.RawMessage, *model.TaskSubmissionReceipt, error) {
	if instructionDiscovery(method) || strings.HasPrefix(method, "access.") {
		return peer.OpenRPCWithReceipt(ctx, target, method, params)
	}
	if method == "artifacts.open" {
		var q struct {
			Grant string `json:"grant"`
		}
		if json.Unmarshal(model.JSON(params), &q) == nil && q.Grant != "" {
			// Preserve the supplied artifact grant's absolute expiry and revocation
			// context. Never replace it with fresh account-client authority.
			return peer.OpenRPCWithReceipt(ctx, target, method, params)
		}
	}
	destination, err := peer.Lookup(ctx, target)
	if err != nil {
		return nil, nil, nil, err
	}
	if !destination.InstructionOnly {
		return peer.OpenRPCWithReceipt(ctx, destination.ID, method, params)
	}
	for _, grant := range model.Delegations(ctx) {
		if grant.Target == destination.ID && grant.Matches(method, model.JSON(params)) {
			return peer.OpenRPCWithReceipt(ctx, destination.ID, method, params)
		}
	}
	if method == "tasks.start" {
		var spec model.TaskSpec
		if err = json.Unmarshal(model.JSON(params), &spec); err != nil {
			return nil, nil, nil, err
		}
		if spec.ID == "" {
			spec.ID = identity.NewID()
		}
		params = spec
	}
	var grant model.Delegation
	if err = peerCall(ctx, peer, destination.ID, "access.grant", model.Delegation{Subject: peer.IdentityID(), Method: method, Params: model.JSON(params)}, &grant); err != nil {
		return nil, nil, nil, err
	}
	grants := append([]model.Delegation{}, model.Delegations(ctx)...)
	grants = append(grants, grant)
	conn, result, receipt, err := peer.OpenRPCWithReceipt(model.WithDelegations(ctx, grants), destination.ID, method, params)
	// Retain task grants for durable execution, including uncertain acceptance.
	// All other grants follow the returned stream's lifetime.
	if method == "tasks.start" {
		if unusedSubmission(receipt, method, model.JSON(params)) {
			cleanupDelegations(ctx, peer, []model.Delegation{grant})
		}
		return conn, result, receipt, err
	}
	cleanup := func() {
		cleanupDelegations(ctx, peer, []model.Delegation{grant})
	}
	if err != nil {
		cleanup()
		return nil, nil, receipt, err
	}
	return &instructionConn{Conn: conn, cleanup: cleanup}, result, receipt, nil
}

func instructionDiscovery(method string) bool {
	switch method {
	case "nodes.list", "nodes.select", "node.describe", "capabilities.list":
		return true
	}
	return false
}

type instructionConn struct {
	net.Conn
	cleanup func()
	once    sync.Once
}

func (c *instructionConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(c.cleanup)
	return err
}

func (c *instructionConn) CloseWrite() error { return transport.CloseWrite(c.Conn) }

func openPeerTCP(ctx context.Context, peer *transport.Peer, target, address string) (net.Conn, error) {
	conn, result, err := openPeerRPC(ctx, peer, target, "tcp.open", map[string]any{"address": address, "duplex": transport.DuplexVersion})
	if err != nil {
		return nil, err
	}
	var ack struct {
		Duplex int `json:"duplex"`
	}
	err = json.Unmarshal(result, &ack)
	if err == nil && ack.Duplex != 0 && ack.Duplex != transport.DuplexVersion {
		err = errors.New("unsupported TCP duplex version")
	}
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	if ack.Duplex == transport.DuplexVersion {
		return transport.NewDuplexConn(conn), nil
	}
	return conn, nil
}

func openPeerListener(ctx context.Context, peer *transport.Peer, target, listen string) (net.Conn, string, error) {
	conn, result, err := openPeerRPC(ctx, peer, target, "tcp.listen", map[string]any{"listen": listen, "protocol": transport.TCPListenerProtocol})
	if err != nil {
		return nil, "", err
	}
	var ack struct {
		Listen   string `json:"listen"`
		Protocol string `json:"protocol"`
	}
	err = json.Unmarshal(result, &ack)
	if err == nil && (ack.Protocol != transport.TCPListenerProtocol || ack.Listen == "") {
		err = errors.New("unsupported TCP listener protocol")
	}
	if err != nil {
		_ = conn.Close()
		return nil, "", err
	}
	return conn, ack.Listen, nil
}
