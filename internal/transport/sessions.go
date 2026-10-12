package transport

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sort"
	"sync"
	"time"

	"github.com/hashicorp/yamux"
	"github.com/koltyakov/control/internal/model"
	"github.com/pion/webrtc/v4"
)

// Lanes isolate long-lived and bulk traffic from short control RPCs. Each lane
// has its own authenticated TLS/yamux session, including on legacy receivers.
type Lane string

const (
	ControlLane     Lane = "control"
	BulkLane        Lane = "bulk"
	InteractiveLane Lane = "interactive"
	IncomingLane    Lane = "incoming"
)

type sessionKey struct {
	remote string
	lane   Lane
}

type incomingSetup struct {
	remote         string
	cancel         context.CancelFunc
	candidates     chan webrtc.ICECandidateInit
	candidateCount int
}
type sessionDial struct {
	refresh    *peerSession
	generation uint64
	done       chan struct{}
	cancel     context.CancelFunc
	waiters    int
	session    *peerSession
	err        error
}
type peerSession struct {
	draining           bool // Protected by the peer mutex; new streams use the replacement.
	mux                *yamux.Session
	link               *link
	id, remote, mode   string
	lane               Lane
	outgoing, incoming chan struct{}
}

// Called with p.mu held. A disconnected carrier is not evidence of reachability,
// even while its multiplexer is waiting for failure detection to close it.
func (s *peerSession) usableLocked() bool {
	return !s.mux.IsClosed() && (s.link == nil || (s.link.conn.ctx.Err() == nil &&
		(s.link.carrier == nil || (!s.link.carrier.closed && !s.link.carrier.disconnected))))
}

func (p *Peer) worker(fn func()) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed || p.ctx == nil || p.ctx.Err() != nil {
		return false
	}
	p.wg.Add(1)
	go func() { defer p.wg.Done(); fn() }()
	return true
}

func (p *Peer) registerSession(s *peerSession) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed || p.ctx.Err() != nil || (s.link != nil && (s.link.generation != p.networkGeneration || s.link.conn.ctx.Err() != nil)) {
		return false
	}
	p.allSessions[s.id] = s
	return true
}

func (p *Peer) serve(s *peerSession) {
	defer func() {
		_ = s.mux.Close()
		p.mu.Lock()
		delete(p.allSessions, s.id)
		key := sessionKey{s.remote, s.lane}
		if p.sessions[key] == s {
			delete(p.sessions, key)
		}
		p.mu.Unlock()
	}()
	for {
		stream, err := s.mux.AcceptStream()
		if err != nil {
			return
		}
		select {
		case s.incoming <- struct{}{}:
		default:
			abortStream(stream)
			continue
		}
		select {
		case p.streamSlots <- struct{}{}:
		default:
			<-s.incoming
			abortStream(stream)
			continue
		}
		conn := &sessionStream{Conn: stream, mode: s.mode, session: s, release: func() { <-s.incoming; <-p.streamSlots }}
		if !p.worker(func() {
			defer func() { _ = conn.Close() }()
			if p.handler != nil {
				p.handler(s.remote, conn)
			}
		}) {
			_ = conn.Close()
		}
	}
}

func abortStream(conn net.Conn) {
	_ = conn.SetDeadline(time.Now())
	_ = conn.Close()
}

type sessionStream struct {
	net.Conn
	mode    string
	session *peerSession
	once    sync.Once
	release func()
}

func (s *sessionStream) Close() error {
	s.once.Do(func() { abortStream(s.Conn); s.release() })
	return nil
}

func (p *Peer) Open(ctx context.Context, target string) (net.Conn, error) {
	return p.OpenLane(ctx, target, ControlLane)
}

