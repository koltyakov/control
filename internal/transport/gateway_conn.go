package transport

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/koltyakov/control/internal/gateway"
	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/protocol"
	"github.com/koltyakov/control/internal/wsio"
	"github.com/pion/webrtc/v4"
	"google.golang.org/protobuf/proto"
)

func (p *Peer) connect(ctx context.Context) (*websocket.Conn, error) {
	if err := p.cfg.Route.Wait(ctx); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	p.mu.Lock()
	generation := p.networkGeneration
	p.connectCancel = cancel
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		p.connectCancel = nil
		p.mu.Unlock()
	}()
	// Fleet identity comes from the authenticated gateway, never from labels or
	// an incoming peer. Refuse old gateways that cannot attest membership.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, gateway.URL(p.cfg.Gateway, "/v1/auth"), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+p.cfg.Token)
	connectClient := p.cfg.Route.ConnectClient()
	resp, err := connectClient.Do(req)
	if err != nil {
		return nil, gatewayNetworkError(err)
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		return nil, gatewayResponseError(resp.StatusCode, "gateway authentication")
	}
	var scope struct {
		UserID            string   `json:"userId"`
		NodeHealth        bool     `json:"nodeHealth"`
		ClientSessions    bool     `json:"clientSessions"`
		ClientOwners      bool     `json:"clientOwners"`
		PeerChannels      bool     `json:"peerChannels"`
		Delegation        bool     `json:"delegation"`
		LargePackets      bool     `json:"largePackets"`
		NodeBytes         bool     `json:"nodeBytes"`
		MachineStatePush  bool     `json:"machineStatePush"`
		LANDiscovery      bool     `json:"lanDiscovery"`
		LANView           bool     `json:"lanView"`
		GatewayHeartbeat  bool     `json:"gatewayHeartbeat"`
		TelemetryLiveness bool     `json:"telemetryLiveness"`
		RelayDrain        bool     `json:"relayDrain"`
		GatewayProtocol   int      `json:"gatewayProtocol"`
		TrickleICE        bool     `json:"trickleICE"`
		STUNServers       []string `json:"stunServers"`
	}
	err = json.NewDecoder(io.LimitReader(resp.Body, 8192)).Decode(&scope)
	_ = resp.Body.Close()
	if err != nil {
		return nil, gatewayNetworkError(err)
	}
	if scope.UserID == "" {
		return nil, errors.New("gateway did not authenticate fleet membership; upgrade the gateway before this node")
	}
	if !p.cfg.Client && p.cfg.Node.Software.NodeGatewayMax != 0 && !p.cfg.Node.Software.SupportsGateway(scope.GatewayProtocol) {
		return nil, errors.New("node executable is incompatible with the running gateway protocol")
	}
	if p.cfg.Client && !scope.ClientSessions {
		return nil, errors.New("gateway does not support client sessions; upgrade the gateway and target nodes before standalone CLI use")
	}
	if p.cfg.ClientOwner != nil && (!p.cfg.Client || !scope.ClientOwners) {
		return nil, errors.New("gateway does not support concurrent client owners; upgrade the gateway and target nodes")
	}
	var wakeInterfaces []model.WakeInterface
	// The first Wake-on-LAN gateway rejects metadata without an explicit lan
	// label. Nodes update before the gateway during managed rollouts, so only
	// enable automatic reporting once the gateway advertises LAN discovery.
	if !p.cfg.Client && scope.NodeBytes && (scope.LANDiscovery || p.cfg.Node.Labels["lan"] != "") {
		wakeInterfaces, err = p.localWakeInterfaces()
		if err != nil {
			p.log.Warn("cannot collect Wake-on-LAN interfaces", "error", err)
		}
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
	p.telemetryLiveness.Store(scope.TelemetryLiveness && scope.GatewayHeartbeat && !p.cfg.Client)
	p.gatewayHeartbeat.Store(scope.GatewayHeartbeat)
	p.clientSessions = scope.ClientSessions
	p.cfg.Node.UserID = scope.UserID
	// Negotiate this optional signed field so new machines still connect to old
	// gateways whose registration decoder does not know about client sessions.
	p.cfg.Node.ClientSessions = scope.ClientSessions && !p.cfg.Client
	p.cfg.Node.ClientOwners = scope.ClientOwners && !p.cfg.Client
	p.cfg.Node.PeerChannels = scope.PeerChannels
	p.cfg.Node.LargePackets = scope.LargePackets
	p.cfg.Node.TrickleICE = scope.TrickleICE
	// Registration signs exact bytes only with nodeBytes, and an older gateway
	// could not verify a field it does not know about otherwise.
	p.cfg.Node.MachineStatePush = scope.MachineStatePush && scope.NodeBytes && !p.cfg.Client
	p.machineStatePush.Store(p.cfg.Node.MachineStatePush)
	p.cfg.Node.WakeInterfaces = wakeInterfaces
	p.cfg.Node.LANDiscovery = scope.LANDiscovery && scope.NodeBytes && !p.cfg.Client
	p.lanDiscovery = p.cfg.Node.LANDiscovery
	p.iceServers = p.cfg.ICEServers
	if p.iceServers == nil && len(scope.STUNServers) != 0 {
		p.iceServers = []webrtc.ICEServer{{URLs: scope.STUNServers}}
	}
	p.cfg.Node.InstructionDelegation = scope.Delegation && !p.cfg.Client
	nodeBytes := scope.NodeBytes
	if p.cfg.ClientOwner != nil {
		p.cfg.Node.ClientOwner = p.cfg.ClientOwner.ID
	}
	p.mu.Unlock()
	u := gateway.URL(p.cfg.Gateway, "/v1/connect")
	if p.cfg.Client {
		u = gateway.URL(p.cfg.Gateway, "/v1/client/connect")
	}
	u = strings.Replace(strings.Replace(u, "https://", "wss://", 1), "http://", "ws://", 1)
	ws, response, err := websocket.Dial(ctx, u, &websocket.DialOptions{HTTPClient: connectClient, HTTPHeader: http.Header{"Authorization": {"Bearer " + p.cfg.Token}}})
	if err != nil {
		if response != nil && response.StatusCode != http.StatusSwitchingProtocols {
			return nil, gatewayResponseError(response.StatusCode, "connect gateway")
		}
		return nil, fmt.Errorf("connect gateway: %w", gatewayNetworkError(err))
	}
	ws.SetReadLimit(1 << 20)
	_, challenge, err := ws.Read(ctx)
	if err == nil && len(challenge) != 32 {
		err = errors.New("invalid gateway challenge")
	}
	var lanViewSession string
	if err == nil {
		p.mu.Lock()
		node := p.cfg.Node
		p.mu.Unlock()
		hello := gateway.Hello{Version: model.Version, Node: node, Client: p.cfg.Client, GatewayHeartbeat: scope.GatewayHeartbeat, RelayDrain: scope.RelayDrain}
		if nodeBytes {
			// Sign the exact registration bytes so a gateway that does not know
			// a newer field ignores it instead of failing signature verification.
			hello.NodeBytes, hello.Node = model.JSON(node), model.Node{}
		} else if hello.Node.System != nil {
			// Legacy gateways reconstruct the signed Node with their known
			// fields. New nested metrics must not change those signature bytes.
			info := *hello.Node.System
			info.GPUs = nil
			hello.Node.System = &info
		}
		if !nodeBytes {
			hello.Node.GatewayProxyManagement = false
			hello.Node.SecretManagementKey = nil
			hello.Node.SecretPolicy = false
			hello.Node.Software.Runtime = ""
			hello.Node.Software.GatewayProtocol = 0
			hello.Node.Software.NodeGatewayMin, hello.Node.Software.NodeGatewayMax = 0, 0
			// Legacy gateways re-encode known fields only; never sign this one.
			hello.Node.ProxyEndpoints = nil
			hello.Node.GatewayProxy = nil
		}
		hello.Signature = ed25519.Sign(p.cfg.Identity.Private, hello.Message(challenge))
		if scope.LANView && node.LANDiscovery && !p.cfg.Client {
			lanViewSession = gateway.LANViewSession(hello.Signature)
		}
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
		return nil, gatewayNetworkError(err)
	}
	p.mu.Lock()
	if generation != p.networkGeneration {
		p.mu.Unlock()
		_ = ws.CloseNow()
		return nil, gatewayNetworkError(errors.New("network changed during gateway connection"))
	}
	p.ws = ws
	p.lanViewSession = lanViewSession
	p.relayDrainUntil, p.relayDrainID = time.Time{}, ""
	clear(p.relayPeerDrains)
	p.gatewayWS.Store(ws)
	// A reconnect may have missed an owner rename. Keep identity-bound sessions,
	// but resolve names again before opening new streams through them.
	p.resolved = map[string]string{}
	p.lookups = map[string]lookupEntry{}
	p.directoryGeneration++
	if p.closed || p.ctx.Err() != nil {
		p.ws = nil
		p.lanViewSession = ""
		p.gatewayWS.Store(nil)
		p.mu.Unlock()
		_ = ws.CloseNow()
		return nil, errors.New("peer stopped during connection")
	}
	// Count a reconnect before signaling so observers of gatewayConnected
	// see the stat that describes the session they were woken for.
	if p.gatewaySession.Add(1) > 1 {
		p.stats.gatewayReconnects.Add(1)
	}
	p.mu.Unlock()
	select {
	case p.gatewayConnected <- struct{}{}:
	default:
	}
	return ws, nil
}

