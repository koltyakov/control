// Package node implements the enrolled execution service. A node can coordinate
// work as an orchestrator and execute requested work as a worker. These are roles
// within an operation, not distinct runtime types. AI agents are optional providers.
package node

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"sync"
	"time"

	"github.com/gofrs/flock"
	"github.com/koltyakov/control/internal/clipboard"
	"github.com/koltyakov/control/internal/identity"
	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/system"
	"github.com/koltyakov/control/internal/transport"
	"github.com/koltyakov/control/internal/update"
	"github.com/koltyakov/control/internal/workgate"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type Execution struct {
	Dir   string
	Log   io.Writer
	Owner string
}

// Providers share this interface whether built in or backed by a subprocess.
type Provider interface {
	Capability() model.Capability
	Run(context.Context, json.RawMessage, Execution) (any, error)
}

type provider struct {
	cap model.Capability
	run func(context.Context, json.RawMessage, Execution) (any, error)
}

func (p provider) Capability() model.Capability { return p.cap }
func (p provider) Run(ctx context.Context, args json.RawMessage, e Execution) (any, error) {
	return p.run(ctx, args, e)
}

// Node owns a machine's capabilities, tasks, artifacts, and peer connections.
type Node struct {
	Config              Config
	Identity            *identity.Identity
	Peer                *transport.Peer
	ctx                 context.Context
	cancel              context.CancelFunc
	root                *os.Root
	providers           map[string]Provider
	mu                  sync.Mutex
	tasks               map[string]*model.Task
	cancels             map[string]context.CancelFunc
	taskChanges         map[string]chan struct{}
	slots               chan struct{}
	wg                  sync.WaitGroup
	mcpMu               sync.Mutex
	mcpSessions         map[string]*mcp.ClientSession
	artifactMu          sync.Mutex
	transfers           map[string]*sync.Mutex
	lease               *model.Lease
	lock                *flock.Flock
	closeOnce           sync.Once
	closeErr            error
	closed              bool
	activeInvocations   int
	activityMu          sync.Mutex
	activities          map[string]*activityHandle
	recentActivities    []model.Activity
	untrackedActivities int
	system              *system.Collector
	work                *workgate.Gate
	updater             *update.Updater
	shutdown            func()
	lifecycleMu         sync.Mutex
	machineState        model.MachineState
	rpaLockPath         string
	tcpListeners        chan struct{}
	readClipboard       func(context.Context) (clipboard.Value, error)
	copyClipboard       func(context.Context, string) error
	clipboardSlots      chan struct{}
}

func New(cfg Config) (*Node, error) {
	if err := cfg.defaults(); err != nil {
		return nil, err
	}
	lock := flock.New(filepath.Join(cfg.DataDir, "node.lock"))
	locked, err := lock.TryLock()
	if err != nil {
		return nil, err
	}
	if !locked {
		return nil, errors.New("node data directory is already in use")
	}
	ready := false
	defer func() {
		if !ready {
			_ = lock.Close()
		}
	}()
	id, err := identity.Load(cfg.DataDir)
	if err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(cfg.WorkDir)
	if err != nil {
		return nil, err
	}
	n := &Node{Config: cfg, Identity: id, root: root, providers: map[string]Provider{}, tasks: map[string]*model.Task{}, cancels: map[string]context.CancelFunc{}, slots: make(chan struct{}, cfg.MaxTasks), mcpSessions: map[string]*mcp.ClientSession{}, transfers: map[string]*sync.Mutex{}}
	n.lock = lock
	n.taskChanges = map[string]chan struct{}{}
	n.work = workgate.New()
	n.tcpListeners = make(chan struct{}, 32)
	n.readClipboard, n.copyClipboard = clipboard.Read, clipboard.Copy
	n.clipboardSlots = make(chan struct{}, 8)
	if err = n.loadMachineState(); err != nil {
		_ = root.Close()
		return nil, err
	}
	n.activities = map[string]*activityHandle{}
	n.system = system.New([]string{cfg.WorkDir, cfg.DataDir}, time.Duration(cfg.MetricsIntervalSeconds)*time.Second)
	// Refuse a profile change that would expose existing credentials through
	// the filesystem API. Empty installations do not need secret storage.
	if _, err = n.secretValues(); err != nil {
		_ = root.Close()
		return nil, err
	}
	if err = n.registerProviders(); err != nil {
		_ = root.Close()
		return nil, err
	}
	if err = n.loadTasks(); err != nil {
		_ = root.Close()
		return nil, err
	}
	if err = n.loadLease(); err != nil {
		_ = root.Close()
		return nil, err
	}
	n.Peer = transport.New(transport.Config{
		Gateway: cfg.Gateway, Token: cfg.Token, Identity: id, RelayOnly: cfg.RelayOnly, ICEServers: cfg.ICEServers,
		Node: model.Node{ID: id.ID, Name: cfg.Name, PublicKey: id.Public, OS: runtime.GOOS, Labels: cfg.Labels, Capabilities: n.Capabilities()},
	}, n.handle)
	ready = true
	return n, nil
}

