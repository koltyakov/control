// Package gateway implements enrollment, discovery, signaling, encrypted relay
// fallback, and fleet administration. It does not schedule or execute application work.
package gateway

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/gofrs/flock"
	"github.com/koltyakov/control/internal/identity"
	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/protocol"
	"github.com/koltyakov/control/internal/system"
	"github.com/koltyakov/control/internal/update"
	"google.golang.org/protobuf/proto"
)

type Hello struct {
	Version string     `json:"version"`
	Node    model.Node `json:"node"`
	// NodeBytes, when present, is the exact signed registration. Gateways that
	// advertise nodeBytes decode it leniently, so a newer node's unknown fields
	// no longer invalidate a signature computed over a re-encoding.
	NodeBytes      []byte `json:"nodeBytes,omitempty"`
	Client         bool   `json:"client,omitempty"`
	OwnerKey       []byte `json:"ownerKey,omitempty"`
	OwnerSignature []byte `json:"ownerSignature,omitempty"`
	Signature      []byte `json:"signature"`
}

const maxNodeBytes = 512 << 10

// Message binds client proofs to their role while preserving machine proofs.
func (h Hello) Message(challenge []byte) []byte {
	b := append([]byte(nil), challenge...)
	if h.Client {
		b = append(b, []byte("control-client-session-v1\x00")...)
	}
	if len(h.NodeBytes) != 0 {
		b = append(b, []byte("control-node-bytes-v1\x00")...)
		return append(b, h.NodeBytes...)
	}
	return append(b, model.JSON(h.Node)...)
}

// RegisteredNode returns the registration covered by the hello signature.
func (h Hello) RegisteredNode() (model.Node, error) {
	if len(h.NodeBytes) == 0 {
		return h.Node, nil
	}
	if len(h.NodeBytes) > maxNodeBytes {
		return model.Node{}, errors.New("registration exceeds size limit")
	}
	var n model.Node
	if err := json.Unmarshal(h.NodeBytes, &n); err != nil {
		return model.Node{}, err
	}
	return n, nil
}

// OwnerMessage authorizes this particular TLS identity to act for a stable
// client owner. A bearer credential alone cannot impersonate another owner.
func (h Hello) OwnerMessage(challenge []byte) []byte {
	return append([]byte("control-client-owner-v1\x00"), h.Message(challenge)...)
}

type connection struct {
	client            bool
	userID            string
	ws                *websocket.Conn
	out               chan *protocol.Packet
	queued            atomic.Int64 // Relay payload bytes waiting in out.
	keyID             string
	health            *model.NodeHealth
	healthAt          time.Time
	lifecycleAt       time.Time
	lifecycleRevision uint64
}

// Gateway manages fleet membership and connectivity for nodes and clients.
type Gateway struct {
	db               *sql.DB
	users            map[string]User
	owners           map[string]string
	clientOwners     map[string]bool
	clientTransports map[string]string
	token            string
	path             string
	mu               sync.Mutex
	nodes            map[string]model.Node
	clients          map[string]model.Node
	peers            map[string]*connection
	wg               sync.WaitGroup
	closed           bool
	restarting       bool
	lock             *flock.Flock
	superuser        string
	keys             map[string]keyRecord
	updates          *update.Repository
	options          Options
	updateStatus     map[string]update.Status
	updateCtx        context.Context
	updateCancel     context.CancelFunc
	updateWG         sync.WaitGroup
	rolloutMu        sync.Mutex
	installations    map[string]installation
	metrics          *system.Collector
	metricsCancel    context.CancelFunc
	metricsWG        sync.WaitGroup
	startedAt        time.Time
	machineStates    map[string]model.MachineState
	// credentials indexes usable bearer credentials by digest; see authenticate.
	credentials atomic.Pointer[map[string]principal]
	// routes mirrors peers for relay forwarding under its own lock, so packet
	// routing does not wait for commits or administration holding g.mu.
	routeMu  sync.RWMutex
	routes   map[string]*connection
	counters counters
}