// gatewayReadIdle is six heartbeat intervals without any gateway packet.
const gatewayReadIdle = 30 * time.Second

func (p *Peer) run(ws *websocket.Conn) {
	for {
		var reader wsio.Reader
		// A heartbeat gateway writes at least every few seconds. Silence
		// beyond gatewayReadIdle means the path died without a local route
		// change, so close the socket instead of waiting on TCP timeouts.
		var watchdog *time.Timer
		idle := gatewayReadIdle
		if p.readIdle > 0 {
			idle = p.readIdle
		}
		if p.gatewayHeartbeat.Load() {
			current := ws
			watchdog = time.AfterFunc(idle, func() {
				p.stats.gatewayIdleCloses.Add(1)
				_ = current.CloseNow()
			})
		}
		// Resetting a timer per packet costs more than the packet at relay
		// rates; renew at most once a second, extending idle by under that.
		var renewed time.Time
		renew := min(time.Second, idle/4)
		for {
			_, b, err := reader.Read(p.ctx, ws)
			if err != nil {
				break
			}
			if watchdog != nil {
				if now := time.Now(); now.Sub(renewed) >= renew {
					watchdog.Reset(idle)
					renewed = now
				}
			}
			var msg protocol.Packet
			if proto.Unmarshal(b, &msg) != nil {
				break
			}
			p.receive(&msg)
		}
		if watchdog != nil {
			watchdog.Stop()
		}
		_ = ws.CloseNow()
		p.mu.Lock()
		if p.ws == ws {
			p.ws = nil
			p.lanViewSession = ""
			p.gatewayWS.Store(nil)
		}
		// The gateway closes this connection when it retires the identity, and no
		// hint can arrive while disconnected. Check now, then poll at the fast rate.
		p.signalMachineState()
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
		// A lost socket often means a dead path; never reuse its pooled peers.
		p.cfg.Route.CloseIdleConnections()
		// Jitter keeps nodes from reconnecting in lockstep after a gateway
		// restart. Persistent failures, such as a rejected credential, back off.
		delay, failures := reconnectInitial, 0
		for {
			select {
			case <-p.ctx.Done():
				return
			case <-time.After(delay/2 + rand.N(delay)):
			case <-p.reconnectWake:
				delay, failures = reconnectInitial, 0
			}
			var err error
			ws, err = p.connect(p.ctx)
			if err == nil {
				if failures > 0 {
					p.log.Info("gateway reconnected", "attempts", failures+1)
				}
				break
			}
			failures++
			p.stats.gatewayConnectFailure.Add(1)
			if failures == 1 || failures%10 == 0 {
				p.log.Warn("gateway reconnect failed", "attempts", failures, "error", err)
			} else {
				p.log.Debug("gateway reconnect failed", "attempts", failures, "error", err)
			}
			if failures >= reconnectFastAttempts {
				delay = min(delay*2, reconnectMax)
			}
		}
	}
}