// Register must be called before Start. The same API is used by built-ins.
func (n *Node) Register(p Provider) error {
	c := p.Capability()
	if c.Name == "" || !json.Valid(c.InputSchema) {
		return errors.New("provider requires a name and valid input schema")
	}
	if _, exists := n.providers[c.Name]; exists {
		return fmt.Errorf("duplicate capability %s", c.Name)
	}
	n.providers[c.Name] = p
	return nil
}

func (n *Node) Capabilities() []model.Capability {
	caps := make([]model.Capability, 0, len(n.providers))
	for _, p := range n.providers {
		caps = append(caps, p.Capability())
	}
	sort.Slice(caps, func(i, j int) bool { return caps[i].Name < caps[j].Name })
	return caps
}

// SetShutdown configures local service control before Start.
func (n *Node) SetShutdown(stop func()) { n.shutdown = stop }

func (n *Node) Start(ctx context.Context) error {
	n.Peer.SetCapabilities(n.Capabilities())
	n.ctx, n.cancel = context.WithCancel(ctx)
	if err := n.syncMachineState(n.ctx); err != nil {
		n.cancel()
		return err
	}
	initial, err := n.system.Refresh(n.ctx)
	if err != nil {
		slog.Warn("initial system sample", "error", err)
	}
	n.Peer.SetSystemInfo(initial)
	if err := n.Peer.Start(n.ctx); err != nil {
		n.cancel()
		return err
	}
	n.wg.Add(1)
	go func() { defer n.wg.Done(); n.system.Run(n.ctx) }()
	n.wg.Add(1)
	go func() { defer n.wg.Done(); n.runHealth(n.ctx) }()
	n.wg.Add(1)
	go func() { defer n.wg.Done(); n.runMachineState(n.ctx) }()
	if n.updater != nil {
		n.wg.Add(1)
		go func() { defer n.wg.Done(); n.updater.Run(n.ctx) }()
	}
	return nil
}

func (n *Node) Close() error {
	n.closeOnce.Do(func() {
		n.mu.Lock()
		n.closed = true
		n.mu.Unlock()
		if n.cancel != nil {
			n.cancel()
		}
		_ = n.Peer.Close()
		n.wg.Wait()
		n.mcpMu.Lock()
		var sessions []*mcp.ClientSession
		for _, s := range n.mcpSessions {
			sessions = append(sessions, s)
		}
		n.mcpMu.Unlock()
		for _, s := range sessions {
			_ = s.Close()
		}
		n.closeErr = n.root.Close()
		_ = n.lock.Close()
	})
	return n.closeErr
}

