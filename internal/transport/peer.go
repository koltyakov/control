// Package transport provides mutually authenticated, multiplexed peer sessions
// over WebRTC data channels or the gateway's WebSocket relay.
package transport

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/koltyakov/control/internal/buildinfo"
	"github.com/koltyakov/control/internal/gatewayroute"
	"github.com/koltyakov/control/internal/identity"
	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/protocol"
	"github.com/pion/webrtc/v4"
)

type Config struct {
	Client            bool
	Gateway           string
	Token             string
	Identity          *identity.Identity
	ClientOwner       *identity.Identity
	Node              model.Node
	RelayOnly         bool
	ICEServers        []webrtc.ICEServer
	DirectTimeout     time.Duration
	RequireDelegation bool
	// Route carries every gateway request: authentication, WebSocket,
	// directory, policy, and placement reads. The zero value is direct.
	Route gatewayroute.Route
	// NameHints optionally remembers routing-name identities across short-lived
	// clients so a name lookup can read one gateway record, not the directory.
	NameHints NameHints
}

// NameHints suggests the stable identity last seen for a routing name. A hint
// is never authority: lookups accept it only when the gateway's current record
// for that identity still carries the requested name.
type NameHints interface {
	Hint(name string) string
	Remember(nodes []model.Node)
}

type link struct {
	ice        *linkICE
	generation uint64
	id         string
	remote     string
	mode       string
	conn       *packetConn
	pc         *webrtc.PeerConnection
	carrier    *rtcCarrier
	answer     chan webrtc.SessionDescription
	ready      chan struct{}
	readyOnce  sync.Once
	dcOnce     sync.Once
	dcMu       sync.Mutex
	dc         *webrtc.DataChannel
}

type Peer struct {
	userID           string
	members          map[string]bool
	memberSeen       map[string]time.Time
	owners           map[string]string
	channels         map[string]bool
	largePackets     map[string]bool
	trickle          map[string]bool
	iceServers       []webrtc.ICEServer
	directRetry      map[string]time.Time
	lookups          map[string]lookupEntry
	carriers         map[string]*rtcCarrier
	api              *webrtc.API
	trickleAPI       *webrtc.API
	stats            transportStats
	cfg              Config
	ctx              context.Context
	cancel           context.CancelFunc
	mu               sync.Mutex
	ws               *websocket.Conn
	gatewayWS        atomic.Pointer[websocket.Conn] // send-path mirror of ws, read without mu
	connectCancel    context.CancelFunc
	writes           chan *gatewayWrite
	priorityWrites   chan *gatewayWrite
	reconnectWake    chan struct{}
	gatewayConnected chan struct{}
	links            map[string]*link
	// linkIndex mirrors links, under mu for writes, so relay data packets
	// find their link without taking mu.
	linkIndex sync.Map
	// lookupFlights and directoryFlight coalesce concurrent lookup misses.
	lookupFlights       map[string]*flight[model.Node]
	directoryFlight     *flight[[]model.Node]
	sessions            map[sessionKey]*peerSession
	allSessions         map[string]*peerSession
	pending             map[sessionKey]*sessionDial
	pendingSlots        map[Lane]chan struct{}
	incomingSetups      map[string]*incomingSetup
	resolved            map[string]string
	directoryGeneration uint64
	networkGeneration   uint64
	// setups bounds concurrent incoming setups; dialSetups bounds outgoing
	// TLS/ICE setups separately, so a slow fan-out cannot refuse incoming work.
	setups     chan struct{}
	dialSetups chan struct{}
	// closers tracks asynchronous pion teardowns; see closeAsync.
	closersMu      sync.Mutex
	closersWaiting bool
	closers        sync.WaitGroup
	// rejects bounds concurrent close replies to refused setups.
	rejects                  chan struct{}
	streamSlots              chan struct{}
	interactiveIncomingSlots chan struct{}
	interactiveWaiters       chan struct{}
	outgoingSlots            map[Lane]chan struct{}
	closed                   bool
	relayDrainUntil          time.Time
	relayDrainID             string
	relayPeerDrains          map[string]peerDrain
	closeOnce                sync.Once
	handler                  func(string, net.Conn)
	log                      *slog.Logger
	wg                       sync.WaitGroup
	control                  func(string, []byte)
	wake                     func([]byte)
	startup                  func([]byte)
	secrets                  func([]byte)
	proxyManagement          func([]byte)
	proxySlots               chan struct{}
	// controlSlots bound concurrent wake and startup handlers.
	controlSlots      chan struct{}
	secretSlots       chan struct{}
	nodeHealth        bool
	telemetryLiveness atomic.Bool
	// gatewayHeartbeat records that the current gateway sends periodic
	// challenges, so prolonged silence means a dead path.
	gatewayHeartbeat atomic.Bool
	// readIdle overrides gatewayReadIdle in tests.
	readIdle         time.Duration
	gatewaySession   atomic.Uint64
	clientSessions   bool
	machineStatePush atomic.Bool
	machineStateWake chan struct{}
	networkSnapshot  func() (string, error)
	wakeSnapshot     func() ([]model.WakeInterface, error)
	lanDiscovery     bool
	lanViewSession   string
	lanSlots         chan struct{}
	lanInbound       map[string]time.Time
}