func (p *Peer) send(ctx context.Context, msg *protocol.Packet) error {
	// The send path runs per relay chunk, so it must not serialize on p.mu
	// behind session setup or directory work; the live socket is mirrored
	// in gatewayWS and lifecycle state comes from the peer context.
	ws := p.gatewayWS.Load()
	if p.ctx == nil || p.ctx.Err() != nil {
		return errors.New("gateway disconnected")
	}
	if ws == nil {
		// The running peer is reconnecting to its gateway.
		return transientSetup("gateway disconnected")
	}
	pooled := getWriteBuffer()
	b, err := proto.MarshalOptions{}.MarshalAppend((*pooled)[:0], msg)
	if err != nil {
		putWriteBuffer(pooled)
		return err
	}
	*pooled = b
	// Requests, their result channels, and their timeout timers are reused per
	// packet. A request the sender stops waiting for stays with the writer.
	request := getGatewayWrite(ctx, ws, b, pooled)
	queue := p.writes
	if msg.Kind != "data" && msg.Kind != "close" {
		queue = p.priorityWrites
	}
	select {
	case queue <- request:
	case <-ctx.Done():
		putWriteBuffer(pooled)
		putGatewayWrite(request)
		return ctx.Err()
	case <-request.timeout.C:
		putWriteBuffer(pooled)
		putGatewayWrite(request)
		return context.DeadlineExceeded
	case <-p.ctx.Done():
		putWriteBuffer(pooled)
		putGatewayWrite(request)
		return p.ctx.Err()
	}
	select {
	case err := <-request.done:
		putGatewayWrite(request)
		return err
	case <-ctx.Done():
		request.abandoned.Store(true)
		return ctx.Err()
	case <-request.timeout.C:
		request.abandoned.Store(true)
		return context.DeadlineExceeded
	case <-p.ctx.Done():
		request.abandoned.Store(true)
		return p.ctx.Err()
	}
}
