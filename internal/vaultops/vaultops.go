// Package vaultops dispatches a bounded set of typed fleet operations for a
// protected broker. The broker holds account credentials that its callers
// must never read. Each frontend connection gets its own account client and
// transport session; retained tunnels run in the broker process, independent
// of any connection or grant, and can be listed and disposed while offline.
//
// Operations reach the fleet only through the configured gateway, and callers
// cannot change the gateway, route or transport settings. Forwards and
// retained tunnels keep the client's existing validation, including
// allowNonLoopback and reverse destinations dialed from the broker host.
// Unlike the CLI and MCP client, the broker exposes no operation that reads
// files, the clipboard or the local node API of its host; node.describe adds
// worker runbook stores but never the broker host's project or global stores;
// and call rejects methods that open raw streams.
package vaultops

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"sync"

	"github.com/koltyakov/control/internal/client"
	"github.com/koltyakov/control/internal/secrets"
)

const (
	// MaxSessions bounds concurrent frontend connections, each with its own
	// gateway connection and transport identity.
	MaxSessions = 64
	// MaxInFlight bounds concurrent operations of one session. Excess
	// requests are rejected before anything is sent.
	MaxInFlight = 16
	// MaxArgsBytes bounds one operation's JSON arguments.
	MaxArgsBytes = 8 << 20
)

// Runtime owns the broker's retained tunnels and its frontend sessions.
type Runtime struct {
	ctx     context.Context
	cancel  context.CancelFunc
	cfg     client.StandaloneConfig
	tunnels *client.TunnelRuntime

	mu       sync.Mutex
	closed   bool
	sessions map[*Handler]struct{}
	// account caches one successful credential role check.
	account bool
}

// Open validates the broker configuration, then restores retained tunnels
// from privateTunnelDir. cfg.StateDir and privateTunnelDir must be private to
// the broker's OS user; on Unix-like systems both are created with mode 0700
// and rejected when another user owns them or group/other access is granted.
// Retained tunnels keep running until Close or ctx cancellation, regardless
// of which session created them.
func Open(ctx context.Context, cfg client.StandaloneConfig, privateTunnelDir string) (*Runtime, error) {
	if cfg.Gateway.URL == "" || cfg.Gateway.Key == "" {
		return nil, errors.New("broker requires a saved account login")
	}
	if !secrets.SecureURL(cfg.Gateway.URL) {
		return nil, errors.New("broker gateway must use HTTPS, or HTTP on a loopback address for development")
	}
	if cfg.StateDir == "" || !filepath.IsAbs(cfg.StateDir) || privateTunnelDir == "" || !filepath.IsAbs(privateTunnelDir) {
		return nil, errors.New("broker state and tunnel directories must be absolute paths")
	}
	cfg.AccountRouting = true
	cfg.PeerOptions = slices.Clone(cfg.PeerOptions)
	for _, dir := range []string{cfg.StateDir, privateTunnelDir} {
		if err := ensurePrivateDir(dir); err != nil {
			return nil, err
		}
	}
	runtimeCtx, cancel := context.WithCancel(ctx)
	tunnels, err := client.OpenTunnelRuntime(runtimeCtx, privateTunnelDir, cfg)
	if err != nil {
		cancel()
		return nil, err
	}
	return &Runtime{ctx: runtimeCtx, cancel: cancel, cfg: cfg, tunnels: tunnels, sessions: map[*Handler]struct{}{}}, nil
}