func (p *Peer) OpenLane(ctx context.Context, target string, lane Lane) (net.Conn, error) {
	if lane != ControlLane && lane != BulkLane && lane != InteractiveLane {
		return nil, errors.New("invalid outgoing traffic lane")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s, err := p.session(ctx, target, lane)
	if err != nil {
		return nil, err
	}
	return p.openStream(ctx, s)
}

func (p *Peer) session(ctx context.Context, target string, lane Lane) (*peerSession, error) {
	p.mu.Lock()
	if p.closed || p.ctx == nil || p.ctx.Err() != nil {
		p.mu.Unlock()
		return nil, errors.New("peer is not running")
	}
	id := p.resolved[target]
	if id == "" {
		id = target
	}
	key := sessionKey{id, lane}
	if s := p.sessions[key]; s != nil && s.usableLocked() {
		if s.mode == "relay" && !p.cfg.RelayOnly && p.pending[key] == nil && p.directAvailableLocked(s.remote, time.Now()) {
			// Keep the relay available while a bounded setup probes a better path.
			// Only new streams switch; existing bytes are never replayed.
			if d := p.beginDialLocked(s.remote, key); d != nil {
				d.refresh, d.waiters = s, 1
				p.directRetry[s.remote] = time.Now().Add(directRetryInterval)
			}
		}
		p.mu.Unlock()
		return s, nil
	}
	d := p.pending[key]
	if d == nil {
		d = p.beginDialLocked(target, key)
		if d == nil {
			p.mu.Unlock()
			return nil, errors.New("peer setup limit reached")
		}
	}
	d.waiters++
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		p.releaseDialWaiterLocked(d)
		p.mu.Unlock()
	}()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-p.ctx.Done():
		return nil, p.ctx.Err()
	case <-d.done:
		return d.session, d.err
	}
}

func (p *Peer) releaseDialWaiterLocked(d *sessionDial) {
	d.waiters--
	if d.waiters != 0 {
		return
	}
	d.cancel()
	// A later caller must not attach to an already-cancelled setup while its
	// network cleanup is still finishing.
	for key, pending := range p.pending {
		if pending == d {
			delete(p.pending, key)
		}
	}
}

// Called with p.mu held, including while assigning a refresh before the worker starts.
func (p *Peer) beginDialLocked(target string, key sessionKey) *sessionDial {
	select {
	case p.pendingSlots[key.lane] <- struct{}{}:
	default:
		return nil
	}
	dialCtx, cancel := context.WithTimeout(p.ctx, 20*time.Second)
	d := &sessionDial{done: make(chan struct{}), cancel: cancel, generation: p.networkGeneration}
	p.pending[key] = d
	p.wg.Add(1)
	go func() { defer p.wg.Done(); defer cancel(); p.establish(dialCtx, target, key, d) }()
	return d
}

const directRetryInterval = 30 * time.Second

// Called with p.mu held. A healthy shared carrier need not repeat ICE checks.
func (p *Peer) directAvailableLocked(remote string, now time.Time) bool {
	c := p.carriers[remote]
	return (c != nil && c.pc != nil && !c.closed && !c.disconnected && c.pc.ConnectionState() == webrtc.PeerConnectionStateConnected) || !now.Before(p.directRetry[remote])
}