func New(cfg Config, handler func(string, net.Conn), options ...Options) *Peer {
	if cfg.DirectTimeout == 0 {
		cfg.DirectTimeout = 5 * time.Second
	}
	p := &Peer{
		cfg: cfg, handler: handler, log: slog.Default(),
		writes: make(chan *gatewayWrite, 64), priorityWrites: make(chan *gatewayWrite, 32),
		reconnectWake: make(chan struct{}, 1), gatewayConnected: make(chan struct{}, 1), machineStateWake: make(chan struct{}, 1),
		lanSlots: make(chan struct{}, 4), lanInbound: map[string]time.Time{},
		secretSlots: make(chan struct{}, 1),
		proxySlots:  make(chan struct{}, 1), controlSlots: make(chan struct{}, 8),
		links: map[string]*link{}, sessions: map[sessionKey]*peerSession{},
		allSessions: map[string]*peerSession{}, pending: map[sessionKey]*sessionDial{},
		incomingSetups: map[string]*incomingSetup{}, resolved: map[string]string{},
		members: map[string]bool{}, memberSeen: map[string]time.Time{}, owners: map[string]string{}, channels: map[string]bool{},
		largePackets: map[string]bool{}, trickle: map[string]bool{}, directRetry: map[string]time.Time{}, lookups: map[string]lookupEntry{},
		carriers: map[string]*rtcCarrier{}, setups: make(chan struct{}, 32), dialSetups: make(chan struct{}, 32), rejects: make(chan struct{}, 8), streamSlots: make(chan struct{}, 512),
		interactiveIncomingSlots: make(chan struct{}, incomingInteractive), interactiveWaiters: make(chan struct{}, interactiveWaiters),
		pendingSlots: map[Lane]chan struct{}{
			ControlLane: make(chan struct{}, 32), BulkLane: make(chan struct{}, 16), InteractiveLane: make(chan struct{}, 16),
		},
		outgoingSlots: map[Lane]chan struct{}{
			ControlLane: make(chan struct{}, 256), BulkLane: make(chan struct{}, 64), InteractiveLane: make(chan struct{}, outgoingInteractive),
		},
	}
	p.api = newWebRTCAPI(cfg.DirectTimeout, false, &p.stats.udpBuffers, options...)
	p.trickleAPI = newWebRTCAPI(cfg.DirectTimeout, true, &p.stats.udpBuffers, options...)
	return p
}

// SetCapabilities updates registration metadata before Start.
func (p *Peer) SetCapabilities(capabilities []model.Capability) {
	p.cfg.Node.Capabilities = capabilities
}

// SetSystemInfo attaches an initial hardware/resource snapshot before Start.
func (p *Peer) SetSystemInfo(info model.SystemInfo) { p.cfg.Node.System = &info }

func (p *Peer) SetSoftware(info buildinfo.Info) {
	// Registration is signed after JSON encoding. Keep its software fields
	// compatible with gateways predating extended build metadata so a rolling
	// update can reconnect nodes before the gateway itself restarts.
	info.ReleaseRepo, info.BuildTime = "", ""
	p.cfg.Node.Software = info
}

func (p *Peer) SetControlHandler(handler func(string, []byte)) { p.control = handler }

// GatewayConnected signals successful registration and reconnections. Unread
// signals coalesce; receiving one does not prove ongoing connection health.
func (p *Peer) GatewayConnected() <-chan struct{} { return p.gatewayConnected }

// SetWakeHandler receives wake requests the gateway relays for the fleet owner.
func (p *Peer) SetWakeHandler(handler func([]byte)) { p.wake = handler }

