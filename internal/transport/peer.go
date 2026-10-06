// Package transport provides mutually authenticated, multiplexed peer sessions
// over WebRTC data channels or the gateway's WebSocket relay.
package transport

import (
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/hashicorp/yamux"
	"github.com/koltyakov/control/internal/buildinfo"
	"github.com/koltyakov/control/internal/gateway"
	"github.com/koltyakov/control/internal/identity"
	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/protocol"
	"github.com/koltyakov/control/internal/update"
	"github.com/pion/webrtc/v4"
	"google.golang.org/protobuf/proto"
)

type Config struct {
	Client        bool
	Gateway       string
	Token         string
	Identity      *identity.Identity
	ClientOwner   *identity.Identity
	Node          model.Node
	RelayOnly     bool
	ICEServers    []webrtc.ICEServer
	DirectTimeout time.Duration
}

type link struct {
	remote    string
	mode      string
	conn      *packetConn
	pc        *webrtc.PeerConnection
	carrier   *rtcCarrier
	answer    chan webrtc.SessionDescription
	ready     chan struct{}
	readyOnce sync.Once
	dcOnce    sync.Once
	dcMu      sync.Mutex
	dc        *webrtc.DataChannel
}

type Peer struct {
	userID         string
	members        map[string]bool
	owners         map[string]string
	channels       map[string]bool
	carriers       map[string]*rtcCarrier
	cfg            Config
	ctx            context.Context
	cancel         context.CancelFunc
	mu             sync.Mutex
	ws             *websocket.Conn
	writes         chan *gatewayWrite
	links          map[string]*link
	sessions       map[sessionKey]*peerSession
	allSessions    map[string]*peerSession
	pending        map[sessionKey]*sessionDial
	pendingSlots   map[Lane]chan struct{}
	incomingSetups map[string]*incomingSetup
	resolved       map[string]string
	setups         chan struct{}
	streamSlots    chan struct{}
	outgoingSlots  map[Lane]chan struct{}
	closed         bool
	closeOnce      sync.Once
	handler        func(string, net.Conn)
	log            *slog.Logger
	wg             sync.WaitGroup
	control        func(string, []byte)
	nodeHealth     bool
	clientSessions bool
}

func New(cfg Config, handler func(string, net.Conn)) *Peer {
	if cfg.DirectTimeout == 0 {
		cfg.DirectTimeout = 5 * time.Second
	}
	return &Peer{
		cfg: cfg, handler: handler, log: slog.Default(),
		writes: make(chan *gatewayWrite, 64),
		links:  map[string]*link{}, sessions: map[sessionKey]*peerSession{},
		allSessions: map[string]*peerSession{}, pending: map[sessionKey]*sessionDial{},
		incomingSetups: map[string]*incomingSetup{}, resolved: map[string]string{},
		members: map[string]bool{}, owners: map[string]string{}, channels: map[string]bool{},
		carriers: map[string]*rtcCarrier{}, setups: make(chan struct{}, 32), streamSlots: make(chan struct{}, 512),
		pendingSlots: map[Lane]chan struct{}{
			ControlLane: make(chan struct{}, 32), BulkLane: make(chan struct{}, 16), InteractiveLane: make(chan struct{}, 16),
		},
		outgoingSlots: map[Lane]chan struct{}{
			ControlLane: make(chan struct{}, 256), BulkLane: make(chan struct{}, 64), InteractiveLane: make(chan struct{}, 192),
		},
	}
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
func (p *Peer) SendControl(ctx context.Context, kind string, data []byte) error {
	return p.send(ctx, &protocol.Packet{Kind: kind, Data: data})
}

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
	ws, err := p.connect(p.ctx)
	if err != nil {
		p.cancel()
		return err
	}
	if !p.worker(p.writeGateway) || !p.worker(func() { p.run(ws) }) {
		_ = ws.CloseNow()
		return errors.New("peer stopped during startup")
	}
	return nil
}