func (p *Peer) establish(ctx context.Context, target string, key sessionKey, d *sessionDial) {
	var s *peerSession
	var err error
	defer func() {
		p.mu.Lock()
		<-p.pendingSlots[key.lane]
		d.session, d.err = s, err
		for alias, entry := range p.pending {
			if entry == d {
				delete(p.pending, alias)
			}
		}
		close(d.done)
		p.mu.Unlock()
	}()
	p.mu.Lock()
	directoryGeneration := p.directoryGeneration
	networkGeneration := d.generation
	p.mu.Unlock()
	node, resolveErr := p.Resolve(ctx, target)
	if resolveErr != nil {
		err = resolveErr
		return
	}
	if p.cfg.Client && !node.ClientSessions {
		err = fmt.Errorf("node %s does not support client sessions; upgrade the target node before standalone CLI use", node.Name)
		return
	}
	if p.cfg.ClientOwner != nil && !node.ClientOwners {
		err = fmt.Errorf("node %s does not support concurrent client owners; upgrade the target node", node.Name)
		return
	}
	if p.cfg.RequireDelegation && !node.InstructionDelegation {
		err = errors.New("target does not support instruction-bound delegation; upgrade it before executing work")
		return
	}
	actualKey := sessionKey{node.ID, key.lane}
	p.mu.Lock()
	if ctx.Err() != nil || networkGeneration != p.networkGeneration {
		p.mu.Unlock()
		err = errors.New("network changed during peer lookup")
		return
	}
	other := p.pending[actualKey]
	if other != nil && other != d {
		other.waiters++
		p.mu.Unlock()
		defer func() {
			p.mu.Lock()
			p.releaseDialWaiterLocked(other)
			p.mu.Unlock()
		}()
		select {
		case <-ctx.Done():
			err = ctx.Err()
		case <-other.done:
			s, err = other.session, other.err
		}
		return
	}
	p.pending[actualKey] = d
	p.mu.Unlock()
	p.mu.Lock()
	channelSupport := p.cfg.Node.PeerChannels
	p.mu.Unlock()
	if d.refresh == nil && key.lane != ControlLane && node.PeerChannels && channelSupport && !p.cfg.RelayOnly {
		// Establish one carrier, then add channels without another ICE handshake.
		if _, err = p.session(ctx, node.ID, ControlLane); err != nil {
			return
		}
	}
	// Names and IDs can race on their first lookup. Reuse an already established
	// lane after resolution without replacing a session carrying active work.
	p.mu.Lock()
	cached := p.sessions[actualKey]
	usable := cached != nil && cached.usableLocked()
	p.mu.Unlock()
	if usable && d.refresh == nil {
		s = cached
		return
	}
	// Do not hold a TLS/ICE slot while waiting for another lane's setup. A
	// full batch of bulk callers must still let their control carriers start.
	select {
	case p.setups <- struct{}{}:
		defer func() { <-p.setups }()
	case <-ctx.Done():
		err = ctx.Err()
		return
	}
	p.mu.Lock()
	tryDirect := !p.cfg.RelayOnly && (d.refresh != nil || p.directAvailableLocked(node.ID, time.Now()))
	if tryDirect {
		p.directRetry[node.ID] = time.Now().Add(directRetryInterval)
	}
	p.mu.Unlock()
	if tryDirect {
		started := time.Now()
		directCtx, cancel := context.WithTimeout(ctx, p.cfg.DirectTimeout)
		s, err = p.dial(directCtx, node.ID, "webrtc", key.lane)
		cancel()
		if err != nil {
			p.stats.relayFallbacks.Add(1)
			p.log.Debug("WebRTC setup failed; retaining relay fallback", "peer", node.Name, "lane", key.lane, "elapsed", time.Since(started), "error", err)
		}
	}
	if d.refresh != nil && (s == nil || err != nil) {
		// A failed probe does not tear down or recreate the working relay.
		p.mu.Lock()
		usable := d.refresh.usableLocked() && networkGeneration == p.networkGeneration
		p.mu.Unlock()
		if usable {
			s, err = d.refresh, nil
		} else if err == nil {
			err = errors.New("relay unavailable after direct probe")
		}
		return
	}
	if s == nil || err != nil {
		if ctx.Err() != nil {
			err = ctx.Err()
			return
		}
		s, err = p.dial(ctx, node.ID, "relay", key.lane)
	}
	if err != nil {
		return
	}
	p.mu.Lock()
	changed := networkGeneration != p.networkGeneration
	p.mu.Unlock()
	if changed || ctx.Err() != nil {
		_ = s.mux.Close()
		s = nil
		err = errors.New("peer setup interrupted by cancellation or network change")
		return
	}
	if s.mode == "relay" {
		p.stats.relaySessions.Add(1)
	} else {
		p.stats.directSessions.Add(1)
	}
	if !p.registerSession(s) {
		_ = s.mux.Close()
		s = nil
		err = errors.New("peer stopped during setup")
		return
	}
	p.mu.Lock()
	if s.link != nil && s.link.generation != p.networkGeneration {
		p.mu.Unlock()
		_ = s.mux.Close()
		s = nil
		err = errors.New("network changed during peer setup")
		return
	}
	p.sessions[actualKey] = s
	var replaced *peerSession
	if d.refresh != nil {
		replaced = d.refresh
		replaced.draining = true
	}
	if s.mode == "webrtc" {
		delete(p.directRetry, node.ID)
	}
	if directoryGeneration == p.directoryGeneration {
		p.resolved[target], p.resolved[node.Name] = node.ID, node.ID
	}
	p.mu.Unlock()
	route := s.route()
	p.log.Debug("peer session established", "peer", node.Name, "lane", key.lane, "transport", s.mode, "route", route.Kind, "localCandidate", route.LocalCandidate, "remoteCandidate", route.RemoteCandidate)
	if replaced != nil {
		p.worker(func() { p.drainSession(replaced) })
	}
	if !p.worker(func() { p.serve(s) }) {
		_ = s.mux.Close()
		err = errors.New("peer stopped during setup")
		s = nil
	}
}

