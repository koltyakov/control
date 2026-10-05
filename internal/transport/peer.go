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
	Gateway       string
	Token         string
	Identity      *identity.Identity
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
	answer    chan webrtc.SessionDescription
	ready     chan struct{}
	readyOnce sync.Once
}

type Peer struct {
	userID     string
	members    map[string]bool
	cfg        Config
	ctx        context.Context
	cancel     context.CancelFunc
	mu         sync.Mutex
	ws         *websocket.Conn
	links      map[string]*link
	sessions   map[string]*yamux.Session
	resolved   map[string]string
	dialMu     sync.Mutex
	handler    func(string, net.Conn)
	log        *slog.Logger
	wg         sync.WaitGroup
	control    func(string, []byte)
	nodeHealth bool
}

func New(cfg Config, handler func(string, net.Conn)) *Peer {
	if cfg.DirectTimeout == 0 {
		cfg.DirectTimeout = 5 * time.Second
	}
	return &Peer{cfg: cfg, links: map[string]*link{}, sessions: map[string]*yamux.Session{}, resolved: map[string]string{}, members: map[string]bool{}, handler: handler, log: slog.Default()}
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
	p.ctx, p.cancel = context.WithCancel(ctx)
	ws, err := p.connect(p.ctx)
	if err != nil {
		p.cancel()
		return err
	}
	p.wg.Add(1)
	go func() { defer p.wg.Done(); p.run(ws) }()
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
		UserID     string `json:"userId"`
		NodeHealth bool   `json:"nodeHealth"`
	}
	err = json.NewDecoder(io.LimitReader(resp.Body, 8192)).Decode(&scope)
	_ = resp.Body.Close()
	if err != nil || resp.StatusCode != http.StatusOK || scope.UserID == "" {
		return nil, errors.New("gateway did not authenticate fleet membership; upgrade the gateway before this node")
	}
	p.mu.Lock()
	if p.userID != "" && p.userID != scope.UserID {
		p.mu.Unlock()
		return nil, errors.New("node fleet cannot change")
	}
	p.userID = scope.UserID
	p.nodeHealth = scope.NodeHealth
	p.mu.Unlock()
	p.cfg.Node.UserID = scope.UserID
	u := gateway.URL(p.cfg.Gateway, "/v1/connect")
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
		signature := ed25519.Sign(p.cfg.Identity.Private, append(challenge, model.JSON(p.cfg.Node)...))
		err = ws.Write(ctx, websocket.MessageBinary, model.JSON(gateway.Hello{Version: model.Version, Node: p.cfg.Node, Signature: signature}))
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
	p.mu.Unlock()
	if ws == nil {
		return errors.New("gateway disconnected")
	}
	b, err := proto.Marshal(msg)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return ws.Write(ctx, websocket.MessageBinary, b)
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
		full := len(p.links) >= 256
		p.mu.Unlock()
		if exists || full {
			return
		}
		p.wg.Add(1)
		go func() { defer p.wg.Done(); p.accept(msg) }()
		return
	}
	p.mu.Lock()
	l := p.links[msg.Session]
	p.mu.Unlock()
	if l == nil || l.remote != msg.From {
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

func (p *Peer) newLink(id, remote, mode string) (*link, error) {
	l := &link{remote: remote, mode: mode, answer: make(chan webrtc.SessionDescription, 1), ready: make(chan struct{})}
	l.conn = newConn(p.ctx, func(ctx context.Context, b []byte) error {
		return p.send(ctx, &protocol.Packet{Kind: "data", To: remote, Session: id, Data: b})
	}, func() {
		p.mu.Lock()
		delete(p.links, id)
		p.mu.Unlock()
		if l.pc != nil {
			go func() { _ = l.pc.Close() }()
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = p.send(ctx, &protocol.Packet{Kind: "close", To: remote, Session: id})
	})
	if mode == "webrtc" {
		pc, err := webrtc.NewPeerConnection(webrtc.Configuration{ICEServers: p.cfg.ICEServers})
		if err != nil {
			return nil, err
		}
		l.pc = pc
		pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
			if state == webrtc.PeerConnectionStateFailed || state == webrtc.PeerConnectionStateClosed {
				go func() { _ = l.conn.Close() }()
			}
		})
	}
	p.mu.Lock()
	if p.ctx.Err() != nil {
		p.mu.Unlock()
		if l.pc != nil {
			_ = l.pc.Close()
		}
		return nil, p.ctx.Err()
	}
	if _, exists := p.links[id]; exists {
		p.mu.Unlock()
		if l.pc != nil {
			_ = l.pc.Close()
		}
		return nil, errors.New("duplicate session")
	}
	p.links[id] = l
	p.mu.Unlock()
	return l, nil
}

