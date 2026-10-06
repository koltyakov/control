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
	err         error
}

// WithStandalone selects a backend with a read-only probe before any work is
// submitted. Only a refused local connection permits fallback. Authentication,
// timeouts, and uncertain application failures never cause a backend switch.
// Close must be called when the CLI or MCP process finishes.
func (c Client) WithStandalone(ctx context.Context, cfg StandaloneConfig) Client {
	c = c.WithLifetime(ctx)
	c.routing = &routing{ctx: ctx, cfg: cfg}
	return c
}

func (c Client) backend(ctx context.Context) (*transport.Peer, error) {
	r := c.routing
	if r == nil {
		return nil, nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil, errors.New("client is closed")
	}
	if !r.initialized {
		r.initialized = true
		local := c
		local.routing = nil
		probeCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		err := local.Call(probeCtx, "", "node.describe", map[string]any{}, nil)
		cancel()
		if err != nil {
			if !connectionRefused(err) {
				r.err = err
			} else {
				r.err = r.start(ctx)
			}
		}
	}
	return r.peer, r.err
}

func (r *routing) start(ctx context.Context) error {
	if r.cfg.Gateway.URL == "" || r.cfg.Gateway.Key == "" {
		return errors.New("no local node and no gateway login; run control login --gateway URL")
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	var auth struct {
		UserID string `json:"userId"`
	}
	if err := r.cfg.Gateway.JSON(ctx, http.MethodGet, "/v1/auth", nil, &auth); err != nil {
		return err
	}
	if auth.UserID == "" {
		return errors.New("gateway did not authenticate fleet membership")
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
		Gateway: r.cfg.Gateway.URL, Token: r.cfg.Gateway.Key, Identity: sessionID, ClientOwner: id,
		Node:      model.Node{ID: sessionID.ID, Name: "cli-" + id.ID[:16], PublicKey: sessionID.Public, OS: runtime.GOOS},
		RelayOnly: r.cfg.RelayOnly, ICEServers: r.cfg.ICEServers,
	}, nil)
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
	if c.resources != nil {
		c.resources.close()
	}
	if c.routing == nil {
		return nil
	}
	r := c.routing
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil
	}
	r.closed = true
	if r.cancel != nil {
		r.cancel()
	}
	if r.peer != nil {
		_ = r.peer.Close()
	}
	return nil
}

func peerCall(ctx context.Context, peer *transport.Peer, target, method string, params, result any) error {
	var data json.RawMessage
	if target == "" {
		var value any
		var err error
		switch method {
		case "nodes.list":
			value, err = peer.Nodes(ctx)
		case "nodes.select":
			value, err = peer.Select(ctx, model.JSON(params))
		default:
			return errors.New("standalone calls require a target machine; only nodes.list and nodes.select accept an empty target")
		}
		if err != nil {
			return err
		}
		data = model.JSON(value)
	} else {
		conn, response, err := peer.OpenRPC(ctx, target, method, params)
		if err != nil {
			return err
		}
		_ = conn.Close()
		data = response
	}
	if result == nil {
		return nil
	}
	return json.Unmarshal(data, result)
}

func peerDownload(ctx context.Context, peer *transport.Peer, a model.Artifact, offset int64, w io.Writer) error {
	if a.Node == "" || offset < 0 || offset > a.Size {
		return errors.New("invalid artifact node or offset")
	}
	conn, data, err := peer.OpenRPC(ctx, a.Node, "artifacts.open", map[string]any{"id": a.ID, "offset": offset, "grant": a.Grant})
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
