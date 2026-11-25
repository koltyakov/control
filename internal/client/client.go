// Package client implements the orchestrator's CLI/MCP component, routing calls
// through a local node API or a standalone authenticated client session.
// Standalone clients are not enrolled fleet machines and expose no execution capabilities.
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
	"github.com/koltyakov/control/internal/runbooks"
	"github.com/koltyakov/control/internal/transport"
)

// Client submits and observes work on nodes without defining a fixed machine role.
type Client struct {
	URL          string
	Token        string
	routing      *routing
	resources    *resources
	pool         *poolActivityCache
	persistent   *persistentAccess
	runbookRoots []runbooks.Root
}

// WithLifetime owns local listeners and streams until Close or cancellation.
func (c Client) WithLifetime(ctx context.Context) Client {
	if c.resources == nil {
		c.resources = newResources(ctx)
	}
	if c.pool == nil {
		c.pool = &poolActivityCache{}
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
	return localAPIClient.Do(req)
}

func (c Client) Call(ctx context.Context, target, method string, params any, result any) error {
	if method == "nodes.select" {
		return c.selectNode(ctx, target, params, result)
	}
	if method == model.TaskCheckMethod {
		return taskCheckError(c.call(ctx, target, method, params, result))
	}
	if method != "node.describe" || result == nil {
		return c.call(ctx, target, method, params, result)
	}
	return c.describeWithRunbooks(ctx, target, params, result)
}

func (c Client) call(ctx context.Context, target, method string, params any, result any) error {
	// Resolve workflow select/each targets before any grant or submission, on
	// every route, so no coordinator or worker ever chooses machines.
	if method == "workflow.run" || method == "tasks.start" || method == model.TaskCheckMethod {
		resolved, err := c.resolveWorkflowCall(ctx, target, method, model.JSON(params))
		if err != nil {
			return err
		}
		params = resolved
	}
	if err := c.requireTaskTagsOnce(ctx, target, method, params); err != nil {
		return err
	}
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
		receipt, err := peerCallWithReceipt(ctx, peer, target, method, prepared, result)
		cleanupDelegations(ctx, peer, releasableDelegations(method, prepared, grants, receipt))
		return err
	}
	if plan, ok := submittedWorkflow(method, model.JSON(params)); ok && plan.HasAlways() {
		// The local node forwards the plan unchanged; check its coordinator
		// before submission, as account routing does before preparing grants.
		var capabilities []model.Capability
		if err := c.call(ctx, target, "capabilities.list", nil, &capabilities); err != nil {
			return err
		}
		if !model.WorkflowAlwaysSupported(capabilities) {
			return errWorkflowAlwaysUnsupported
		}
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
		return response.Err()
	}
	if result == nil {
		return nil
	}
	return json.Unmarshal(response.Result, result)
}

// PrepareTaskWait overlaps setup of the lane used by blocking task reads with a
// submission in flight. It sends no request and cannot affect acceptance;
// setup errors are ignored because Wait reports its own failures.
func (c Client) PrepareTaskWait(ctx context.Context, target string) {
	peer, err := c.backend(ctx)
	if err != nil || peer == nil || target == "" {
		return
	}
	if c.routing.cfg.AccountRouting {
		destination, err := peer.Lookup(ctx, target)
		if err != nil {
			return
		}
		target = destination.ID
	}
	_ = peer.Prepare(ctx, target, transport.InteractiveLane)
}

// taskWaitSeconds asks the worker to hold tasks.get until the task finishes.
const taskWaitSeconds = 30

// Wait returns when the task is terminal. Current workers answer a blocking
// tasks.get as soon as the task finishes; older workers ignore waitSeconds and
// answer at once, so the loop keeps a minimum poll interval for them. On error,
// it returns the last successfully observed task, if any, without cancelling it.
func (c Client) Wait(ctx context.Context, target, id string) (model.Task, error) {
	const interval = 250 * time.Millisecond
	var task model.Task
	for {
		started := time.Now()
		var observed model.Task
		if err := c.Call(ctx, target, "tasks.get", map[string]any{"id": id, "waitSeconds": taskWaitSeconds}, &observed); err != nil {
			return task, err
		}
		task = observed
		if task.Terminal() {
			return task, nil
		}
		if remaining := interval - time.Since(started); remaining > 0 {
			select {
			case <-ctx.Done():
				return task, ctx.Err()
			case <-time.After(remaining):
			}
		} else if err := ctx.Err(); err != nil {
			return task, err
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
	return c.tunnel(ctx, target, address, openPeerTCP)
}

// tunnel lets SOCKS sockets replace the peer opener; see openSOCKSPeerTCP.
func (c Client) tunnel(ctx context.Context, target, address string, openPeer func(context.Context, *transport.Peer, string, string) (net.Conn, error)) (net.Conn, error) {
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
		conn, err = openPeer(ctx, peer, target, address)
		if err != nil {
			return nil, err
		}
	} else {
		u := strings.TrimRight(c.URL, "/") + "/v1/tunnel?" + url.Values{"target": {target}, "address": {address}, "duplex": {"1"}}.Encode()
		u = strings.Replace(strings.Replace(u, "https://", "wss://", 1), "http://", "ws://", 1)
		ws, response, err := websocket.Dial(ctx, u, &websocket.DialOptions{HTTPClient: localAPIClient, HTTPHeader: http.Header{"Authorization": {"Bearer " + c.Token}}})
		if err != nil {
			if response != nil && response.StatusCode == http.StatusServiceUnavailable {
				return nil, transport.ErrStreamCapacity
			}
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