// NewSession creates the account client of one frontend connection. Its
// forwards and transport session last until connectionCtx ends, Close is
// called, or the runtime closes; revoking a grant cancels only the request
// contexts passed to Do. The session's transport is set up lazily by the
// first operation that needs it.
//
// NewSession never fails. When the runtime is closed or MaxSessions sessions
// are open, it returns a rejected handler whose every operation fails with
// outcome not_sent and which holds no client or transport.
func (r *Runtime) NewSession(connectionCtx context.Context) *Handler {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed || r.ctx.Err() != nil {
		return rejected(errors.New("broker is closed"))
	}
	if len(r.sessions) >= MaxSessions {
		return rejected(errors.New("broker session limit reached"))
	}
	ctx, cancel := context.WithCancel(connectionCtx)
	stopRuntime := context.AfterFunc(r.ctx, cancel)
	h := &Handler{runtime: r, ctx: ctx, cancel: cancel, stop: stopRuntime, slots: make(chan struct{}, MaxInFlight), done: make(chan struct{})}
	h.client = client.Client{}.WithoutLocalRunbooks().WithStandalone(ctx, r.cfg)
	r.sessions[h] = struct{}{}
	// A connection that ends without Close still releases its session.
	context.AfterFunc(ctx, h.release)
	return h
}

// rejected returns a handler that refuses every operation with err.
func rejected(err error) *Handler {
	return &Handler{rejection: notSent(err)}
}

// Close ends every session and its forwards, then stops retained listeners
// without removing their definitions; a later Open restores them.
func (r *Runtime) Close() error {
	r.mu.Lock()
	r.closed = true
	sessions := make([]*Handler, 0, len(r.sessions))
	for h := range r.sessions {
		sessions = append(sessions, h)
	}
	r.mu.Unlock()
	for _, h := range sessions {
		_ = h.Close()
	}
	r.cancel()
	r.tunnels.Close()
	return nil
}

func (r *Runtime) remove(h *Handler) {
	r.mu.Lock()
	delete(r.sessions, h)
	r.mu.Unlock()
}

// checkAccount accepts only a fleet account key (role user) or an
// execution-only orchestrator key. Superuser and machine credentials are
// refused, so the broker never holds operator authority. A successful check is cached; failures are checked again by the
// next operation. Offline tunnel listing and disposal do not need it.
func (r *Runtime) checkAccount(ctx context.Context) error {
	r.mu.Lock()
	verified := r.account
	r.mu.Unlock()
	if verified {
		return nil
	}
	role, err := r.cfg.Gateway.AuthRole(ctx)
	if err != nil {
		return err
	}
	if role == "superuser" || !client.ExecutionRole(role) {
		return fmt.Errorf("broker credentials have role %q; fleet operations require an account or orchestrator key", role)
	}
	r.mu.Lock()
	r.account = true
	r.mu.Unlock()
	return nil
}

// Handler serves one frontend connection.
type Handler struct {
	runtime *Runtime
	ctx     context.Context
	cancel  context.CancelFunc
	client  client.Client
	slots   chan struct{}
	stop    func() bool
	once    sync.Once
	done    chan struct{}
	// rejection is set on a handler NewSession could not admit.
	rejection error
}

// Do runs one allowlisted operation. ctx is the request's grant context:
// cancelling it abandons the request and closes its remote stream, but does
// not stop forwards that an earlier request already started. Errors for
// requests rejected before anything was sent carry outcome "not_sent"; other
// errors have unknown effects and must not be replayed automatically.
func (h *Handler) Do(ctx context.Context, op string, args json.RawMessage) (json.RawMessage, error) {
	if h.rejection != nil {
		return nil, h.rejection
	}
	if h.ctx.Err() != nil {
		return nil, notSent(errors.New("broker session is closed"))
	}
	select {
	case h.slots <- struct{}{}:
		defer func() { <-h.slots }()
	default:
		return nil, notSent(errors.New("broker session request limit reached"))
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(h.ctx, cancel)
	defer stop()
	return h.dispatch(ctx, op, args)
}

// Close ends this session's forwards and transport. It is idempotent.
func (h *Handler) Close() error {
	if h.rejection != nil {
		return nil
	}
	h.cancel()
	h.release()
	return nil
}

func (h *Handler) release() {
	h.once.Do(func() {
		h.stop()
		h.cancel()
		_ = h.client.Close()
		h.runtime.remove(h)
		close(h.done)
	})
	<-h.done
}
