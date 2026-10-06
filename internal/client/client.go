// Package client implements the orchestrator's CLI/MCP component, routing calls
// through a local node API or a standalone authenticated client session.
// Standalone clients are not enrolled fleet machines and cannot execute work.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/transport"
)

// Client submits and observes work on nodes without defining a fixed machine role.
type Client struct {
	URL       string
	Token     string
	routing   *routing
	resources *resources
}

// WithLifetime owns local listeners and streams until Close or cancellation.
func (c Client) WithLifetime(ctx context.Context) Client {
	if c.resources == nil {
		c.resources = newResources(ctx)
	}
	return c
}

func (c Client) lifetimeContext(ctx context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(ctx)
	if c.resources == nil {
		return ctx, cancel
	}
	stop := context.AfterFunc(c.resources.ctx, cancel)
	if c.resources.ctx.Err() != nil {
		cancel()
	}
	return ctx, func() { stop(); cancel() }
}

func (c Client) request(ctx context.Context, path string, value any) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(c.URL, "/")+path, bytes.NewReader(model.JSON(value)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Content-Type", "application/json")
	return http.DefaultClient.Do(req)
}

func (c Client) Call(ctx context.Context, target, method string, params any, result any) error {
	peer, err := c.backend(ctx)
	if err != nil {
		return err
	}
	if peer != nil {
		if c.routing.cfg.AccountRouting && target != "" {
			destination, err := peer.Lookup(ctx, target)
			if err != nil {
				return err
			}
			if !destination.InstructionDelegation {
				return errors.New("target does not support instruction-bound delegation; upgrade it before executing work")
			}
			// Use the identity just checked, not a cached session for a former
			// registration that had the same routing name.
			target = destination.ID
		}
		prepared, grants, err := prepareDelegations(ctx, peer, target, method, model.JSON(params))
		if err != nil {
			return err
		}
		ctx = model.WithDelegations(ctx, grants)
		return peerCall(ctx, peer, target, method, prepared, result)
	}
	resp, err := c.request(ctx, "/v1/call", model.APICall{Target: target, Method: method, Params: model.JSON(params)})
	if err != nil {
		return fmt.Errorf("local node API: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("local node API: %s", resp.Status)
	}
	var response model.Response
	if err = json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&response); err != nil {
		return err
	}
	if response.Error != "" {
		return errors.New(response.Error)
	}
	if result == nil {
		return nil
	}
	return json.Unmarshal(response.Result, result)
}

func (c Client) Wait(ctx context.Context, target, id string) (model.Task, error) {
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		var task model.Task
		if err := c.Call(ctx, target, "tasks.get", map[string]any{"id": id}, &task); err != nil {
			return task, err
		}
		if task.Terminal() {
			return task, nil
		}
		select {
		case <-ctx.Done():
			return task, ctx.Err()
		case <-ticker.C:
		}
	}
}

func (c Client) Download(ctx context.Context, a model.Artifact, offset int64, w io.Writer) error {
	peer, err := c.backend(ctx)
	if err != nil {
		return err
	}
	if peer != nil {
		return peerDownload(ctx, peer, a, offset, w)
	}
	resp, err := c.request(ctx, "/v1/download", map[string]any{"artifact": a, "offset": offset})
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download: %s", resp.Status)
	}
	_, err = io.CopyN(w, resp.Body, a.Size-offset)
	return err
}

func (c Client) Tunnel(ctx context.Context, target, address string) (net.Conn, error) {
	ctx, cancel := context.WithCancel(ctx)
	stopLifetime := func() bool { return false }
	if c.resources != nil {
		if c.resources.ctx.Err() != nil {
			cancel()
			return nil, errors.New("client is closed")
		}
		stopLifetime = context.AfterFunc(c.resources.ctx, cancel)
	}
	cleanup := func() { stopLifetime(); cancel() }
	var conn net.Conn
	var err error
	defer func() {
		if conn == nil {
			cleanup()
		}
	}()
	peer, err := c.backend(ctx)
	if err != nil {
		return nil, err
	}
	if peer != nil {
		if target == "" {
			return nil, errors.New("standalone tunnels require a target machine")
		}
		conn, err = peer.OpenTCP(ctx, target, address)
		if err != nil {
			return nil, err
		}
	} else {
		u := strings.TrimRight(c.URL, "/") + "/v1/tunnel?" + url.Values{"target": {target}, "address": {address}, "duplex": {"1"}}.Encode()
		u = strings.Replace(strings.Replace(u, "https://", "wss://", 1), "http://", "ws://", 1)
		ws, response, err := websocket.Dial(ctx, u, &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer " + c.Token}}})
		if err != nil {
			return nil, err
		}
		conn = websocket.NetConn(ctx, ws, websocket.MessageBinary)
		if response.Header.Get("Control-Tunnel-Duplex") == "1" {
			conn = transport.NewDuplexConn(conn)
		}
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.SetDeadline(time.Now()); _ = conn.Close(); cleanup() })
	return &contextConn{Conn: conn, stop: func() bool { stopped := stop(); cleanup(); return stopped }}, nil
}
