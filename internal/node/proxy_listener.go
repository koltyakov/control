package node

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/store"
)

// Managed listener bounds. The handshake deadline bounds an unauthenticated
// CONNECT request, never an established tunnel: tunnels have no idle timeout.
const (
	maxManagedTunnels       = 256
	managedHandshakeTimeout = 10 * time.Second
	managedHeaderBytes      = 8 << 10
	managedDialTimeout      = 10 * time.Second
	// legacyProxyStateFile was written by older workers and is ignored.
	legacyProxyStateFile = "proxy-listeners.json"
)

type managedEndpoint struct {
	url     string
	addr    netip.AddrPort
	sources []netip.Prefix
}

// managedProxy owns Control's fixed-gateway CONNECT listeners. It accepts only
// CONNECT to the canonical gateway authority from configured private/VPN
// sources, so it is not a general proxy, and it never terminates gateway TLS.
// Listeners follow explicit durable worker settings; this type never touches
// external proxy software. Every socket and goroutine it
// creates is closed and joined by closeAll.
type managedProxy struct {
	target           string // Gateway authority host:port, the only CONNECT target.
	endpoints        map[string]managedEndpoint
	write            func(string, any) error
	listen           func(context.Context, string) (net.Listener, error)
	dial             func(context.Context, string, string) (net.Conn, error)
	changed          func()
	base             []model.ProxyEndpoint
	settings         *model.ProxySettings
	enabled          bool
	settingsPath     string
	strictInterface  bool
	storageUncertain bool

	mu                 sync.Mutex
	ctx                context.Context
	listeners          map[string]net.Listener
	failures           map[string]string
	desired            []string
	supported          bool   // The gateway confirmed managed proxy state support.
	gen                uint64 // Changes whenever listener state changes.
	active             int    // Admitted connections, from accept through tunnel close.
	peak               int
	capacityRejections uint64
	fenced             bool // Maintenance reservation: refuse new connections.
	closed             bool
	conns              map[net.Conn]struct{}
	wg                 sync.WaitGroup
}

func newManagedProxy(cfg Config) (*managedProxy, error) {
	endpoints := map[string]managedEndpoint{}
	for _, endpoint := range cfg.ProxyEndpoints {
		if !endpoint.Managed {
			continue
		}
		addr, err := model.ManagedListenAddress(endpoint.URL)
		if err != nil {
			return nil, err
		}
		sources, err := model.ManagedSources(endpoint.Sources)
		if err != nil {
			return nil, err
		}
		endpoints[endpoint.URL] = managedEndpoint{url: endpoint.URL, addr: addr, sources: sources}
	}
	target, err := gatewayAuthority(cfg.Gateway)
	if err != nil {
		if len(endpoints) == 0 {
			return nil, nil
		}
		return nil, err
	}
	m := &managedProxy{
		target: target, endpoints: endpoints,
		listeners: map[string]net.Listener{}, failures: map[string]string{}, conns: map[net.Conn]struct{}{},
		settingsPath: filepath.Join(cfg.DataDir, "proxy-settings.json"),
		supported:    true,
	}
	m.listen = func(ctx context.Context, address string) (net.Listener, error) {
		return (&net.ListenConfig{}).Listen(ctx, "tcp", address)
	}
	m.write = func(path string, value any) error { return store.Write(path, value) }
	m.dial = (&net.Dialer{Timeout: managedDialTimeout}).DialContext
	// Desired listeners derive from the profile and proxy-settings.json below.
	// Older workers also wrote proxy-listeners.json; it is no longer read, so
	// a corrupt legacy copy cannot block startup.
	// Explicit profile declarations are enabled independently of dependencies.
	// The first managed declaration becomes the owner-editable setting.
	for _, e := range cfg.ProxyEndpoints {
		if e.Managed && m.settings == nil {
			a, _ := model.ManagedListenAddress(e.URL)
			m.settings = &model.ProxySettings{Interface: e.Interface, Address: a.Addr().String(), Port: int(a.Port()), Sources: slices.Clone(e.Sources)}
			m.enabled = true
		} else {
			m.base = append(m.base, e)
		}
	}
	var savedSettings proxySavedSettings
	if err := store.Read(m.settingsPath, &savedSettings); err == nil {
		m.settings, m.enabled = savedSettings.Settings, savedSettings.Enabled
		m.strictInterface = true
		if m.enabled {
			if m.settings == nil {
				return nil, errors.New("enabled proxy settings have no endpoint")
			}
			s := m.settings
			if _, err := (model.ProxyConfigure{Enabled: true, Interface: s.Interface, Address: s.Address, Port: s.Port, Sources: s.Sources}).Endpoint(); err != nil {
				return nil, fmt.Errorf("invalid saved proxy settings: %w", err)
			}
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("read proxy settings: %w", err)
	}
	m.endpoints = map[string]managedEndpoint{}
	m.desired = nil
	if cfg.GatewayProxy != nil && len(m.advertisementLocked()) != 0 {
		return nil, errors.New("a proxy-routed node cannot advertise gateway proxy endpoints")
	}
	for _, e := range m.advertisementLocked() {
		if !e.Managed {
			continue
		}
		a, err := model.ManagedListenAddress(e.URL)
		if err != nil {
			return nil, err
		}
		sources, err := model.ManagedSources(e.Sources)
		if err != nil {
			return nil, err
		}
		m.endpoints[e.URL] = managedEndpoint{url: e.URL, addr: a, sources: sources}
		m.desired = append(m.desired, e.URL)
	}
	return m, nil
}

// gatewayAuthority returns the lower-case host:port of an HTTPS gateway.
func gatewayAuthority(gateway string) (string, error) {
	u, err := url.Parse(gateway)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" {
		return "", errors.New("a managed proxy requires an https:// gateway")
	}
	port := u.Port()
	if port == "" {
		port = "443"
	}
	return net.JoinHostPort(strings.TrimSuffix(strings.ToLower(u.Hostname()), "."), port), nil
}