func New(dir, token string, options ...Options) (*Gateway, error) {
	var opts Options
	if len(options) > 0 {
		opts = options[0]
	}
	if opts.SuperuserKey != "" && (len(opts.SuperuserKey) < 32 || opts.SuperuserKey == token) {
		return nil, errors.New("superuser key must be distinct from the common token and at least 32 characters")
	}
	if (token != "" && len(token) < 16) || (token == "" && opts.SuperuserKey == "") {
		return nil, errors.New("gateway token must be at least 16 characters")
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	lock := flock.New(filepath.Join(dir, "gateway.lock"))
	locked, err := lock.TryLock()
	if err != nil {
		return nil, err
	}
	if !locked {
		return nil, errors.New("gateway data directory is already in use")
	}
	g := &Gateway{token: token, path: filepath.Join(dir, "nodes.json"), nodes: map[string]model.Node{}, clients: map[string]model.Node{}, peers: map[string]*connection{}, routes: map[string]*connection{}}
	g.lock = lock
	g.superuser, g.options = opts.SuperuserKey, opts
	g.updateStatus = map[string]update.Status{}
	if err := g.openDatabase(dir); err != nil {
		if g.db != nil {
			_ = g.db.Close()
		}
		_ = lock.Close()
		return nil, err
	}
	if err := g.migrateLegacyClients(); err != nil {
		_ = g.db.Close()
		_ = lock.Close()
		return nil, err
	}
	g.updates, err = update.OpenRepository(filepath.Join(dir, "updates"))
	if err != nil {
		_ = g.db.Close()
		_ = lock.Close()
		return nil, err
	}
	g.reindexCredentialsLocked()
	var reset []string
	for id, n := range g.nodes {
		if n.Online {
			n.Online = false
			g.nodes[id] = n
			reset = append(reset, id)
		}
	}
	if len(reset) != 0 {
		if err := g.commit(change{nodes: reset}); err != nil {
			_ = g.db.Close()
			_ = lock.Close()
			return nil, err
		}
	}
	g.startedAt = time.Now().UTC()
	g.metrics = system.New([]string{dir}, 15*time.Second)
	metricsCtx, cancel := context.WithCancel(context.Background())
	g.metricsCancel = cancel
	g.metricsWG.Add(1)
	go func() {
		defer g.metricsWG.Done()
		_, _ = g.metrics.Refresh(metricsCtx)
		g.metrics.Run(metricsCtx)
	}()
	return g, nil
}

func (g *Gateway) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("ok\n")) })
	mux.HandleFunc("GET /v1/nodes", g.auth(g.list))
	mux.HandleFunc("GET /v1/status", g.auth(g.status))
	mux.HandleFunc("GET /v1/connect", g.auth(g.connect))
	mux.HandleFunc("GET /v1/client/connect", g.auth(g.connectClient))
	mux.HandleFunc("GET /v1/peers/{id}", g.auth(g.lookupPeer))
	mux.HandleFunc("GET /v1/admin/metrics", g.admin(g.metricsHandler))
	g.authRoutes(mux)
	g.userRoutes(mux)
	g.updateRoutes(mux)
	g.installationRoutes(mux)
	g.machineRoutes(mux)
	return mux
}

