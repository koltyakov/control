// Package gateway implements the shared directory, signaling and opaque relay.
package gateway

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
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
	Version   string     `json:"version"`
	Node      model.Node `json:"node"`
	Signature []byte     `json:"signature"`
}

type connection struct {
	userID            string
	ws                *websocket.Conn
	out               chan *protocol.Packet
	keyID             string
	health            *model.NodeHealth
	healthAt          time.Time
	lifecycleAt       time.Time
	lifecycleRevision uint64
}

type Gateway struct {
	db            *sql.DB
	users         map[string]User
	owners        map[string]string
	token         string
	path          string
	mu            sync.Mutex
	nodes         map[string]model.Node
	peers         map[string]*connection
	wg            sync.WaitGroup
	closed        bool
	restarting    bool
	lock          *flock.Flock
	superuser     string
	keys          map[string]keyRecord
	updates       *update.Repository
	options       Options
	updateStatus  map[string]update.Status
	updateCtx     context.Context
	updateCancel  context.CancelFunc
	updateWG      sync.WaitGroup
	rolloutMu     sync.Mutex
	installations map[string]installation
	metrics       *system.Collector
	metricsCancel context.CancelFunc
	metricsWG     sync.WaitGroup
	startedAt     time.Time
	machineStates map[string]model.MachineState
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
	g := &Gateway{token: token, path: filepath.Join(dir, "nodes.json"), nodes: map[string]model.Node{}, peers: map[string]*connection{}}
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
	g.updates, err = update.OpenRepository(filepath.Join(dir, "updates"))
	if err != nil {
		_ = g.db.Close()
		_ = lock.Close()
		return nil, err
	}
	for id, n := range g.nodes {
		n.Online = false
		g.nodes[id] = n
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
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].Name < nodes[j].Name })
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(nodes)
}

var validName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,62}$`)

func (g *Gateway) connect(w http.ResponseWriter, r *http.Request) {
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
	n := hello.Node
	if hello.Version != model.Version || !validName.MatchString(n.Name) || len(n.PublicKey) != ed25519.PublicKeySize || identity.ID(n.PublicKey) != n.ID {
		return
	}
	// Sign both the fresh challenge and registration metadata.
	if !ed25519.Verify(n.PublicKey, append(challenge, model.JSON(n)...), hello.Signature) {
		return
	}
	g.mu.Lock()
	p := g.authenticateLocked(bearer(r))
	if p.UserID == "" || (n.UserID != "" && n.UserID != p.UserID) || (g.owners[n.ID] != "" && g.owners[n.ID] != p.UserID) {
		g.mu.Unlock()
		return
	}
	n.UserID = p.UserID
	if g.machineStates[n.ID].Unregistered {
		g.mu.Unlock()
		_ = ws.Close(websocket.StatusPolicyViolation, "machine was unregistered")
		return
	}
	n.Disabled, n.ControlPending = g.machineStates[n.ID].Disabled, false
	registered := g.nodes[n.ID]
	n.Managed = registered.Managed && n.Software.SHA256 != "" && registered.Software.SHA256 == n.Software.SHA256
	c := &connection{ws: ws, out: make(chan *protocol.Packet, 256), keyID: p.KeyID, userID: p.UserID}
	if !g.allowInstallation(p.KeyID, n) {
		g.mu.Unlock()
		_ = ws.Close(websocket.StatusPolicyViolation, "credential does not authorize this registration")
		return
	}
	if g.closed || g.restarting {
		g.mu.Unlock()
		return
	}
	for id, existing := range g.nodes {
		if existing.UserID == n.UserID && existing.Name == n.Name && id != n.ID {
			g.mu.Unlock()
			_ = ws.Close(websocket.StatusPolicyViolation, "name already enrolled")
			return
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
	previousOwner := g.owners[n.ID]
	g.owners[n.ID] = n.UserID
	g.nodes[n.ID] = n
	if err = g.persist(); err != nil {
		if previousOwner == "" {
			delete(g.owners, n.ID)
		}
		if existed {
			g.nodes[n.ID] = previous
		} else {
			delete(g.nodes, n.ID)
		}
		g.mu.Unlock()
		return
	}
	g.peers[n.ID] = c
	delete(g.updateStatus, n.ID)
	g.mu.Unlock()
	defer func() {
		g.mu.Lock()
		delete(g.peers, n.ID)
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
			g.nodes[n.ID] = latest
		}
		_ = g.persist()
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
			if !g.receiveHealth(n, c, p.Data) {
				return
			}
			continue
		}
		if p.Kind == "update.status" && p.To == "" {
			var status update.Status
			if len(p.Data) > 8192 || json.Unmarshal(p.Data, &status) != nil {
				return
			}
			status.SeenAt = time.Now().UTC()
			g.mu.Lock()
			g.updateStatus[n.ID] = status
			g.mu.Unlock()
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
		g.mu.Lock()
		dest := g.peers[p.To]
		if dest != nil && dest.userID != c.userID {
			dest = nil
		}
		g.mu.Unlock()
		if dest == nil {
			if p.Kind == "open" {
				select {
				case c.out <- &protocol.Packet{Kind: "close", From: p.To, Session: p.Session, Data: []byte("peer offline")}:
				default:
					return
				}
			}
			continue
		}
		select {
		case dest.out <- &p:
		default:
			// Disconnect a stalled receiver rather than blocking unrelated peers.
			_ = dest.ws.CloseNow()
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