// start restores the last durable desired state so a restarted worker keeps
// serving children before it reaches the gateway again.
func (m *managedProxy) start(ctx context.Context) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.ctx = ctx
	desired := m.desired
	m.mu.Unlock()
	_ = m.apply(desired)
}

// apply starts listeners for desired managed endpoints and stops accepting on
// the rest. Stopping closes only the listening socket; admitted tunnels run
// until their peers close them or the node shuts down. Desired endpoints come
// from durable profile and owner settings, so apply itself persists nothing.
func (m *managedProxy) apply(desired []string) error {
	if m == nil {
		return nil
	}
	desired = slices.Clone(desired)
	slices.Sort(desired)
	desired = slices.Compact(desired)
	m.mu.Lock()
	if m.closed || m.ctx == nil {
		m.desired = desired
		m.mu.Unlock()
		return nil
	}
	changed := !slices.Equal(m.desired, desired)
	m.desired = desired
	want := map[string]bool{}
	for _, u := range desired {
		want[u] = true
	}
	for u, ln := range m.listeners {
		if !want[u] {
			_ = ln.Close()
			delete(m.listeners, u)
			changed = true
		}
	}
	for u := range m.failures {
		if !want[u] {
			delete(m.failures, u)
		}
	}
	changed = m.startMissingLocked() || changed
	if changed {
		m.gen++
	}
	notify := m.changed
	m.mu.Unlock()
	if changed && notify != nil {
		notify()
	}
	return nil
}

// retryListeners restores already enabled listeners after a transient bind or
// interface failure. It neither writes settings nor replays management.
func (m *managedProxy) retryListeners() {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed || m.ctx == nil || m.fenced || m.storageUncertain {
		return
	}
	if m.startMissingLocked() {
		m.gen++
	}
}

// startMissingLocked is bounded by the configured endpoint count. The caller
// owns mu, so a retry cannot restore a stale desired set during configuration.
func (m *managedProxy) startMissingLocked() bool {
	changed := false
	for _, u := range m.desired {
		endpoint, ok := m.endpoints[u]
		if !ok || m.listeners[u] != nil {
			continue
		}
		if m.settingsPath != "" {
			interfaces, err := nativeProxyInterfaces()
			if err != nil || !slices.ContainsFunc(interfaces, func(nic model.ProxyInterface) bool {
				if nic.Address != endpoint.addr.Addr().String() {
					return false
				}
				return !m.strictInterface || m.settings == nil || m.settings.Address != nic.Address || m.settings.Interface == nic.Name
			}) {
				m.failures[u] = "proxy address is not assigned to a live private interface"
				changed = true
				continue
			}
		}
		ln, err := m.listen(m.ctx, endpoint.addr.String())
		if err != nil {
			m.failures[u] = "managed listener could not bind its address"
			slog.Warn("start managed proxy listener", "url", u, "error", err)
			changed = true
			continue
		}
		delete(m.failures, u)
		m.listeners[u] = ln
		changed = true
		m.wg.Add(1)
		go m.accept(ln, endpoint)
	}
	return changed
}

