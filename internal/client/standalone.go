package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/gofrs/flock"
	"github.com/koltyakov/control/internal/buildinfo"
	"github.com/koltyakov/control/internal/identity"
	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/transport"
	"github.com/pion/webrtc/v4"
)

type StandaloneConfig struct {
	Gateway    Admin
	StateDir   string
	RelayOnly  bool
	ICEServers []webrtc.ICEServer
	// AccountRouting uses the requesting account identity even with a local node.
	AccountRouting bool
	// PeerOptions is construction-only and follows in-process client copies.
	// Saved tunnel-service configuration retains the default transport settings.
	PeerOptions []transport.Options `json:"-"`
}

type routing struct {
	ctx         context.Context
	cfg         StandaloneConfig
	mu          sync.Mutex
	initialized bool
	closed      bool
	peer        *transport.Peer
	ownerID     string
	cancel      context.CancelFunc
	attempt     *routingAttempt
}

type routingAttempt struct {
	done   chan struct{}
	cancel context.CancelFunc
	peer   *transport.Peer
	err    error
	// abandoned means the initiator's own cancellation ended the attempt.
	abandoned bool
}

// WithStandalone configures command-scoped peer routing. AccountRouting uses an
// authenticated client session without probing the local API. Other callers
// probe the API first and fall back only on connection refusal, never after
// authentication, timeout, or uncertain application failures.
// Close must be called when the CLI or MCP process finishes.
func (c Client) WithStandalone(ctx context.Context, cfg StandaloneConfig) Client {
	c = c.WithLifetime(ctx)
	cfg.PeerOptions = slices.Clone(cfg.PeerOptions)
	c.routing = &routing{ctx: ctx, cfg: cfg}
	return c
}

func (c Client) backend(ctx context.Context) (*transport.Peer, error) {
	r := c.routing
	if r == nil {
		return nil, nil
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil, errors.New("client is closed")
	}
	if r.initialized {
		peer := r.peer
		r.mu.Unlock()
		return peer, nil
	}
	if a := r.attempt; a != nil {
		r.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-a.done:
			if a.abandoned && ctx.Err() == nil {
				// The initiator's own cancellation says nothing about this
				// caller; start a fresh attempt instead of sharing it.
				return c.backend(ctx)
			}
			return a.peer, a.err
		}
	}
	// The initiating request owns setup cancellation; other callers only wait.
	// A failed attempt is shared by its waiters, never retried within that call.
	setup, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(r.ctx, cancel)
	a := &routingAttempt{done: make(chan struct{}), cancel: cancel}
	r.attempt = a
	r.mu.Unlock()
	candidate := &routing{ctx: r.ctx, cfg: r.cfg}
	localSelected := false
	var err error
	if r.cfg.AccountRouting {
		err = candidate.start(setup)
	} else {
		local := c
		local.routing = nil
		probeCtx, probeCancel := context.WithTimeout(setup, 2*time.Second)
		err = local.Call(probeCtx, "", "node.describe", map[string]any{}, nil)
		probeCancel()
		if connectionRefused(err) {
			err = candidate.start(setup)
		} else {
			// An answering local backend remains selected after an application
			// error. Later calls may try it again, but cannot switch authority.
			localSelected = setup.Err() == nil
		}
	}
	stop()
	cancel()
	r.mu.Lock()
	if err == nil {
		err = ctx.Err()
	}
	if r.closed || r.ctx.Err() != nil {
		err = errors.New("client is closed")
	}
	if err == nil || localSelected && !r.closed && r.ctx.Err() == nil {
		r.initialized = true
		r.peer, r.ownerID, r.cancel = candidate.peer, candidate.ownerID, candidate.cancel
	} else if candidate.peer != nil {
		r.mu.Unlock()
		candidate.cancel()
		_ = candidate.peer.Close()
		r.mu.Lock()
	}
	a.peer, a.err = r.peer, err
	a.abandoned = err != nil && ctx.Err() != nil && !r.closed && r.ctx.Err() == nil
	r.attempt = nil
	close(a.done)
	r.mu.Unlock()
	return a.peer, a.err
}

