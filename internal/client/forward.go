package client

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sort"
	"sync"

	"github.com/koltyakov/control/internal/identity"
	"github.com/koltyakov/control/internal/node"
)

const maxForwards = 32
const maxForwardConnections = 128

type ForwardSpec struct {
	Node    string `json:"node"`
	Address string `json:"address"`
	Listen  string `json:"listen,omitempty"`
}

type ForwardInfo struct {
	ID        string `json:"id"`
	Node      string `json:"node"`
	Address   string `json:"address"`
	Listen    string `json:"listen"`
	Active    int    `json:"activeConnections"`
	LastError string `json:"lastError,omitempty"`
}

type resources struct {
	ctx      context.Context
	cancel   context.CancelFunc
	mu       sync.Mutex
	closed   bool
	forwards map[string]*Forward
	done     chan struct{}
}

func newResources(ctx context.Context) *resources {
	ctx, cancel := context.WithCancel(ctx)
	r := &resources{ctx: ctx, cancel: cancel, forwards: map[string]*Forward{}, done: make(chan struct{})}
	context.AfterFunc(ctx, r.close)
	return r
}

func (r *resources) close() {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		<-r.done
		return
	}
	r.closed = true
	r.cancel()
	fs := make([]*Forward, 0, len(r.forwards))
	for _, f := range r.forwards {
		fs = append(fs, f)
	}
	r.mu.Unlock()
	for _, f := range fs {
		f.Close()
	}
	close(r.done)
}

type Forward struct {
	info        ForwardInfo
	ctx         context.Context
	cancel      context.CancelFunc
	listener    net.Listener
	mu          sync.Mutex
	connections map[net.Conn]net.Conn
	wg          sync.WaitGroup
	done        chan struct{}
	once        sync.Once
}

func (f *Forward) Info() ForwardInfo {
	f.mu.Lock()
	defer f.mu.Unlock()
	info := f.info
	info.Active = len(f.connections)
	return info
}

func (f *Forward) Done() <-chan struct{} { return f.done }

func (f *Forward) closeConnections() {
	_ = f.listener.Close()
	f.mu.Lock()
	connections := make([]net.Conn, 0, 2*len(f.connections))
	for local, remote := range f.connections {
		connections = append(connections, local)
		if remote != nil {
			connections = append(connections, remote)
		}
	}
	f.mu.Unlock()
	for _, conn := range connections {
		_ = conn.Close()
	}
}

func (f *Forward) Close() {
	f.once.Do(func() {
		f.cancel()
		f.closeConnections()
		f.wg.Wait()
		close(f.done)
	})
}

func (c Client) StartForward(ctx context.Context, spec ForwardSpec) (*Forward, error) {
	if c.resources == nil {
		return nil, errors.New("forwarding requires a client lifetime; use WithLifetime and Close")
	}
	if spec.Node == "" {
		return nil, errors.New("forwarding requires a target machine")
	}
	if _, _, err := net.SplitHostPort(spec.Address); err != nil {
		return nil, fmt.Errorf("remote address: %w", err)
	}
	// Select the backend before publishing a local port. Never create a dummy
	// remote TCP connection just to probe readiness or replay a failed stream.
	if _, err := c.backend(ctx); err != nil {
		return nil, err
	}
	if spec.Listen == "" {
		spec.Listen = "127.0.0.1:0"
	}
	r := c.resources
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed || r.ctx.Err() != nil {
		return nil, errors.New("client is closed")
	}
	if len(r.forwards) >= maxForwards {
		return nil, errors.New("port forward limit reached")
	}
	listener, err := net.Listen("tcp", spec.Listen)
	if err != nil {
		return nil, err
	}
	forwardCtx, cancel := context.WithCancel(r.ctx)
	f := &Forward{ctx: forwardCtx, cancel: cancel, listener: listener, connections: map[net.Conn]net.Conn{}, done: make(chan struct{}),
		info: ForwardInfo{ID: identity.NewID(), Node: spec.Node, Address: spec.Address, Listen: listener.Addr().String()}}
	r.forwards[f.info.ID] = f
	f.wg.Add(1)
	go f.serve(c)
	return f, nil
}

func (f *Forward) serve(c Client) {
	defer f.wg.Done()
	defer func() { go f.Close() }()
	stop := context.AfterFunc(f.ctx, f.closeConnections)
	defer stop()
	for {
		local, err := f.listener.Accept()
		if err != nil {
			if f.ctx.Err() == nil {
				f.mu.Lock()
				f.info.LastError = err.Error()
				f.mu.Unlock()
			}
			f.cancel()
			return
		}
		f.mu.Lock()
		if f.ctx.Err() != nil || len(f.connections) >= maxForwardConnections {
			f.info.LastError = "forward connection limit reached or closing"
			f.mu.Unlock()
			_ = local.Close()
			continue
		}
		f.connections[local] = nil
		f.wg.Add(1)
		f.mu.Unlock()
		go f.bridge(c, local)
	}
}

func (f *Forward) bridge(c Client, local net.Conn) {
	defer f.wg.Done()
	defer func() {
		_ = local.Close()
		f.mu.Lock()
		delete(f.connections, local)
		f.mu.Unlock()
	}()
	remote, err := c.Tunnel(f.ctx, f.info.Node, f.info.Address)
	if err != nil {
		f.mu.Lock()
		message := err.Error()
		if len(message) > 512 {
			message = message[:512]
		}
		f.info.LastError = message
		f.mu.Unlock()
		return
	}
	f.mu.Lock()
	f.connections[local] = remote
	f.mu.Unlock()
	defer func() { _ = remote.Close() }()
	node.Bridge(f.ctx, local, remote)
}

func (c Client) Forwards() []ForwardInfo {
	infos := []ForwardInfo{}
	if c.resources == nil {
		return infos
	}
	c.resources.mu.Lock()
	defer c.resources.mu.Unlock()
	for _, f := range c.resources.forwards {
		infos = append(infos, f.Info())
	}
	sort.Slice(infos, func(i, j int) bool { return infos[i].ID < infos[j].ID })
	return infos
}

func (c Client) StopForward(id string) error {
	if c.resources == nil {
		return errors.New("port forward not found in this process")
	}
	r := c.resources
	r.mu.Lock()
	f := r.forwards[id]
	r.mu.Unlock()
	if f == nil {
		return errors.New("port forward not found in this process")
	}
	f.Close()
	r.mu.Lock()
	delete(r.forwards, id)
	r.mu.Unlock()
	return nil
}