func (m *managedProxy) accept(ln net.Listener, endpoint managedEndpoint) {
	defer m.wg.Done()
	for {
		conn, err := ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			// Back off briefly on transient accept errors such as EMFILE.
			m.mu.Lock()
			ctx := m.ctx
			m.mu.Unlock()
			timer := time.NewTimer(50 * time.Millisecond)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
			continue
		}
		if !m.admit(conn, endpoint) {
			_ = conn.Close()
		}
	}
}

// admit checks the source and reserves a connection slot atomically with the
// maintenance fence. Pending handshakes count as active.
func (m *managedProxy) admit(conn net.Conn, endpoint managedEndpoint) bool {
	remote, ok := conn.RemoteAddr().(*net.TCPAddr)
	if !ok {
		return false
	}
	source := remote.AddrPort().Addr().Unmap()
	m.mu.Lock()
	defer m.mu.Unlock()
	// A same-address settings change replaces the allowlist without replacing
	// the listening socket or leaving an old accept loop with stale rules.
	current, exists := m.endpoints[endpoint.url]
	if !exists {
		return false
	}
	endpoint = current
	allowed := source == endpoint.addr.Addr() // The worker's own readiness check.
	for _, prefix := range endpoint.sources {
		allowed = allowed || prefix.Contains(source)
	}
	if !allowed {
		return false
	}
	if m.closed || m.fenced {
		return false
	}
	if m.active >= maxManagedTunnels {
		m.capacityRejections++
		return false
	}
	m.active++
	m.peak = max(m.peak, m.active)
	m.conns[conn] = struct{}{}
	m.wg.Add(1)
	go m.serve(conn)
	return true
}

type managedProxyCapacity struct {
	Active     int    `json:"activeConnections"`
	Peak       int    `json:"peakConnections"`
	Limit      int    `json:"connectionLimit"`
	Rejections uint64 `json:"capacityRejections"`
}

func (m *managedProxy) capacity() (managedProxyCapacity, bool) {
	if m == nil {
		return managedProxyCapacity{}, false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return managedProxyCapacity{Active: m.active, Peak: m.peak, Limit: maxManagedTunnels, Rejections: m.capacityRejections}, len(m.endpoints) != 0
}

func (m *managedProxy) track(conn net.Conn) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return false
	}
	m.conns[conn] = struct{}{}
	return true
}

func (m *managedProxy) untrack(conn net.Conn, admitted bool) {
	m.mu.Lock()
	delete(m.conns, conn)
	if admitted {
		m.active--
	}
	m.mu.Unlock()
	_ = conn.Close()
}

func (m *managedProxy) serve(conn net.Conn) {
	defer m.wg.Done()
	defer m.untrack(conn, true)
	_ = conn.SetDeadline(time.Now().Add(managedHandshakeTimeout))
	limited := &io.LimitedReader{R: conn, N: managedHeaderBytes}
	reader := bufio.NewReaderSize(limited, 4096)
	req, err := http.ReadRequest(reader)
	if err != nil {
		writeProxyStatus(conn, http.StatusBadRequest)
		return
	}
	_ = req.Body.Close()
	if req.Method != http.MethodConnect {
		writeProxyStatus(conn, http.StatusMethodNotAllowed)
		return
	}
	if !m.allowedTarget(req.RequestURI) {
		writeProxyStatus(conn, http.StatusForbidden)
		return
	}
	m.mu.Lock()
	ctx := m.ctx
	m.mu.Unlock()
	dialCtx, cancel := context.WithTimeout(ctx, managedDialTimeout)
	upstream, err := m.dial(dialCtx, "tcp", m.target)
	cancel()
	if err != nil {
		writeProxyStatus(conn, http.StatusBadGateway)
		return
	}
	if !m.track(upstream) {
		_ = upstream.Close()
		return
	}
	defer m.untrack(upstream, false)
	if _, err = io.WriteString(conn, "HTTP/1.1 200 Connection established\r\n\r\n"); err != nil {
		return
	}
	_ = conn.SetDeadline(time.Time{})
	// Forward bytes the client sent after its request, such as a TLS hello.
	if buffered := reader.Buffered(); buffered > 0 {
		data, _ := reader.Peek(buffered)
		if _, err = upstream.Write(data); err != nil {
			return
		}
	}
	m.pipe(conn, upstream)
}