func bindDataChannel(l *link, dc *webrtc.DataChannel) {
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
		if !l.conn.push(msg.Data) {
			go func() { _ = l.conn.Close() }()
		}
	})
	dc.OnClose(func() { go func() { _ = l.conn.Close() }() })
	dc.OnOpen(func() { l.readyOnce.Do(func() { close(l.ready) }) })
}

func (p *Peer) accept(msg *protocol.Packet) {
	ctx, cancel := context.WithTimeout(p.ctx, 20*time.Second)
	defer cancel()
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
	defer func() { _ = l.conn.Close() }()
	if mode == "webrtc" {
		l.pc.OnDataChannel(func(dc *webrtc.DataChannel) { bindDataChannel(l, dc) })
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
	defer func() { _ = session.Close() }()
	p.serve(session, msg.From)
}

func muxConfig() *yamux.Config {
	cfg := yamux.DefaultConfig()
	cfg.LogOutput = io.Discard
	cfg.StreamOpenTimeout = 15 * time.Second
	cfg.StreamCloseTimeout = 5 * time.Second
	return cfg
}

func (p *Peer) serve(session *yamux.Session, remote string) {
	for {
		stream, err := session.AcceptStream()
		if err != nil {
			return
		}
		p.wg.Add(1)
		go func() {
			defer p.wg.Done()
			defer func() { _ = stream.Close() }()
			if p.handler != nil {
				p.handler(remote, stream)
			}
		}()
	}
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

func (p *Peer) Resolve(ctx context.Context, name string) (model.Node, error) {
	nodes, err := p.Nodes(ctx)
	if err != nil {
		return model.Node{}, err
	}
	for _, node := range nodes {
		if node.ID == name || node.Name == name {
			if !node.Online {
				return node, fmt.Errorf("node %s is offline", name)
			}
			p.mu.Lock()
			p.members[node.ID] = true
			p.mu.Unlock()
			return node, nil
		}
	}
	return model.Node{}, fmt.Errorf("unknown node %q", name)
}

// IsMember only accepts identities attested by this gateway's scoped directory.
// Identity ownership is immutable, so established direct sessions can remain
// usable during a gateway outage without crossing fleet boundaries.
func (p *Peer) IsMember(id string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.members[id]
}

func (p *Peer) Open(ctx context.Context, target string) (net.Conn, error) {
	p.mu.Lock()
	id := p.resolved[target]
	if id == "" {
		id = target
	}
	cached := p.sessions[id]
	p.mu.Unlock()
	if cached != nil && !cached.IsClosed() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		stream, err := cached.OpenStream()
		if err == nil {
			return stream, nil
		}
		// Nothing has been sent on an application stream yet, so retrying
		// session establishment cannot replay a command or transfer.
		_ = cached.Close()
		p.mu.Lock()
		if p.sessions[id] == cached {
			delete(p.sessions, id)
		}
		p.mu.Unlock()
	}
	node, err := p.Resolve(ctx, target)
	if err != nil {
		return nil, err
	}
	p.dialMu.Lock()
	defer p.dialMu.Unlock()
	p.mu.Lock()
	session := p.sessions[node.ID]
	p.mu.Unlock()
	if session == nil || session.IsClosed() {
		session = nil
		if !p.cfg.RelayOnly {
			directCtx, cancel := context.WithTimeout(ctx, p.cfg.DirectTimeout)
			session, err = p.dial(directCtx, node.ID, "webrtc")
			cancel()
			if err != nil {
				p.log.Debug("using relay", "node", node.Name, "reason", err)
			}
		}
		if session == nil || err != nil {
			session, err = p.dial(ctx, node.ID, "relay")
		}
		if err != nil {
			return nil, err
		}
		p.mu.Lock()
		p.sessions[node.ID] = session
		p.resolved[target] = node.ID
		p.resolved[node.Name] = node.ID
		p.mu.Unlock()
		p.wg.Add(1)
		go func() { defer p.wg.Done(); p.serve(session, node.ID) }()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return session.OpenStream()
}

func (p *Peer) dial(ctx context.Context, remote, mode string) (_ *yamux.Session, err error) {
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
	return yamux.Client(secure, muxConfig())
}

func (p *Peer) Connections() map[string]string {
	p.mu.Lock()
	defer p.mu.Unlock()
	result := map[string]string{}
	for _, l := range p.links {
		result[l.remote] = l.mode
	}
	return result
}

func (p *Peer) Close() error {
	if p.cancel != nil {
		p.cancel()
	}
	p.mu.Lock()
	ws := p.ws
	var links []*link
	for _, l := range p.links {
		links = append(links, l)
	}
	p.mu.Unlock()
	if ws != nil {
		_ = ws.CloseNow()
	}
	for _, l := range links {
		_ = l.conn.Close()
	}
	p.wg.Wait()
	return nil
}