// SetStartupHandler is configured before Start by an installed node only.
func (p *Peer) SetStartupHandler(handler func([]byte)) {
	p.startup = handler
	p.cfg.Node.GatewayStartup = handler != nil
}

// SetSecretsHandler installs a bounded, gateway-only management handler before Start.
func (p *Peer) SetSecretsHandler(key []byte, handler func([]byte)) {
	p.secrets = handler
	p.cfg.Node.SecretManagementKey = key
	p.cfg.Node.SecretPolicy = handler != nil && len(key) != 0
}

// SetProxyManagementHandler installs the fixed owner-management handler.
func (p *Peer) SetProxyManagementHandler(handler func([]byte)) {
	p.proxyManagement = handler
	p.cfg.Node.GatewayProxyManagement = handler != nil
}

// SetProxyEndpoints updates future registration without reconnecting peers.
func (p *Peer) SetProxyEndpoints(endpoints []model.ProxyEndpoint) {
	p.mu.Lock()
	p.cfg.Node.ProxyEndpoints = endpoints
	p.mu.Unlock()
}

func (p *Peer) SendControl(ctx context.Context, kind string, data []byte) error {
	return p.send(ctx, &protocol.Packet{Kind: kind, Data: data})
}

// TelemetryLiveness reports whether the current gateway keeps unchanged
// health and update status fresh through heartbeat echoes.
func (p *Peer) TelemetryLiveness() bool { return p.telemetryLiveness.Load() }

// GatewaySession changes after each successful gateway connection. Telemetry
// that suppresses unchanged reports must send them again to a new connection.
func (p *Peer) GatewaySession() uint64 { return p.gatewaySession.Load() }

// SupportsNodeHealth prevents new reports from disconnecting older gateways.
func (p *Peer) SupportsNodeHealth() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.nodeHealth
}

func (p *Peer) Start(ctx context.Context) error {
	p.mu.Lock()
	if p.closed || p.ctx != nil {
		p.mu.Unlock()
		return errors.New("peer already started or closed")
	}
	p.ctx, p.cancel = context.WithCancel(ctx)
	p.wg.Add(1)
	p.mu.Unlock()
	defer p.wg.Done()
	var ws *websocket.Conn
	connect := func(ctx context.Context) (err error) {
		ws, err = p.connect(ctx)
		return err
	}
	var err error
	if p.cfg.Client {
		// Command-scoped clients retain their fail-fast startup behavior.
		err = connect(p.ctx)
	} else {
		err = waitForGateway(p.ctx, p.log, "gateway connection", connect)
	}
	if err != nil {
		if ws != nil {
			_ = ws.CloseNow()
		}
		p.cancel()
		return err
	}
	if !p.worker(p.writeGateway) || !p.worker(func() { p.run(ws) }) || !p.worker(p.watchNetwork) || (!p.cfg.Client && !p.worker(p.watchLAN)) {
		_ = ws.CloseNow()
		return errors.New("peer stopped during startup")
	}
	return nil
}

func (p *Peer) Connections() map[string]string {
	p.mu.Lock()
	defer p.mu.Unlock()
	result := map[string]string{}
	for _, s := range p.allSessions {
		if s.usableLocked() && result[s.remote] != "webrtc" {
			result[s.remote] = s.mode
		}
	}
	return result
}

func (p *Peer) Close() error {
	p.closeOnce.Do(func() {
		p.mu.Lock()
		p.closed = true
		if p.cancel != nil {
			p.cancel()
		}
		ws := p.ws
		var links []*link
		for _, l := range p.links {
			links = append(links, l)
		}
		var sessions []*peerSession
		for _, s := range p.allSessions {
			sessions = append(sessions, s)
		}
		p.mu.Unlock()
		if ws != nil {
			_ = ws.CloseNow()
		}
		for _, l := range links {
			_ = l.conn.Close()
		}
		for _, s := range sessions {
			_ = s.mux.Close()
		}
		p.wg.Wait()
		p.closersMu.Lock()
		p.closersWaiting = true
		p.closersMu.Unlock()
		p.closers.Wait()
		p.drainWrites()
	})
	return nil
}

// closeAsync runs a pion teardown off the caller's goroutine, which may hold
// locks or run inside a pion callback. Close waits for teardowns started
// before it, so returning from Close means data channels and peer connections
// of closed links are torn down.
func (p *Peer) closeAsync(fn func()) {
	p.closersMu.Lock()
	defer p.closersMu.Unlock()
	if p.closersWaiting {
		go fn()
		return
	}
	p.closers.Go(fn)
}