func (p *Peer) connect(ctx context.Context) (*websocket.Conn, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	// Fleet identity comes from the authenticated gateway, never from labels or
	// an incoming peer. Refuse old gateways that cannot attest membership.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, gateway.URL(p.cfg.Gateway, "/v1/auth"), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+p.cfg.Token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	var scope struct {
		UserID         string `json:"userId"`
		NodeHealth     bool   `json:"nodeHealth"`
		ClientSessions bool   `json:"clientSessions"`
		ClientOwners   bool   `json:"clientOwners"`
		PeerChannels   bool   `json:"peerChannels"`
	}
	err = json.NewDecoder(io.LimitReader(resp.Body, 8192)).Decode(&scope)
	_ = resp.Body.Close()
	if err != nil || resp.StatusCode != http.StatusOK || scope.UserID == "" {
		return nil, errors.New("gateway did not authenticate fleet membership; upgrade the gateway before this node")
	}
	if p.cfg.Client && !scope.ClientSessions {
		return nil, errors.New("gateway does not support client sessions; upgrade the gateway and target nodes before standalone CLI use")
	}
	if p.cfg.ClientOwner != nil && (!p.cfg.Client || !scope.ClientOwners) {
		return nil, errors.New("gateway does not support concurrent client owners; upgrade the gateway and target nodes")
	}
	p.mu.Lock()
	if p.userID != "" && p.userID != scope.UserID {
		p.mu.Unlock()
		return nil, errors.New("node fleet cannot change")
	}
	if p.closed || p.ctx.Err() != nil {
		p.mu.Unlock()
		return nil, errors.New("peer stopped during authentication")
	}
	p.userID = scope.UserID
	p.nodeHealth = scope.NodeHealth
	p.clientSessions = scope.ClientSessions
	p.cfg.Node.UserID = scope.UserID
	// Negotiate this optional signed field so new machines still connect to old
	// gateways whose registration decoder does not know about client sessions.
	p.cfg.Node.ClientSessions = scope.ClientSessions && !p.cfg.Client
	p.cfg.Node.ClientOwners = scope.ClientOwners && !p.cfg.Client
	p.cfg.Node.PeerChannels = scope.PeerChannels
	if p.cfg.ClientOwner != nil {
		p.cfg.Node.ClientOwner = p.cfg.ClientOwner.ID
	}
	p.mu.Unlock()
	u := gateway.URL(p.cfg.Gateway, "/v1/connect")
	if p.cfg.Client {
		u = gateway.URL(p.cfg.Gateway, "/v1/client/connect")
	}
	u = strings.Replace(strings.Replace(u, "https://", "wss://", 1), "http://", "ws://", 1)
	ws, _, err := websocket.Dial(ctx, u, &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer " + p.cfg.Token}}})
	if err != nil {
		return nil, fmt.Errorf("connect gateway: %w", err)
	}
	ws.SetReadLimit(1 << 20)
	_, challenge, err := ws.Read(ctx)
	if err == nil && len(challenge) != 32 {
		err = errors.New("invalid gateway challenge")
	}
	if err == nil {
		hello := gateway.Hello{Version: model.Version, Node: p.cfg.Node, Client: p.cfg.Client}
		hello.Signature = ed25519.Sign(p.cfg.Identity.Private, hello.Message(challenge))
		if p.cfg.ClientOwner != nil {
			hello.OwnerKey = p.cfg.ClientOwner.Public
			hello.OwnerSignature = ed25519.Sign(p.cfg.ClientOwner.Private, hello.OwnerMessage(challenge))
		}
		err = ws.Write(ctx, websocket.MessageBinary, model.JSON(hello))
	}
	if err == nil {
		var ready []byte
		_, ready, err = ws.Read(ctx)
		if err == nil && string(ready) != "ready" {
			err = errors.New("gateway rejected registration")
		}
	}
	if err != nil {
		_ = ws.CloseNow()
		return nil, err
	}
	p.mu.Lock()
	p.ws = ws
	if p.closed || p.ctx.Err() != nil {
		p.ws = nil
		p.mu.Unlock()
		_ = ws.CloseNow()
		return nil, errors.New("peer stopped during connection")
	}
	p.mu.Unlock()
	return ws, nil
}

