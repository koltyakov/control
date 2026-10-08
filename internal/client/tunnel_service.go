package client

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gofrs/flock"
	"github.com/koltyakov/control/internal/identity"
	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/store"
)

type retainedTunnel struct {
	info    PersistentTunnelInfo // Protected by tunnelService.mu.
	forward *Forward
	ctx     context.Context
	cancel  context.CancelFunc
	done    chan struct{}
}

type tunnelService struct {
	ctx     context.Context
	cancel  context.CancelFunc
	dir     string
	cfg     StandaloneConfig
	mu      sync.Mutex
	entries map[string]*retainedTunnel
	wg      sync.WaitGroup
}

// RunTunnelService owns the retained listeners independently of CLI/MCP processes.
// It must run under a user service manager for host-login and crash recovery.
func RunTunnelService(ctx context.Context, dir string) error {
	dir, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	lock := flock.New(filepath.Join(dir, "service.lock"))
	defer func() { _ = lock.Close() }()
	held, err := lock.TryLock()
	if err != nil {
		return err
	}
	if !held {
		return errors.New("tunnel service is already running")
	}
	endpointPath := filepath.Join(dir, "endpoint.json")
	// Keep the endpoint until owned forwards have finished shutting down.
	defer func() { _ = os.Remove(endpointPath) }()
	var cfg StandaloneConfig
	if err = store.Read(filepath.Join(dir, "service.json"), &cfg); err != nil {
		return err
	}
	if !cfg.AccountRouting || cfg.Gateway.URL == "" || cfg.Gateway.Key == "" || cfg.StateDir == "" || filepath.Base(dir) != tunnelScope(cfg) {
		return errors.New("invalid tunnel service profile")
	}
	m, err := newTunnelService(ctx, dir, cfg)
	if err != nil {
		return err
	}
	defer m.close()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	defer func() { _ = listener.Close() }()
	endpoint := tunnelEndpoint{Address: listener.Addr().String(), Token: identity.NewID() + identity.NewID(), PID: os.Getpid()}
	if err = store.Write(endpointPath, endpoint); err != nil {
		return err
	}
	server := &http.Server{Handler: m.handler(endpoint.Token), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 20 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 16 << 10,
		BaseContext: func(net.Listener) context.Context { return m.ctx }}
	stop := context.AfterFunc(ctx, func() { _ = server.Close() })
	defer stop()
	err = server.Serve(listener)
	_ = server.Close()
	if ctx.Err() != nil || errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func newTunnelService(ctx context.Context, dir string, cfg StandaloneConfig) (*tunnelService, error) {
	ctx, cancel := context.WithCancel(ctx)
	m := &tunnelService{ctx: ctx, cancel: cancel, dir: dir, cfg: cfg, entries: map[string]*retainedTunnel{}}
	var infos []PersistentTunnelInfo
	if err := store.Read(filepath.Join(dir, "tunnels.json"), &infos); err != nil && !os.IsNotExist(err) {
		cancel()
		return nil, err
	}
	if len(infos) > maxForwards {
		cancel()
		return nil, errors.New("too many retained tunnels")
	}
	for _, info := range infos {
		if err := validatePersistentTunnel(info.PersistentTunnelSpec); err != nil || len(info.NodeID) != 64 || info.Created.IsZero() || m.entries[info.ID] != nil {
			cancel()
			return nil, errors.New("invalid retained tunnel state")
		}
		m.entries[info.ID] = m.entry(info)
	}
	for _, entry := range m.entries {
		m.start(entry)
	}
	m.wg.Add(1)
	go m.expire()
	return m, nil
}

func (m *tunnelService) entry(info PersistentTunnelInfo) *retainedTunnel {
	ctx, cancel := context.WithCancel(m.ctx)
	if !info.Expires.IsZero() {
		cancel()
		ctx, cancel = context.WithDeadline(m.ctx, info.Expires)
	}
	info.State, info.LastError, info.Active = "starting", "", 0
	return &retainedTunnel{info: info, ctx: ctx, cancel: cancel, done: make(chan struct{})}
}

func (m *tunnelService) start(e *retainedTunnel) {
	m.wg.Add(1)
	go func() { defer m.wg.Done(); defer close(e.done); m.run(e) }()
}

func (m *tunnelService) close() {
	m.mu.Lock()
	m.cancel()
	m.mu.Unlock()
	m.wg.Wait()
}

// saveLocked commits desired state before acknowledging creation or disposal.
func (m *tunnelService) saveLocked() error {
	infos := make([]PersistentTunnelInfo, 0, len(m.entries))
	for _, e := range m.entries {
		info := e.info
		info.State, info.LastError, info.Active = "", "", 0
		infos = append(infos, info)
	}
	sort.Slice(infos, func(i, j int) bool { return infos[i].ID < infos[j].ID })
	return store.Write(filepath.Join(m.dir, "tunnels.json"), infos)
}

func (m *tunnelService) list() []PersistentTunnelInfo {
	m.mu.Lock()
	defer m.mu.Unlock()
	infos := []PersistentTunnelInfo{}
	for _, e := range m.entries {
		info := e.info
		if !info.Expires.IsZero() && !time.Now().Before(info.Expires) {
			continue
		}
		if e.forward != nil {
			f := e.forward.Info()
			info.Active, info.LastError = f.Active, f.LastError
		}
		infos = append(infos, info)
	}
	sort.Slice(infos, func(i, j int) bool { return infos[i].ID < infos[j].ID })
	return infos
}

func (m *tunnelService) create(ctx context.Context, spec PersistentTunnelSpec) (PersistentTunnelInfo, error) {
	if err := validatePersistentTunnel(spec); err != nil {
		return PersistentTunnelInfo{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.ctx.Err() != nil {
		return PersistentTunnelInfo{}, errors.New("tunnel service is stopping")
	}
	if e := m.entries[spec.ID]; e != nil {
		if e.info.PersistentTunnelSpec != spec {
			return PersistentTunnelInfo{}, errors.New("tunnel ID already has a different specification")
		}
		return e.info, nil
	}
	if len(m.entries) >= maxForwards {
		return PersistentTunnelInfo{}, errors.New("retained tunnel limit reached")
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var nodes []model.Node
	if err := m.cfg.Gateway.JSON(ctx, http.MethodGet, "/v1/nodes", nil, &nodes); err != nil {
		return PersistentTunnelInfo{}, err
	}
	var target model.Node
	for _, n := range nodes {
		if n.Name == spec.Node || n.ID == spec.Node {
			target = n
			break
		}
	}
	if target.ID == "" || !target.Online || target.Disabled || target.ControlPending {
		return PersistentTunnelInfo{}, errors.New("target machine is unknown, offline, disabled, or awaiting policy acknowledgement")
	}
	if !target.InstructionDelegation || !target.ClientOwners || !target.ClientSessions {
		return PersistentTunnelInfo{}, errors.New("target needs an upgrade for account-client tunnels")
	}
	for _, e := range m.entries {
		if e.info.Listen == spec.Listen && e.info.Reverse == spec.Reverse && (!spec.Reverse || e.info.NodeID == target.ID) {
			return PersistentTunnelInfo{}, errors.New("another retained tunnel already uses that listener")
		}
	}
	info := PersistentTunnelInfo{PersistentTunnelSpec: spec, NodeID: target.ID, Created: time.Now().UTC()}
	if spec.TTLSeconds > 0 {
		info.Expires = info.Created.Add(time.Duration(spec.TTLSeconds) * time.Second)
	}
	e := m.entry(info)
	m.entries[spec.ID] = e
	if err := m.saveLocked(); err != nil {
		delete(m.entries, spec.ID)
		e.cancel()
		return PersistentTunnelInfo{}, err
	}
	m.start(e)
	return e.info, nil
}

func (m *tunnelService) dispose(id string) error {
	return m.disposeEntry(id, nil)
}

func (m *tunnelService) disposeEntry(id string, expected *retainedTunnel) error {
	m.mu.Lock()
	e := m.entries[id]
	if e == nil || (expected != nil && e != expected) {
		m.mu.Unlock()
		return nil
	}
	delete(m.entries, id)
	if err := m.saveLocked(); err != nil {
		m.entries[id] = e
		m.mu.Unlock()
		return err
	}
	e.cancel()
	m.mu.Unlock()
	<-e.done
	return nil
}

func (m *tunnelService) expire() {
	defer m.wg.Done()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-m.ctx.Done():
			return
		case now := <-ticker.C:
			m.mu.Lock()
			expired := map[string]*retainedTunnel{}
			for id, e := range m.entries {
				if !e.info.Expires.IsZero() && !now.Before(e.info.Expires) {
					expired[id] = e
				}
			}
			m.mu.Unlock()
			for id, entry := range expired {
				// A concurrently disposed/recreated ID must not expire its replacement.
				_ = m.disposeEntry(id, entry) // Failed writes retry; the deadline already closed the listener.
			}
		}
	}
}

func (m *tunnelService) run(e *retainedTunnel) {
	delay := time.Second
	for e.ctx.Err() == nil {
		// Each attempt gets a fresh client so failed initialization is not cached.
		// The target is an immutable ID, never a replacement using the same name.
		c := (Client{}).WithStandalone(e.ctx, m.cfg)
		setup, cancel := context.WithTimeout(e.ctx, 15*time.Second)
		var description struct{ ID string }
		err := c.Call(setup, e.info.NodeID, "node.describe", map[string]any{}, &description)
		var forward *Forward
		if err == nil && description.ID != e.info.NodeID {
			err = errors.New("tunnel target identity changed")
		}
		if err == nil {
			spec := e.info.ForwardSpec
			spec.Node = e.info.NodeID
			forward, err = c.StartForward(setup, spec)
		}
		cancel()
		if err == nil {
			m.mu.Lock()
			e.forward, e.info.State, e.info.LastError = forward, "active", ""
			m.mu.Unlock()
			select {
			case <-e.ctx.Done():
			case <-forward.Done():
			}
			forward.Close()
			err = errors.New("listener disconnected; restoring for new connections")
			delay = time.Second
		}
		_ = c.Close()
		m.mu.Lock()
		e.forward, e.info.State = nil, "retrying"
		if err != nil {
			message := err.Error()
			if len(message) > 512 {
				message = message[:512]
			}
			e.info.LastError = message
		}
		m.mu.Unlock()
		timer := time.NewTimer(delay)
		select {
		case <-e.ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		delay = min(30*time.Second, delay*2)
	}
}

func (m *tunnelService) handler(token string) http.Handler {
	slots := make(chan struct{}, 32)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+token)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		select {
		case slots <- struct{}{}:
			defer func() { <-slots }()
		default:
			http.Error(w, "tunnel management request limit reached", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		var result any
		var err error
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/health":
			result = map[string]bool{"ready": true}
		case r.Method == http.MethodGet && r.URL.Path == "/tunnels":
			result = m.list()
		case r.Method == http.MethodPost && r.URL.Path == "/tunnels":
			var spec PersistentTunnelSpec
			d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10))
			d.DisallowUnknownFields()
			if err = d.Decode(&spec); err == nil {
				result, err = m.create(r.Context(), spec)
			}
		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/tunnels/"):
			err = m.dispose(strings.TrimPrefix(r.URL.Path, "/tunnels/"))
			result = map[string]bool{"disposed": err == nil}
		default:
			http.NotFound(w, r)
			return
		}
		if err != nil {
			http.Error(w, fmt.Sprint(err), http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(result)
	})
}