func (n *Node) authorize(ctx context.Context, caller, method string) error {
	if caller == n.Identity.ID {
		return nil
	}
	if !n.Peer.IsMember(caller) {
		return errors.New("caller is outside this fleet")
	}
	if n.Config.Allow == nil {
		return nil
	}
	patterns := append([]string{}, n.Config.Allow[caller]...)
	owner := n.Peer.Owner(caller)
	if owner != caller {
		patterns = append(patterns, n.Config.Allow[owner]...)
	}
	patterns = append(patterns, n.Config.Allow["*"]...)
	for _, pattern := range patterns {
		if ok, _ := path.Match(pattern, method); ok {
			return nil
		}
	}
	peer, err := n.Peer.Lookup(ctx, caller)
	if err != nil {
		return err
	}
	for _, pattern := range n.Config.Allow[peer.Name] {
		if ok, _ := path.Match(pattern, method); ok {
			return nil
		}
	}
	return fmt.Errorf("caller is not authorized for %s", method)
}

func (n *Node) handle(caller string, conn net.Conn) {
	// This check also precedes streaming artifacts and their grant bypass.
	if !n.Peer.IsMember(caller) {
		return
	}
	_ = conn.SetReadDeadline(time.Now().Add(15 * time.Second))
	var request model.Request
	if err := readFrame(conn, &request); err != nil {
		return
	}
	_ = conn.SetReadDeadline(time.Time{})
	ctx := n.ctx
	deadline := request.Deadline
	if deadline.IsZero() || deadline.After(time.Now().Add(24*time.Hour)) {
		deadline = time.Now().Add(24 * time.Hour)
	}
	ctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	if request.Version != model.Version {
		_ = writeFrame(conn, model.Response{Error: "unsupported protocol version"})
		return
	}
	if request.Method == "artifacts.open" {
		n.serveArtifact(ctx, caller, conn, request.Params)
		return
	}
	if request.Method == clipboard.OpenMethod || request.Method == clipboard.PasteMethod {
		n.serveClipboard(ctx, caller, conn, request.Method, request.Params)
		return
	}
	if request.Method == "tcp.open" {
		n.serveTCP(ctx, caller, conn, request.Params)
		return
	}
	if request.Method == "tcp.listen" {
		n.serveTCPListener(ctx, caller, conn, request.Params)
		return
	}
	if request.Method == "tasks.logs" {
		var q struct {
			Follow bool `json:"follow"`
		}
		if json.Unmarshal(request.Params, &q) == nil && q.Follow {
			n.serveTaskLogs(ctx, caller, conn, request.Params)
			return
		}
	}
	// A normal RPC has no client payload after the frame. EOF cancels its
	// synchronous provider without affecting separately accepted durable tasks.
	go func() { var b [1]byte; _, _ = conn.Read(b[:]); cancel() }()
	result, err := n.dispatch(ctx, caller, request.Method, request.Params)
	response := model.Response{}
	if err != nil {
		response.Error = err.Error()
	} else {
		response.Result = model.JSON(result)
	}
	_ = writeFrame(conn, response)
	slog.Debug("peer request", "caller", caller, "method", request.Method, "error", err)
}

func (n *Node) open(ctx context.Context, target, method string, params any) (net.Conn, json.RawMessage, error) {
	return n.Peer.OpenRPC(ctx, target, method, params)
}

func (n *Node) Call(ctx context.Context, target, method string, params any, result any) error {
	local := target == "" || target == n.Config.Name || target == n.Identity.ID
	if requiresAdmission(method) && (!local || method != "tasks.start") {
		workCtx, release, err := n.enterWork(ctx)
		if err != nil {
			return err
		}
		defer release()
		ctx = workCtx
	}
	if local {
		n.mu.Lock()
		if n.closed {
			n.mu.Unlock()
			return errors.New("node is closed")
		}
		n.wg.Add(1)
		n.mu.Unlock()
		defer n.wg.Done()
		ctx, cancel := context.WithCancel(ctx)
		defer cancel()
		stop := context.AfterFunc(n.ctx, cancel)
		defer stop()
		value, err := n.dispatch(ctx, n.Identity.ID, method, model.JSON(params))
		if err != nil {
			return err
		}
		if result == nil {
			return nil
		}
		return json.Unmarshal(model.JSON(value), result)
	}
	conn, data, err := n.open(ctx, target, method, params)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	if result == nil {
		return nil
	}
	return json.Unmarshal(data, result)
}