func (p *Peer) run(ws *websocket.Conn) {
	for {
		for {
			_, b, err := ws.Read(p.ctx)
			if err != nil {
				break
			}
			var msg protocol.Packet
			if proto.Unmarshal(b, &msg) != nil {
				break
			}
			p.receive(&msg)
		}
		_ = ws.CloseNow()
		p.mu.Lock()
		if p.ws == ws {
			p.ws = nil
		}
		var broken []*packetConn
		for _, l := range p.links {
			if l.mode == "relay" {
				broken = append(broken, l.conn)
			}
		}
		p.mu.Unlock()
		for _, c := range broken {
			_ = c.Close()
		}
		for {
			select {
			case <-p.ctx.Done():
				return
			case <-time.After(time.Second):
			}
			var err error
			ws, err = p.connect(p.ctx)
			if err == nil {
				break
			}
			p.log.Debug("gateway reconnect", "error", err)
		}
	}
}

func (p *Peer) send(ctx context.Context, msg *protocol.Packet) error {
	p.mu.Lock()
	ws := p.ws
	closed := p.closed || p.ctx == nil || p.ctx.Err() != nil
	p.mu.Unlock()
	if ws == nil || closed {
		return errors.New("gateway disconnected")
	}
	b, err := proto.Marshal(msg)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	request := &gatewayWrite{ctx: ctx, ws: ws, data: b, done: make(chan error, 1)}
	select {
	case p.writes <- request:
	case <-ctx.Done():
		return ctx.Err()
	case <-p.ctx.Done():
		return p.ctx.Err()
	}
	select {
	case err := <-request.done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	case <-p.ctx.Done():
		return p.ctx.Err()
	}
}

func (p *Peer) receive(msg *protocol.Packet) {
	if strings.HasPrefix(msg.Kind, "update.") {
		if msg.From == update.GatewaySender && p.control != nil {
			p.control(msg.Kind, msg.Data)
		}
		return
	}
	if msg.Kind == "offline" {
		p.mu.Lock()
		var connections []*packetConn
		for _, l := range p.links {
			if l.remote == msg.From && l.mode == "relay" {
				connections = append(connections, l.conn)
			}
		}
		p.mu.Unlock()
		for _, c := range connections {
			go func() { _ = c.Close() }()
		}
		return
	}
	if msg.Kind == "open" {
		p.mu.Lock()
		_, exists := p.links[msg.Session]
		exists = exists || p.incomingSetups[msg.Session] != nil
		full := len(p.links) >= 256 || p.closed
		p.mu.Unlock()
		if exists || full {
			return
		}
		select {
		case p.setups <- struct{}{}:
			ctx, cancel := context.WithTimeout(p.ctx, 20*time.Second)
			setup := &incomingSetup{remote: msg.From, cancel: cancel}
			p.mu.Lock()
			p.incomingSetups[msg.Session] = setup
			p.mu.Unlock()
			cleanup := func() { cancel(); <-p.setups; p.mu.Lock(); delete(p.incomingSetups, msg.Session); p.mu.Unlock() }
			if !p.worker(func() { defer cleanup(); p.accept(ctx, msg) }) {
				cleanup()
			}
		default:
			// Drop excess setup without blocking the gateway reader on a write.
		}
		return
	}
	p.mu.Lock()
	l := p.links[msg.Session]
	setup := p.incomingSetups[msg.Session]
	p.mu.Unlock()
	if l == nil || l.remote != msg.From {
		if msg.Kind == "close" && setup != nil && setup.remote == msg.From {
			setup.cancel()
		}
		return
	}
	switch msg.Kind {
	case "data":
		if l.mode == "relay" && !l.conn.push(msg.Data) {
			go func() { _ = l.conn.Close() }()
		}
	case "answer":
		if l.mode == "relay" {
			l.readyOnce.Do(func() { close(l.ready) })
			return
		}
		var answer webrtc.SessionDescription
		if json.Unmarshal(msg.Data, &answer) == nil {
			select {
			case l.answer <- answer:
			default:
			}
		}
	case "close":
		go func() { _ = l.conn.Close() }()
	}
}