func (g *Gateway) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		role, _ := g.role(bearer(r))
		if role == "" {
			g.counters.authFailures.Add(1)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

func (g *Gateway) list(w http.ResponseWriter, r *http.Request) {
	p := g.authenticate(bearer(r))
	g.mu.Lock()
	nodes := make([]model.Node, 0, len(g.nodes))
	for _, n := range g.nodes {
		if n.UserID == p.UserID {
			n.ControlPending = g.machinePending(n.ID)
			nodes = append(nodes, n)
		}
	}
	g.mu.Unlock()
	sort.Slice(nodes, func(i, j int) bool { return nodeNameLess(nodes[i].Name, nodes[j].Name) })
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(nodes)
}

// nodeNameLess keeps machine lists alphabetical regardless of name casing.
// Original spelling breaks case-only ties without changing routing names.
func nodeNameLess(a, b string) bool {
	if lowerA, lowerB := strings.ToLower(a), strings.ToLower(b); lowerA != lowerB {
		return lowerA < lowerB
	}
	return a < b
}

var validName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,62}$`)

func (g *Gateway) connect(w http.ResponseWriter, r *http.Request) {
	g.connectPeer(w, r, false)
}

func (g *Gateway) connectClient(w http.ResponseWriter, r *http.Request) {
	g.connectPeer(w, r, true)
}

func (g *Gateway) connectPeer(w http.ResponseWriter, r *http.Request, client bool) {
	g.mu.Lock()
	if g.closed || g.restarting {
		g.mu.Unlock()
		http.Error(w, "gateway stopping", http.StatusServiceUnavailable)
		return
	}
	g.wg.Add(1)
	g.mu.Unlock()
	defer g.wg.Done()
	ws, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer func() { _ = ws.CloseNow() }()
	ws.SetReadLimit(1 << 20)
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	handshake, done := context.WithTimeout(ctx, 10*time.Second)
	defer done()
	challenge := make([]byte, 32)
	if _, err = rand.Read(challenge); err != nil {
		return
	}
	if err = ws.Write(handshake, websocket.MessageBinary, challenge); err != nil {
		return
	}
	_, data, err := ws.Read(handshake)
	if err != nil {
		return
	}
	var hello Hello
	if json.Unmarshal(data, &hello) != nil {
		return
	}
	n, err := hello.RegisteredNode()
	if err != nil {
		return
	}
	if hello.Client != client || hello.Version != model.Version || !validName.MatchString(n.Name) || len(n.PublicKey) != ed25519.PublicKeySize || identity.ID(n.PublicKey) != n.ID {
		return
	}
	// Sign both the fresh challenge and registration metadata.
	if !ed25519.Verify(n.PublicKey, hello.Message(challenge), hello.Signature) {
		return
	}
	ownerID := n.ID
	if client && len(hello.OwnerKey) != 0 {
		if len(hello.OwnerKey) != ed25519.PublicKeySize || !ed25519.Verify(hello.OwnerKey, hello.OwnerMessage(challenge), hello.OwnerSignature) {
			return
		}
		ownerID = identity.ID(hello.OwnerKey)
		if n.ClientOwner != ownerID || n.ID == ownerID {
			return
		}
	} else if n.ClientOwner != "" || len(hello.OwnerSignature) != 0 || (!client && len(hello.OwnerKey) != 0) {
		return
	}
	g.mu.Lock()
	p := g.authenticateLocked(bearer(r))
	if p.UserID == "" || (n.UserID != "" && n.UserID != p.UserID) || (g.owners[n.ID] != "" && g.owners[n.ID] != p.UserID) {
		g.mu.Unlock()
		return
	}
	n.UserID = p.UserID
	// A machine or common key cannot claim orchestrator authority in its proof.
	n.ExecutionAuthority = client && (p.Role == "user" || p.Role == "superuser")
	if g.machineStates[n.ID].Unregistered {
		g.mu.Unlock()
		_ = ws.Close(websocket.StatusPolicyViolation, "machine was unregistered")
		return
	}
	n.Disabled, n.ControlPending = g.machineStates[n.ID].Disabled, false
	registered := g.nodes[n.ID]
	n.Managed = registered.Managed && n.Software.SHA256 != "" && registered.Software.SHA256 == n.Software.SHA256
	if !client && g.machineStates[n.ID].Name != "" {
		n.Name = g.machineStates[n.ID].Name
	}
	c := &connection{client: client, ws: ws, out: make(chan *protocol.Packet, relayQueuePackets), keyID: p.KeyID, userID: p.UserID}
	if !g.allowInstallation(p.KeyID, n) {
		g.mu.Unlock()
		_ = ws.Close(websocket.StatusPolicyViolation, "credential does not authorize this registration")
		return
	}
	if g.closed || g.restarting {
		g.mu.Unlock()
		return
	}
	if client {
		if err := g.validateClient(n, p, ownerID); err != nil {
			g.mu.Unlock()
			_ = ws.Close(websocket.StatusPolicyViolation, err.Error())
			return
		}
	} else {
		if g.clientOwners[n.ID] {
			g.mu.Unlock()
			_ = ws.Close(websocket.StatusPolicyViolation, "client identities cannot enroll as machines")
			return
		}
		if g.clientTransports[n.ID] != "" {
			g.mu.Unlock()
			_ = ws.Close(websocket.StatusPolicyViolation, "client transport identities cannot enroll as machines")
			return
		}
		if g.clientNameReserved(n.UserID, n.Name) {
			g.mu.Unlock()
			_ = ws.Close(websocket.StatusPolicyViolation, "name is reserved for a client identity")
			return
		}
		for id, existing := range g.nodes {
			if existing.UserID == n.UserID && existing.Name == n.Name && id != n.ID {
				g.mu.Unlock()
				_ = ws.Close(websocket.StatusPolicyViolation, "name already enrolled")
				return
			}
		}
	}
	if _, exists := g.peers[n.ID]; exists {
		g.mu.Unlock()
		_ = ws.Close(websocket.StatusPolicyViolation, "identity already connected")
		return
	}
	n.Online = true
	n.LastSeen = time.Now().UTC()
	previous, existed := g.nodes[n.ID]
	n.UpdateUntil = previous.UpdateUntil // Never trust enrollment's display grace.
	previousOwner := g.owners[ownerID]
	g.owners[ownerID] = n.UserID
	previousClient := g.clientOwners[ownerID]
	previousTransportOwner := g.owners[n.ID]
	previousTransport := g.clientTransports[n.ID]
	if client {
		g.clientOwners[ownerID] = true
		if n.ID != ownerID {
			g.owners[n.ID] = n.UserID
			g.clientTransports[n.ID] = ownerID
		}
	} else {
		g.nodes[n.ID] = n
	}
	persisted := change{identities: []string{ownerID}}
	if client {
		persisted.clients = []string{ownerID}
		if n.ID != ownerID {
			persisted.identities = append(persisted.identities, n.ID)
			persisted.transports = []string{n.ID}
		}
	} else {
		persisted.nodes = []string{n.ID}
	}
	if err = g.commit(persisted); err != nil {
		if previousOwner == "" {
			delete(g.owners, ownerID)
		}
		if existed {
			g.nodes[n.ID] = previous
		} else {
			delete(g.nodes, n.ID)
		}
		if !previousClient {
			delete(g.clientOwners, ownerID)
		}
		if n.ID != ownerID && previousTransportOwner == "" {
			delete(g.owners, n.ID)
		}
		if previousTransport == "" {
			delete(g.clientTransports, n.ID)
		}
		g.mu.Unlock()
		return
	}
	if client {
		g.clients[n.ID] = n
	}
	g.peers[n.ID] = c
	g.routeMu.Lock()
	g.routes[n.ID] = c
	g.routeMu.Unlock()
	delete(g.updateStatus, n.ID)
	g.mu.Unlock()
	g.counters.connects.Add(1)
	defer func() {
		g.counters.disconnects.Add(1)
		g.mu.Lock()
		delete(g.peers, n.ID)
		g.routeMu.Lock()
		if g.routes[n.ID] == c {
			delete(g.routes, n.ID)
		}
		g.routeMu.Unlock()
		delete(g.clients, n.ID)
		for _, peer := range g.peers {
			if peer.userID != n.UserID {
				continue
			}
			select {
			case peer.out <- &protocol.Packet{Kind: "offline", From: n.ID}:
			default:
			}
		}
		if latest, exists := g.nodes[n.ID]; exists {
			latest.Online = false
			latest.LastSeen = time.Now().UTC()
			if status := g.updateStatus[n.ID]; status.State == "restarting" && !latest.UpdateUntil.IsZero() {
				latest.UpdateUntil = latest.LastSeen.Add(time.Minute)
			}
			g.nodes[n.ID] = latest
		}
		if !client {
			if err := g.commit(change{nodes: []string{n.ID}}); err != nil {
				slog.Warn("persist machine disconnect", "node", n.ID, "error", err)
			}
		}
		g.mu.Unlock()
	}()
	if err = ws.Write(handshake, websocket.MessageBinary, []byte("ready")); err != nil {
		return
	}
	go func() {
		ticker := time.NewTicker(20 * time.Second)
		defer ticker.Stop()
		defer cancel()
		defer func() { _ = ws.CloseNow() }()
		for {
			select {
			case <-ctx.Done():
				return
			case p := <-c.out:
				if p.Kind == "data" {
					c.queued.Add(-int64(len(p.Data)))
				}
				b, e := proto.Marshal(p)
				if e != nil {
					return
				}
				writeCtx, release := context.WithTimeout(ctx, 10*time.Second)
				e = ws.Write(writeCtx, websocket.MessageBinary, b)
				release()
				if e != nil {
					return
				}
			case <-ticker.C:
				pingCtx, release := context.WithTimeout(ctx, 10*time.Second)
				e := ws.Ping(pingCtx)
				release()
				if e != nil {
					return
				}
			}
		}
	}()
	// Sessions whose data was dropped for a full destination queue. Only this
	// read loop touches it, and the sender's close packet clears an entry.
	overflowed := map[string]bool{}
	for {
		_, b, e := ws.Read(ctx)
		if e != nil {
			return
		}
		var p protocol.Packet
		if proto.Unmarshal(b, &p) != nil {
			return
		}
		if p.Kind == "node.health" && p.To == "" {
			if client || !g.receiveHealth(n, c, p.Data) {
				return
			}
			continue
		}
		if p.Kind == "update.status" && p.To == "" {
			if client {
				return
			}
			if !g.receiveUpdateStatus(n, c, p.Data) {
				return
			}
			continue
		}
		switch p.Kind {
		case "open", "answer", "data", "close":
		default:
			return
		}
		if p.Session == "" || len(p.Session) > 64 {
			return
		}
		p.From = n.ID // Never trust the sender supplied by the client.
		session := p.To + "\x00" + p.Session
		if overflowed[session] {
			if p.Kind == "close" {
				delete(overflowed, session)
			} else {
				continue
			}
		}
		dest := g.route(p.To, c.userID)
		if dest == nil {
			if p.Kind == "open" && !c.enqueue(&protocol.Packet{Kind: "close", From: p.To, Session: p.Session, Data: []byte("peer offline")}) {
				return
			}
			continue
		}
		if dest.enqueue(&p) {
			if p.Kind == "data" {
				g.counters.relayPackets.Add(1)
				g.counters.relayBytes.Add(int64(len(p.Data)))
			}
			continue
		}
		// A full queue means this destination is receiving faster than its link
		// drains. Close only the affected relay session; a receiver that stops
		// reading entirely is disconnected by its writer's timeout instead.
		switch p.Kind {
		case "data":
			if len(overflowed) < 4096 {
				overflowed[session] = true
			}
			g.relayOverflow(c, &p)
		case "open":
			g.counters.relayDropped.Add(1)
			_ = c.enqueue(&protocol.Packet{Kind: "close", From: p.To, Session: p.Session, Data: []byte("relay queue full")})
		default:
			g.counters.relayDropped.Add(1)
		}
	}
}

func (g *Gateway) Close() error {
	if g.metricsCancel != nil {
		g.metricsCancel()
		g.metricsWG.Wait()
	}
	if g.updateCancel != nil {
		g.updateCancel()
		g.updateWG.Wait()
	}
	g.mu.Lock()
	g.closed = true
	peers := make([]*connection, 0, len(g.peers))
	for _, c := range g.peers {
		peers = append(peers, c)
	}
	g.mu.Unlock()
	for _, c := range peers {
		_ = c.ws.CloseNow()
	}
	g.wg.Wait()
	if g.db != nil {
		_ = g.db.Close()
	}
	_ = g.lock.Close()
	return nil
}

func URL(base, path string) string {
	return fmt.Sprintf("%s%s", strings.TrimRight(base, "/"), path)
}