// pipe copies both directions. A clean end of stream half-closes the other
// side; an error closes both so neither copy can block forever.
func (m *managedProxy) pipe(client, upstream net.Conn) {
	done := make(chan struct{})
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		defer close(done)
		copyHalf(upstream, client)
	}()
	copyHalf(client, upstream)
	<-done
}

func copyHalf(dst, src net.Conn) {
	if _, err := io.Copy(dst, src); err != nil {
		_ = dst.Close()
		_ = src.Close()
		return
	}
	if closer, ok := dst.(interface{ CloseWrite() error }); ok {
		_ = closer.CloseWrite()
		return
	}
	_ = dst.Close()
}

// allowedTarget accepts only the canonical gateway authority. Other hosts,
// ports, IP spellings, and absolute-form URIs are refused.
func (m *managedProxy) allowedTarget(authority string) bool {
	host, port, err := net.SplitHostPort(authority)
	if err != nil || host == "" || port == "" {
		return false
	}
	return net.JoinHostPort(strings.TrimSuffix(strings.ToLower(host), "."), port) == m.target
}

func writeProxyStatus(conn net.Conn, status int) {
	_, _ = io.WriteString(conn, "HTTP/1.1 "+strconv.Itoa(status)+" "+http.StatusText(status)+"\r\nConnection: close\r\nContent-Length: 0\r\n\r\n")
}

// closeAll stops accepting, closes owned sockets, and joins every goroutine.
func (m *managedProxy) closeAll() {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.closed = true
	for u, ln := range m.listeners {
		_ = ln.Close()
		delete(m.listeners, u)
	}
	for conn := range m.conns {
		_ = conn.Close()
	}
	m.mu.Unlock()
	m.wg.Wait()
}

// busy reports admitted connections, including pending CONNECT handshakes.
func (m *managedProxy) busy() bool {
	if m == nil {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.active > 0
}

// fenceIfIdle atomically refuses new connections when none are admitted.
// Updates never force-close an active tunnel.
func (m *managedProxy) fenceIfIdle() bool {
	if m == nil {
		return true
	}
	m.mu.Lock()
	if m.active > 0 {
		m.mu.Unlock()
		return false
	}
	changed := !m.fenced
	m.fenced = true
	if changed {
		m.gen++
	}
	notify := m.changed
	m.mu.Unlock()
	if changed && notify != nil {
		notify()
	}
	return true
}

func (m *managedProxy) unfence() {
	if m == nil {
		return
	}
	m.mu.Lock()
	changed := m.fenced
	m.fenced = false
	if changed {
		m.gen++
	}
	notify := m.changed
	m.mu.Unlock()
	if changed && notify != nil {
		notify()
	}
}

// setSupported records whether the gateway serves managed proxy state and
// reports whether that changed.
func (m *managedProxy) setSupported(supported bool) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.supported == supported {
		return false
	}
	m.supported = supported
	m.gen++
	return true
}

func (m *managedProxy) generation() uint64 {
	if m == nil {
		return 0
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.gen
}

// endpointState describes a managed endpoint for readiness checks.
func (m *managedProxy) endpointState(u string) (supported, listening bool, failure string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fenced {
		return m.supported, false, "managed listener is paused for maintenance"
	}
	return m.supported, m.listeners[u] != nil, m.failures[u]
}

// advertisedProxyEndpoints removes local-only source allowlists from the
// signed registration.
func advertisedProxyEndpoints(endpoints []model.ProxyEndpoint) []model.ProxyEndpoint {
	if len(endpoints) == 0 {
		return nil
	}
	out := make([]model.ProxyEndpoint, len(endpoints))
	for i, endpoint := range endpoints {
		endpoint.Sources = nil
		out[i] = endpoint
	}
	return out
}