func (p *Peer) newLink(id, remote, mode string, shared ...*rtcCarrier) (*link, error) {
	l := &link{remote: remote, mode: mode, answer: make(chan webrtc.SessionDescription, 1), ready: make(chan struct{})}
	l.conn = newConn(p.ctx, func(ctx context.Context, b []byte) error {
		return p.send(ctx, &protocol.Packet{Kind: "data", To: remote, Session: id, Data: b})
	}, func() {
		l.dcMu.Lock()
		dc := l.dc
		l.dcMu.Unlock()
		if dc != nil {
			go func() { _ = dc.Close() }()
		}
		p.mu.Lock()
		delete(p.links, id)
		closePC := p.releaseCarrierLocked(l, id)
		p.mu.Unlock()
		if closePC {
			go func() { _ = l.pc.Close() }()
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = p.send(ctx, &protocol.Packet{Kind: "close", To: remote, Session: id})
	})
	if mode == "webrtc" {
		if len(shared) != 0 {
			l.carrier, l.pc = shared[0], shared[0].pc
		} else {
			pc, err := webrtc.NewPeerConnection(webrtc.Configuration{ICEServers: p.cfg.ICEServers})
			if err != nil {
				l.conn.cancel()
				return nil, err
			}
			l.pc = pc
			l.carrier = &rtcCarrier{pc: pc, remote: remote, links: map[string]*link{}}
			pc.OnDataChannel(func(dc *webrtc.DataChannel) { p.receiveChannel(l, dc) })
			pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
				if state == webrtc.PeerConnectionStateFailed || state == webrtc.PeerConnectionStateClosed {
					p.closeCarrier(l.carrier)
				}
			})
		}
	}
	p.mu.Lock()
	if p.ctx.Err() != nil {
		p.mu.Unlock()
		l.conn.cancel()
		if l.pc != nil && len(shared) == 0 {
			_ = l.pc.Close()
		}
		return nil, p.ctx.Err()
	}
	if _, exists := p.links[id]; exists || len(p.links) >= 256 || (l.carrier != nil && l.carrier.closed) {
		p.mu.Unlock()
		l.conn.cancel()
		if l.pc != nil && len(shared) == 0 {
			_ = l.pc.Close()
		}
		return nil, errors.New("duplicate session or peer link limit reached")
	}
	p.links[id] = l
	if l.carrier != nil {
		l.carrier.links[id] = l
		l.carrier.shared = p.cfg.Node.PeerChannels && p.channels[remote]
		if l.carrier.shared && p.carriers[remote] == nil {
			p.carriers[remote] = l.carrier
		}
	}
	p.mu.Unlock()
	return l, nil
}

func bindDataChannel(l *link, dc *webrtc.DataChannel) {
	if !dc.Ordered() || dc.MaxRetransmits() != nil || dc.MaxPacketLifeTime() != nil {
		_ = dc.Close()
		return
	}
	bound := false
	l.dcOnce.Do(func() { bound = true; bindReliableChannel(l, dc) })
	if !bound {
		_ = dc.Close()
	}
}

func bindReliableChannel(l *link, dc *webrtc.DataChannel) {
	l.dcMu.Lock()
	l.dc = dc
	l.dcMu.Unlock()
	if l.conn.ctx.Err() != nil {
		_ = dc.Close()
		return
	}
	low := make(chan struct{}, 1)
	dc.SetBufferedAmountLowThreshold(256 * 1024)
	dc.OnBufferedAmountLow(func() {
		select {
		case low <- struct{}{}:
		default:
		}
	})
	l.conn.send = func(ctx context.Context, b []byte) error {
		for dc.BufferedAmount() > 1024*1024 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-low:
			}
		}
		return dc.Send(b)
	}
	dc.OnMessage(func(msg webrtc.DataChannelMessage) {
		if msg.IsString || !l.conn.push(msg.Data) {
			go func() { _ = l.conn.Close() }()
		}
	})
	dc.OnClose(func() { go func() { _ = l.conn.Close() }() })
	dc.OnOpen(func() { l.readyOnce.Do(func() { close(l.ready) }) })
}