func (n *Node) dispatch(ctx context.Context, caller, method string, args json.RawMessage) (_ any, err error) {
	if err := n.authorize(ctx, caller, method); err != nil {
		return nil, err
	}
	if requiresAdmission(method) && method != "tasks.start" {
		workCtx, release, err := n.enterWork(ctx)
		if err != nil {
			return nil, err
		}
		defer release()
		ctx = workCtx
	}
	owner := n.Peer.Owner(caller)
	ctx = context.WithValue(ctx, activityContextKey{}, activityContext{owner: owner})
	var activity *activityHandle
	if trackOperation(method) {
		activity = n.beginActivity(ctx, "operation", method, owner, "")
		defer func() { activity.finish(err) }()
	}
	switch method {
	case "system.info":
		var q struct {
			Refresh bool `json:"refresh"`
		}
		if err := json.Unmarshal(args, &q); err != nil {
			return nil, err
		}
		if q.Refresh {
			return n.system.Refresh(ctx)
		}
		return n.system.Snapshot(), nil
	case "activities.list":
		q, err := decodeActivityQuery(args)
		if err != nil {
			return nil, err
		}
		return n.activitySnapshot(q), nil
	case "activities.pool":
		return n.poolActivities(ctx, caller, args)
	case "nodes.list":
		return n.Peer.Nodes(ctx)
	case "nodes.select":
		return n.selectNode(ctx, args)
	case "node.describe":
		agents, servers := []string{}, []string{}
		for name := range n.Config.Agents {
			agents = append(agents, name)
		}
		for name := range n.Config.MCP {
			servers = append(servers, name)
		}
		sort.Strings(agents)
		sort.Strings(servers)
		return map[string]any{"id": n.Identity.ID, "name": n.Config.Name, "capabilities": n.Capabilities(), "connections": n.Peer.Connections(), "sessions": n.Peer.SessionStats(), "agents": agents, "mcpServers": servers, "system": n.system.Snapshot(), "clipboard": map[string]any{"protocol": clipboard.Protocol, "methods": []string{clipboard.OpenMethod, clipboard.PasteMethod}}}, nil
	case "capabilities.list":
		return n.Capabilities(), nil
	case "tasks.start":
		var spec model.TaskSpec
		if err := json.Unmarshal(args, &spec); err != nil {
			return nil, err
		}
		if err := n.authorize(ctx, caller, spec.Capability); err != nil {
			return nil, err
		}
		return n.startTask(owner, spec)
	case "tasks.get", "tasks.cancel", "tasks.logs", "tasks.list":
		return n.taskMethod(owner, method, args)
	case "leases.acquire", "leases.renew", "leases.release", "leases.get":
		return n.leaseMethod(owner, method, args)
	case "artifacts.export", "artifacts.list", "artifacts.delete", "artifacts.pull", "artifacts.deliver", "artifacts.grant":
		return n.artifactMethod(ctx, caller, method, args)
	case "mcp.discover":
		return n.discoverMCP(ctx, args)
	default:
		p, ok := n.providers[method]
		if !ok {
			return nil, fmt.Errorf("unknown method or capability %q", method)
		}
		n.mu.Lock()
		leased := n.lease != nil && (time.Now().Before(n.lease.Expires) || n.leaseBusy())
		if !leased {
			n.activeInvocations++
		}
		n.mu.Unlock()
		if leased {
			return nil, errors.New("node is leased; invoke capabilities through tasks.start with leaseId")
		}
		defer func() { n.mu.Lock(); n.activeInvocations--; n.mu.Unlock() }()
		if method != "workflow.run" {
			activity.phase("waiting for worker slot")
			select {
			case n.slots <- struct{}{}:
				defer func() { <-n.slots }()
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		activity.phase("executing")
		return p.Run(ctx, args, Execution{Dir: n.Config.WorkDir, Log: io.Discard, Owner: owner})
	}
}

func (n *Node) selectNode(ctx context.Context, args json.RawMessage) (any, error) {
	return n.Peer.Select(ctx, args)
}