func (r *routing) start(ctx context.Context) error {
	if r.cfg.Gateway.URL == "" || r.cfg.Gateway.Key == "" {
		return errors.New("no local node and no gateway login; run control login --gateway URL")
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	var auth struct {
		UserID     string `json:"userId"`
		Role       string `json:"role"`
		Delegation bool   `json:"delegation"`
	}
	if err := r.cfg.Gateway.JSON(ctx, http.MethodGet, "/v1/auth", nil, &auth); err != nil {
		return err
	}
	if auth.UserID == "" {
		return errors.New("gateway did not authenticate fleet membership")
	}
	if r.cfg.AccountRouting && !auth.Delegation {
		return errors.New("gateway does not support instruction-bound delegation; upgrade it before executing work")
	}
	if r.cfg.StateDir == "" {
		return errors.New("standalone client state directory is not configured")
	}
	// Different accounts and gateways must not share an immutable peer identity.
	scope := identity.ID([]byte(strings.TrimRight(r.cfg.Gateway.URL, "/") + "\x00" + auth.UserID))
	dir := filepath.Join(r.cfg.StateDir, scope)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	lock := flock.New(filepath.Join(dir, "client.lock"))
	locked, err := lock.TryLockContext(ctx, 50*time.Millisecond)
	defer func() { _ = lock.Close() }()
	if err != nil || !locked {
		_ = lock.Close()
		if err != nil {
			return err
		}
		return errors.New("timed out loading the client owner identity")
	}
	id, err := identity.Load(dir)
	if err != nil {
		return err
	}
	// The lock protects identity creation only. Live connections have independent
	// TLS keys so a tunnel/MCP session cannot block task monitoring or cancellation.
	if err := lock.Unlock(); err != nil {
		return err
	}
	sessionID, err := identity.Generate()
	if err != nil {
		return err
	}
	peer := transport.New(transport.Config{
		Client:  true,
		Gateway: r.cfg.Gateway.URL, Token: r.cfg.Gateway.Key, Identity: sessionID, ClientOwner: id, Route: r.cfg.Gateway.Route,
		Node:      model.Node{ID: sessionID.ID, Name: "cli-" + id.ID[:16], PublicKey: sessionID.Public, OS: runtime.GOOS},
		RelayOnly: r.cfg.RelayOnly, ICEServers: r.cfg.ICEServers,
		RequireDelegation: r.cfg.AccountRouting,
		NameHints:         &nameHints{path: filepath.Join(dir, "names.json")},
	}, nil, r.cfg.PeerOptions...)
	peer.SetSoftware(buildinfo.Current())
	// No listener, providers, health reporting, or update handler is installed.
	peerCtx, peerCancel := context.WithCancel(r.ctx)
	stop := context.AfterFunc(ctx, peerCancel)
	defer stop()
	if err = peer.Start(peerCtx); err != nil {
		peerCancel()
		_ = peer.Close()
		return fmt.Errorf("standalone peer: %w", err)
	}
	r.peer, r.ownerID, r.cancel = peer, id.ID, peerCancel
	return nil
}

func (c Client) Close() error {
	if r := c.routing; r != nil {
		r.mu.Lock()
		r.closed = true
		attempt := r.attempt
		if attempt != nil {
			attempt.cancel()
		}
		cancel, peer := r.cancel, r.peer
		r.mu.Unlock()
		if cancel != nil {
			cancel()
		}
		if peer != nil {
			_ = peer.Close()
		}
		if attempt != nil {
			<-attempt.done
		}
	}
	if c.resources != nil {
		c.resources.close()
	}
	return nil
}

func peerCall(ctx context.Context, peer *transport.Peer, target, method string, params, result any) error {
	_, err := peerCallWithReceipt(ctx, peer, target, method, params, result)
	return err
}

func peerCallWithReceipt(ctx context.Context, peer *transport.Peer, target, method string, params, result any) (*model.TaskSubmissionReceipt, error) {
	var data json.RawMessage
	var receipt *model.TaskSubmissionReceipt
	if target == "" {
		var value any
		var err error
		switch method {
		case "nodes.list":
			value, err = peer.Nodes(ctx)
		case "nodes.select":
			value, err = peer.SelectRequest(ctx, model.JSON(params))
		case "activities.pool":
			value, err = peerActivities(ctx, peer, model.JSON(params))
		default:
			return nil, errors.New("standalone calls require a target machine; only nodes.list, nodes.select, and activities.pool accept an empty target")
		}
		if err != nil {
			return nil, err
		}
		data = model.JSON(value)
	} else {
		conn, response, submission, err := openPeerRPCWithReceipt(ctx, peer, target, method, params)
		if err != nil {
			return submission, err
		}
		_ = conn.Close()
		data = response
		receipt = submission
	}
	if result == nil {
		return receipt, nil
	}
	return receipt, json.Unmarshal(data, result)
}

func peerDownload(ctx context.Context, peer *transport.Peer, a model.Artifact, offset int64, w io.Writer) error {
	if a.Node == "" || offset < 0 || offset > a.Size {
		return errors.New("invalid artifact node or offset")
	}
	conn, data, err := openPeerRPC(ctx, peer, a.Node, "artifacts.open", map[string]any{"id": a.ID, "offset": offset, "grant": a.Grant})
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	var remote model.Artifact
	if err = json.Unmarshal(data, &remote); err != nil {
		return err
	}
	if remote.ID != a.ID || remote.SHA256 != a.ID || remote.Size != a.Size {
		return errors.New("artifact metadata mismatch")
	}
	_, err = io.CopyN(w, conn, remote.Size-offset)
	return err
}

type contextConn struct {
	net.Conn
	stop func() bool
}

func (c *contextConn) CloseWrite() error { return transport.CloseWrite(c.Conn) }

func (c *contextConn) Close() error {
	c.stop()
	_ = c.SetDeadline(time.Now())
	return c.Conn.Close()
}