func (p *Peer) accept(ctx context.Context, msg *protocol.Packet) {
	// Check the scoped directory before processing an SDP offer or accepting
	// relay bytes. TLS then proves possession of this enrolled identity.
	if _, err := p.Resolve(ctx, msg.From); err != nil {
		return
	}
	mode := "relay"
	if len(msg.Data) > 0 {
		mode = "webrtc"
	}
	if mode == "webrtc" && p.cfg.RelayOnly {
		_ = p.send(p.ctx, &protocol.Packet{Kind: "close", To: msg.From, Session: msg.Session})
		return
	}
	l, err := p.newLink(msg.Session, msg.From, mode)
	if err != nil {
		return
	}
	handedOver := false
	defer func() {
		if !handedOver {
			_ = l.conn.Close()
		}
	}()
	if mode == "webrtc" {
		var offer webrtc.SessionDescription
		if json.Unmarshal(msg.Data, &offer) != nil {
			return
		}
		if err = l.pc.SetRemoteDescription(offer); err != nil {
			return
		}
		answer, e := l.pc.CreateAnswer(nil)
		if e != nil {
			return
		}
		gather := webrtc.GatheringCompletePromise(l.pc)
		if err = l.pc.SetLocalDescription(answer); err != nil {
			return
		}
		select {
		case <-gather:
		case <-ctx.Done():
			return
		case <-l.conn.ctx.Done():
			return
		}
		if err = p.send(ctx, &protocol.Packet{Kind: "answer", To: msg.From, Session: msg.Session, Data: model.JSON(l.pc.LocalDescription())}); err != nil {
			return
		}
		select {
		case <-l.ready:
		case <-ctx.Done():
			return
		case <-l.conn.ctx.Done():
			return
		}
	} else {
		if err = p.send(ctx, &protocol.Packet{Kind: "answer", To: msg.From, Session: msg.Session}); err != nil {
			return
		}
	}
	secure := tls.Server(l.conn, p.cfg.Identity.TLS(msg.From))
	if err = secure.HandshakeContext(ctx); err != nil {
		return
	}
	session, err := yamux.Server(secure, muxConfig())
	if err != nil {
		return
	}
	s := &peerSession{mux: session, id: msg.Session, remote: msg.From, lane: IncomingLane, mode: mode, outgoing: make(chan struct{}, 128), incoming: make(chan struct{}, 128)}
	if !p.registerSession(s) {
		_ = session.Close()
		return
	}
	handedOver = p.worker(func() { p.serve(s); _ = l.conn.Close() })
	if !handedOver {
		_ = session.Close()
	}
}

func muxConfig(lanes ...Lane) *yamux.Config {
	cfg := yamux.DefaultConfig()
	cfg.LogOutput = io.Discard
	cfg.StreamOpenTimeout = 15 * time.Second
	cfg.StreamCloseTimeout = 5 * time.Second
	cfg.AcceptBacklog = 128
	if len(lanes) > 0 && lanes[0] == BulkLane {
		cfg.MaxStreamWindowSize = 1024 * 1024
	}
	return cfg
}

func (p *Peer) Nodes(ctx context.Context) ([]model.Node, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, gateway.URL(p.cfg.Gateway, "/v1/nodes"), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+p.cfg.Token)
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("directory: %s", resp.Status)
	}
	var nodes []model.Node
	err = json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&nodes)
	p.mu.Lock()
	userID := p.userID
	p.mu.Unlock()
	for _, node := range nodes {
		if userID == "" || node.UserID != userID {
			return nil, errors.New("directory returned a node outside the authenticated fleet")
		}
	}
	return nodes, err
}

// Lookup separates fleet-machine discovery from identity checks for live clients.
func (p *Peer) Lookup(ctx context.Context, name string) (model.Node, error) {
	p.mu.Lock()
	clientSessions, userID := p.clientSessions, p.userID
	p.mu.Unlock()
	if clientSessions && len(name) == 64 {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, gateway.URL(p.cfg.Gateway, "/v1/peers/"+url.PathEscape(name)), nil)
		if err != nil {
			return model.Node{}, err
		}
		req.Header.Set("Authorization", "Bearer "+p.cfg.Token)
		resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
		if err != nil {
			return model.Node{}, err
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode == http.StatusOK {
			var peer model.Node
			if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&peer); err != nil {
				return model.Node{}, err
			}
			if peer.ID != name || userID == "" || peer.UserID != userID {
				return model.Node{}, errors.New("gateway returned a peer outside the authenticated fleet")
			}
			return peer, nil
		}
		if resp.StatusCode != http.StatusNotFound {
			return model.Node{}, fmt.Errorf("peer directory: %s", resp.Status)
		}
		return model.Node{}, fmt.Errorf("unknown node %q", name)
	}
	nodes, err := p.Nodes(ctx)
	if err != nil {
		return model.Node{}, err
	}
	for _, node := range nodes {
		if node.ID == name || node.Name == name {
			return node, nil
		}
	}
	return model.Node{}, fmt.Errorf("unknown node %q", name)
}