func (p *Peer) openStream(ctx context.Context, s *peerSession) (net.Conn, error) {
	select {
	case s.outgoing <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-p.ctx.Done():
		return nil, p.ctx.Err()
	}
	p.mu.Lock()
	draining := s.draining
	replacement := p.sessions[sessionKey{s.remote, s.lane}]
	p.mu.Unlock()
	if draining {
		<-s.outgoing
		if replacement == nil || replacement == s {
			return nil, errors.New("peer session retired")
		}
		return p.openStream(ctx, replacement)
	}
	global := p.outgoingSlots[s.lane]
	select {
	case global <- struct{}{}:
	case <-ctx.Done():
		<-s.outgoing
		return nil, ctx.Err()
	case <-p.ctx.Done():
		<-s.outgoing
		return nil, p.ctx.Err()
	}
	release := func() { <-s.outgoing; <-global }
	type result struct {
		conn net.Conn
		err  error
	}
	ready := make(chan result)
	if !p.worker(func() {
		stream, err := s.mux.OpenStream()
		var conn net.Conn
		if err == nil {
			conn = &sessionStream{Conn: stream, mode: s.mode, session: s, release: release}
		} else {
			release()
		}
		select {
		case ready <- result{conn, err}:
		case <-ctx.Done():
			if conn != nil {
				_ = conn.Close()
			}
		case <-p.ctx.Done():
			if conn != nil {
				_ = conn.Close()
			}
		}
	}) {
		release()
		return nil, errors.New("peer is closed")
	}
	select {
	case r := <-ready:
		return r.conn, r.err
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-p.ctx.Done():
		return nil, p.ctx.Err()
	}
}

func (p *Peer) drainSession(s *peerSession) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		if len(s.outgoing) == 0 && len(s.incoming) == 0 && s.mux.NumStreams() == 0 {
			_ = s.mux.Close()
			return
		}
		select {
		case <-s.mux.CloseChan():
			return
		case <-p.ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (p *Peer) SessionStats() []model.PeerSession {
	p.mu.Lock()
	var sessions []*peerSession
	for _, s := range p.allSessions {
		if s.usableLocked() {
			sessions = append(sessions, s)
		}
	}
	p.mu.Unlock()
	result := []model.PeerSession{}
	for _, s := range sessions {
		result = append(result, model.PeerSession{ID: s.id, Peer: s.remote, Lane: string(s.lane), Mode: s.mode, Outgoing: len(s.outgoing), Incoming: len(s.incoming), Route: s.route()})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result
}