func (p *Peer) Resolve(ctx context.Context, name string) (model.Node, error) {
	peer, err := p.Lookup(ctx, name)
	if err != nil {
		return peer, err
	}
	if !peer.Online {
		return peer, fmt.Errorf("node %s is offline", name)
	}
	p.mu.Lock()
	p.members[peer.ID] = true
	p.channels[peer.ID] = peer.PeerChannels
	if peer.ClientOwner != "" {
		p.owners[peer.ID] = peer.ClientOwner
	}
	p.mu.Unlock()
	return peer, nil
}

// IsMember only accepts identities attested by this gateway's scoped directory.
// Identity ownership is immutable, so established direct sessions can remain
// usable during a gateway outage without crossing fleet boundaries.
func (p *Peer) IsMember(id string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.members[id]
}

// Owner is derived only from authenticated gateway metadata, never from an RPC.
// Transport identities still identify artifact-grant recipients and TLS peers.
func (p *Peer) Owner(id string) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if owner := p.owners[id]; owner != "" {
		return owner
	}
	return id
}

func (p *Peer) IdentityID() string { return p.cfg.Identity.ID }

func (p *Peer) dial(ctx context.Context, remote, mode string, lane Lane) (_ *peerSession, err error) {
	if mode == "webrtc" {
		p.mu.Lock()
		carrier := p.carriers[remote]
		p.mu.Unlock()
		if carrier != nil {
			return p.dialChannel(ctx, carrier, lane)
		}
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	id := identity.NewID()
	l, err := p.newLink(id, remote, mode)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			_ = l.conn.Close()
		}
	}()
	var offer []byte
	if mode == "webrtc" {
		dc, e := l.pc.CreateDataChannel("control-v1", nil)
		if e != nil {
			return nil, e
		}
		bindDataChannel(l, dc)
		sdp, e := l.pc.CreateOffer(nil)
		if e != nil {
			return nil, e
		}
		gather := webrtc.GatheringCompletePromise(l.pc)
		if e = l.pc.SetLocalDescription(sdp); e != nil {
			return nil, e
		}
		select {
		case <-gather:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		offer = model.JSON(l.pc.LocalDescription())
	}
	if err = p.send(ctx, &protocol.Packet{Kind: "open", To: remote, Session: id, Data: offer}); err != nil {
		return nil, err
	}
	if mode == "webrtc" {
		select {
		case answer := <-l.answer:
			if err = l.pc.SetRemoteDescription(answer); err != nil {
				return nil, err
			}
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-l.conn.ctx.Done():
			return nil, errors.New("peer rejected direct session")
		}
	}
	select {
	case <-l.ready:
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-l.conn.ctx.Done():
		return nil, errors.New("peer closed session")
	}
	secure := tls.Client(l.conn, p.cfg.Identity.TLS(remote))
	if err = secure.HandshakeContext(ctx); err != nil {
		return nil, err
	}
	session, err := yamux.Client(secure, muxConfig(lane))
	if err != nil {
		return nil, err
	}
	return &peerSession{mux: session, id: id, remote: remote, lane: lane, mode: mode, outgoing: make(chan struct{}, 128), incoming: make(chan struct{}, 128)}, nil
}

func (p *Peer) Connections() map[string]string {
	p.mu.Lock()
	defer p.mu.Unlock()
	result := map[string]string{}
	for _, s := range p.allSessions {
		if !s.mux.IsClosed() && result[s.remote] != "webrtc" {
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
		p.drainWrites()
	})
	return nil
}
