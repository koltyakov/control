package vaultwire

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"sort"
	"sync"
	"time"

	"github.com/koltyakov/control/internal/model"
)

// Handler executes fleet operations for one connection principal. Do may be
// called concurrently and must honor ctx, which ends when the call is
// canceled, its grant is revoked or expires, or the connection closes. Close
// is called once after the connection ends and every Do has returned.
type Handler interface {
	Do(ctx context.Context, op string, args json.RawMessage) (json.RawMessage, error)
	Close()
}

// Factory creates the Handler for one connection, lazily on its first
// authorized operation. connectionCtx ends only when the caller disconnects or
// the broker stops, so resources bound to it outlive grant revocation.
type Factory func(connectionCtx context.Context) Handler

var (
	errGrantRevoked = errors.New("grant revoked")
	errGrantExpired = errors.New("grant expired")
)

type limits struct {
	maxFrame           int
	maxConnections     int
	maxInFlight        int
	maxPending         int
	maxChallenges      int
	maxFailures        int
	maxConnFailures    int
	failureWindow      time.Duration
	handshakeTimeout   time.Duration
	idleTimeout        time.Duration
	writeTimeout       time.Duration
	unapprovedLifetime time.Duration
	codeTTL            time.Duration
	challengeTTL       time.Duration
	defaultTTL         time.Duration
	maxTTL             time.Duration
	logger             *slog.Logger
}

func defaultLimits() limits {
	return limits{
		maxFrame:           1 << 20,
		maxConnections:     64,
		maxInFlight:        16,
		maxPending:         32,
		maxChallenges:      16,
		maxFailures:        10,
		maxConnFailures:    3,
		failureWindow:      time.Minute,
		handshakeTimeout:   10 * time.Second,
		idleTimeout:        2 * time.Minute,
		writeTimeout:       30 * time.Second,
		unapprovedLifetime: 15 * time.Minute,
		codeTTL:            CodeTTL,
		challengeTTL:       2 * time.Minute,
		defaultTTL:         DefaultGrantTTL,
		maxTTL:             MaxGrantTTL,
	}
}

// Serve runs the broker on ln until ctx ends, then closes ln and every
// connection and waits for handlers to close. ln must be a loopback listener;
// TCP connections from non-loopback addresses are dropped. approvalKey is the
// public half of NewApprovalKey. Serve returns nil after ctx ends.
func Serve(ctx context.Context, ln net.Listener, cert tls.Certificate, approvalKey ed25519.PublicKey, factory Factory) error {
	return serve(ctx, ln, cert, approvalKey, factory, defaultLimits())
}

type broker struct {
	lim     limits
	pin     string
	key     ed25519.PublicKey
	factory Factory
	tls     *tls.Config

	log *slog.Logger

	mu         sync.Mutex
	conns      map[string]*conn
	codes      map[string]*conn
	challenges map[string]*challenge
	failures   []time.Time
	events     []logEvent
}

// logEvent is an audit record queued under b.mu and emitted after unlock so a
// slow log sink cannot stall the broker. Records carry broker-assigned IDs and
// times only: never passphrases, codes, proofs, payloads, or caller labels.
type logEvent struct {
	msg   string
	attrs []any
}

// unlock releases b.mu and then emits queued audit records.
func (b *broker) unlock() {
	events := b.events
	b.events = nil
	b.mu.Unlock()
	for _, e := range events {
		b.log.Info(e.msg, e.attrs...)
	}
}

func (b *broker) auditLocked(msg string, attrs ...any) {
	b.events = append(b.events, logEvent{msg: msg, attrs: attrs})
}

type challenge struct {
	data   challengeData
	owner  *conn
	target string
	code   string
	ttl    time.Duration
}

func serve(ctx context.Context, ln net.Listener, cert tls.Certificate, approvalKey ed25519.PublicKey, factory Factory, lim limits) error {
	if ln == nil || factory == nil {
		return errors.New("vault broker needs a listener and factory")
	}
	if len(approvalKey) != ed25519.PublicKeySize {
		return errors.New("invalid approval public key")
	}
	if len(cert.Certificate) == 0 || cert.PrivateKey == nil {
		return errors.New("vault broker certificate is empty")
	}
	if tcp, ok := ln.Addr().(*net.TCPAddr); ok && !tcp.IP.IsLoopback() {
		return errors.New("vault broker listener must be bound to loopback")
	}
	logger := lim.logger
	if logger == nil {
		logger = slog.Default()
	}
	b := &broker{
		lim:     lim,
		log:     logger.With("component", "vault"),
		pin:     Pin(cert.Certificate[0]),
		key:     append(ed25519.PublicKey(nil), approvalKey...),
		factory: factory,
		tls: &tls.Config{
			MinVersion:   tls.VersionTLS13,
			Certificates: []tls.Certificate{cert},
			ClientAuth:   tls.NoClientCert,
			NextProtos:   []string{alpnProtocol},
		},
		conns:      map[string]*conn{},
		codes:      map[string]*conn{},
		challenges: map[string]*challenge{},
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(ctx, func() { _ = ln.Close() })
	defer stop()

	slots := make(chan struct{}, lim.maxConnections)
	var wg sync.WaitGroup
	defer wg.Wait()
	defer cancel()
	backoff := time.Duration(0)
	for {
		raw, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			if errors.Is(err, net.ErrClosed) {
				return err
			}
			// Transient accept failures such as descriptor exhaustion must not
			// stop the broker or spin.
			backoff = min(max(2*backoff, 5*time.Millisecond), time.Second)
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(backoff):
			}
			continue
		}
		backoff = 0
		select {
		case slots <- struct{}{}:
		default:
			_ = raw.Close()
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-slots }()
			b.handle(ctx, raw)
		}()
	}
}

type conn struct {
	b       *broker
	id      string
	label   string
	tc      *tls.Conn
	binding []byte
	created time.Time
	ctx     context.Context
	cancel  context.CancelFunc

	// Guarded by b.mu.
	state        string
	code         string
	codeExpires  time.Time
	grantExpires time.Time
	grantCtx     context.Context
	grantCancel  context.CancelCauseFunc
	grantTimer   *time.Timer
	granted      bool
	failures     int

	wmu sync.Mutex

	imu      sync.Mutex
	inflight map[uint64]context.CancelFunc
	wg       sync.WaitGroup

	hmu           sync.Mutex
	handler       Handler
	handlerClosed bool
}

func (b *broker) handle(parent context.Context, raw net.Conn) {
	if tcp, ok := raw.RemoteAddr().(*net.TCPAddr); ok && !tcp.IP.IsLoopback() {
		_ = raw.Close()
		return
	}
	defer func() { _ = raw.Close() }()
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	// Closing the socket unblocks the reader when the connection or broker
	// context ends.
	context.AfterFunc(ctx, func() { _ = raw.Close() })

	_ = raw.SetDeadline(time.Now().Add(b.lim.handshakeTimeout))
	tc := tls.Server(raw, b.tls)
	hctx, hcancel := context.WithTimeout(ctx, b.lim.handshakeTimeout)
	err := tc.HandshakeContext(hctx)
	hcancel()
	if err != nil {
		return
	}
	cs := tc.ConnectionState()
	if cs.Version != tls.VersionTLS13 || cs.NegotiatedProtocol != alpnProtocol {
		return
	}
	binding, err := cs.ExportKeyingMaterial(exporterLabel, nil, 32)
	if err != nil {
		return
	}
	var hello clientFrame
	if err := readFrame(tc, &hello, b.lim.maxFrame); err != nil || hello.Type != frameHello || hello.Version != protocolVersion {
		return
	}
	c := &conn{
		b: b, label: SanitizeLabel(hello.Label), tc: tc, binding: binding,
		created: time.Now(), ctx: ctx, cancel: cancel, state: StateNone,
		inflight: map[uint64]context.CancelFunc{},
	}
	b.mu.Lock()
	c.id = b.uniqueLocked(func(id string) bool { return b.conns[id] != nil }, randomHex)
	b.conns[c.id] = c
	b.unlock()
	defer c.cleanup()

	if err := c.write(serverFrame{ID: hello.ID, Type: frameHello, Version: protocolVersion, Connection: c.id}); err != nil {
		return
	}
	_ = raw.SetDeadline(time.Time{})
	unapproved := time.AfterFunc(b.lim.unapprovedLifetime, func() {
		b.mu.Lock()
		granted := c.granted
		b.unlock()
		if !granted {
			c.cancel()
		}
	})
	defer unapproved.Stop()
	c.readLoop()
}

func (c *conn) cleanup() {
	c.cancel()
	b := c.b
	b.mu.Lock()
	delete(b.conns, c.id)
	if c.code != "" && b.codes[c.code] == c {
		delete(b.codes, c.code)
	}
	if c.grantTimer != nil {
		c.grantTimer.Stop()
	}
	for id, ch := range b.challenges {
		if ch.owner == c {
			delete(b.challenges, id)
		}
	}
	switch {
	case c.state == StateActive:
		b.auditLocked("vault grant ended", "connection", c.id, "reason", "disconnected")
	case c.granted:
		// Resources bound to the connection after revoke or expiry end here.
		b.auditLocked("vault granted connection closed", "connection", c.id, "state", c.state)
	}
	b.unlock()
	c.wg.Wait()
	c.hmu.Lock()
	c.handlerClosed = true
	h := c.handler
	c.hmu.Unlock()
	if h != nil {
		h.Close()
	}
}

func (c *conn) readLoop() {
	for {
		_ = c.tc.SetReadDeadline(time.Now().Add(c.b.lim.idleTimeout))
		var f clientFrame
		if err := readFrame(c.tc, &f, c.b.lim.maxFrame); err != nil {
			return
		}
		if !c.dispatch(&f) {
			return
		}
	}
}

func (c *conn) dispatch(f *clientFrame) bool {
	switch f.Type {
	case framePing:
		return true
	case frameDo:
		c.do(f)
		return true
	case frameCancel:
		c.imu.Lock()
		if cancel := c.inflight[f.ID]; cancel != nil {
			cancel()
		}
		c.imu.Unlock()
		return true
	case frameStatus:
		c.b.mu.Lock()
		entry := c.b.entryLocked(c, time.Now())
		c.b.unlock()
		return c.reply(serverFrame{ID: f.ID, Type: frameResult, Entry: &entry})
	case frameRequest:
		c.b.mu.Lock()
		entry, err := c.b.requestLocked(c, time.Now())
		c.b.unlock()
		if err != nil {
			return c.replyError(f.ID, err)
		}
		return c.reply(serverFrame{ID: f.ID, Type: frameResult, Entry: &entry})
	case frameChallenge:
		return c.challenge(f)
	case frameAuthorize:
		return c.authorize(f)
	default:
		return c.replyError(f.ID, &Error{Code: CodeInvalidRequest, Message: "unknown frame type"})
	}
}

func (c *conn) do(f *clientFrame) {
	if f.ID == 0 || !validOp(f.Op) {
		c.replyError(f.ID, &Error{Code: CodeInvalidRequest, Message: "invalid operation request"})
		return
	}
	b := c.b
	b.mu.Lock()
	now := time.Now()
	b.refreshLocked(c, now)
	if c.state != StateActive {
		err := b.authorizationErrorLocked(c, now)
		b.unlock()
		c.replyError(f.ID, err)
		return
	}
	grantCtx := c.grantCtx
	b.unlock()

	c.imu.Lock()
	if c.inflight[f.ID] != nil {
		c.imu.Unlock()
		c.replyError(f.ID, &Error{Code: CodeInvalidRequest, Message: "duplicate request id"})
		return
	}
	if len(c.inflight) >= b.lim.maxInFlight {
		c.imu.Unlock()
		c.replyError(f.ID, &Error{Code: CodeLimitExceeded, Message: "too many operations in flight on this connection; not dispatched"})
		return
	}
	rctx, rcancel := context.WithCancel(grantCtx)
	c.inflight[f.ID] = rcancel
	c.wg.Add(1)
	c.imu.Unlock()
	args := f.Args
	if len(args) == 0 {
		args = json.RawMessage("null")
	}
	go c.run(rctx, rcancel, f.ID, f.Op, args)
}

// authorizationErrorLocked explains why c cannot dispatch, creating a pending
// code on the first attempt. Nothing is queued.
func (b *broker) authorizationErrorLocked(c *conn, now time.Time) *Error {
	switch c.state {
	case StateNone, StatePending:
		entry, err := b.requestLocked(c, now)
		if err != nil {
			return err
		}
		return &Error{Code: CodeApprovalRequired, Message: "operation not dispatched; a human must grant this connection's code", Entry: &entry}
	case StateDenied:
		return &Error{Code: CodeApprovalDenied, Message: "approval denied for this connection"}
	case StateRevoked:
		return &Error{Code: CodeGrantRevoked, Message: "grant revoked for this connection; reconnect to request approval again"}
	default:
		return &Error{Code: CodeGrantExpired, Message: "grant expired for this connection; reconnect to request approval again"}
	}
}

func (c *conn) run(ctx context.Context, cancel context.CancelFunc, id uint64, op string, args json.RawMessage) {
	defer c.wg.Done()
	defer func() {
		cancel()
		c.imu.Lock()
		delete(c.inflight, id)
		c.imu.Unlock()
	}()
	h, err := c.getHandler()
	if err != nil {
		// Handler.Do was never called.
		c.replyError(id, &Error{Code: CodeUnavailable, Outcome: OutcomeNotSent, Message: err.Error()})
		return
	}
	result, err := h.Do(ctx, op, args)
	if err != nil {
		c.replyError(id, opError(ctx, err))
		return
	}
	if len(result) == 0 {
		result = json.RawMessage("null")
	}
	if !json.Valid(result) {
		c.replyError(id, &Error{Code: CodeOpFailed, Outcome: OutcomeUnknown, Message: "handler returned invalid JSON; the operation may have completed"})
		return
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, result); err == nil {
		result = compact.Bytes()
	}
	c.reply(serverFrame{ID: id, Type: frameResult, Result: result})
}

// opError maps a handler error to the wire. Cancellation wins over any
// handler claim: once the call context ended, the outcome stays unknown. Only a
// handler's explicit not_sent outcome is propagated, and authorization codes
// are reserved for the broker so a handler cannot spoof approval state.
func opError(ctx context.Context, err error) *Error {
	if ctx.Err() != nil {
		cause := context.Cause(ctx)
		switch {
		case errors.Is(cause, errGrantRevoked):
			return &Error{Code: CodeGrantRevoked, Outcome: OutcomeUnknown, Message: "grant revoked during the operation; it may have partly run"}
		case errors.Is(cause, errGrantExpired):
			return &Error{Code: CodeGrantExpired, Outcome: OutcomeUnknown, Message: "grant expired during the operation; it may have partly run"}
		default:
			return &Error{Code: CodeCanceled, Outcome: OutcomeUnknown, Message: "operation canceled; it may have partly run"}
		}
	}
	var e *Error
	if errors.As(err, &e) {
		out := &Error{Code: CodeOpFailed, Outcome: handlerOutcome(e.Outcome), Message: truncate(e.Message)}
		if !reservedCode(e.Code) {
			out.Code = e.Code
		}
		return out
	}
	var rpc *model.RPCError
	if errors.As(err, &rpc) {
		return &Error{Code: CodeOpFailed, Outcome: handlerOutcome(rpc.Outcome), Message: truncate(rpc.Message)}
	}
	return &Error{Code: CodeOpFailed, Outcome: OutcomeUnknown, Message: truncate(err.Error())}
}

func handlerOutcome(outcome string) string {
	if outcome == OutcomeNotSent {
		return OutcomeNotSent
	}
	return OutcomeUnknown
}

// reservedCode reports codes only the broker may assign to a Do result.
func reservedCode(code string) bool {
	switch code {
	case "", CodeApprovalRequired, CodeApprovalDenied, CodeApprovalFailed, CodeGrantRevoked, CodeGrantExpired,
		CodeCanceled, CodeResultTooLarge, CodeRateLimited, CodeUnavailable:
		return true
	}
	return false
}

func truncate(s string) string {
	const limit = 4096
	if len(s) <= limit {
		return s
	}
	return s[:limit]
}

func (c *conn) getHandler() (Handler, error) {
	c.hmu.Lock()
	defer c.hmu.Unlock()
	if c.handlerClosed {
		return nil, ErrClosed
	}
	if c.handler == nil {
		h := c.b.factory(c.ctx)
		if h == nil {
			return nil, errors.New("operation handler unavailable")
		}
		c.handler = h
	}
	return c.handler, nil
}

func (c *conn) challenge(f *clientFrame) bool {
	b := c.b
	b.mu.Lock()
	resp, err := b.challengeLocked(c, f, time.Now())
	b.unlock()
	if err != nil {
		return c.replyError(f.ID, err)
	}
	return c.reply(resp)
}

func (b *broker) challengeLocked(c *conn, f *clientFrame, now time.Time) (serverFrame, *Error) {
	b.pruneFailuresLocked(now)
	if len(b.failures) >= b.lim.maxFailures {
		return serverFrame{}, &Error{Code: CodeRateLimited, Message: "too many failed approvals; try again later"}
	}
	for id, ch := range b.challenges {
		if ch.owner == c || !now.Before(ch.data.Expires) {
			delete(b.challenges, id)
		}
	}
	if len(b.challenges) >= b.lim.maxChallenges {
		return serverFrame{}, &Error{Code: CodeLimitExceeded, Message: "too many outstanding approval challenges"}
	}
	ch := &challenge{owner: c}
	var params any
	switch f.Action {
	case actionGrant:
		var p grantParams
		if err := strictJSON(f.Params, &p); err != nil {
			return serverFrame{}, &Error{Code: CodeInvalidRequest, Message: "invalid grant parameters"}
		}
		code := normalizeCode(p.Code)
		ttl := time.Duration(p.TTLMillis) * time.Millisecond
		if ttl == 0 {
			ttl = b.lim.defaultTTL
		}
		if code == "" || p.TTLMillis < 0 || ttl > b.lim.maxTTL {
			return serverFrame{}, &Error{Code: CodeInvalidRequest, Message: "invalid code or TTL above the maximum"}
		}
		target := b.codes[code]
		if target != nil {
			b.refreshLocked(target, now)
		}
		if target == nil || target.state != StatePending || target.code != code {
			return serverFrame{}, &Error{Code: CodeNotFound, Message: "no pending request with that code"}
		}
		ch.target, ch.code, ch.ttl = target.id, code, ttl
		params = grantParams{Code: code, Connection: target.id, Label: target.label, TTLMillis: ttl.Milliseconds()}
	case actionList:
		if err := strictJSON(f.Params, &struct{}{}); err != nil {
			return serverFrame{}, &Error{Code: CodeInvalidRequest, Message: "invalid list parameters"}
		}
		params = struct{}{}
	case actionRevoke:
		var p revokeParams
		if err := strictJSON(f.Params, &p); err != nil || p.ID == "" || len(p.ID) > maxIDBytes {
			return serverFrame{}, &Error{Code: CodeInvalidRequest, Message: "invalid revoke parameters"}
		}
		target := b.conns[p.ID]
		if target != nil {
			b.refreshLocked(target, now)
		}
		if target == nil || (target.state != StatePending && target.state != StateActive) {
			return serverFrame{}, &Error{Code: CodeNotFound, Message: "no pending or active grant with that id"}
		}
		ch.target = target.id
		params = revokeParams{ID: target.id, Label: target.label}
	default:
		return serverFrame{}, &Error{Code: CodeInvalidRequest, Message: "unknown approval action"}
	}
	raw, err := json.Marshal(params)
	if err != nil {
		return serverFrame{}, &Error{Code: CodeInvalidRequest, Message: "invalid parameters"}
	}
	ch.data = challengeData{
		ID:      b.uniqueLocked(func(id string) bool { return b.challenges[id] != nil }, randomHex),
		Action:  f.Action,
		Params:  raw,
		Nonce:   randomBytes(32),
		Expires: now.Add(b.lim.challengeTTL).Truncate(time.Millisecond).UTC(),
	}
	b.challenges[ch.data.ID] = ch
	data := ch.data
	return serverFrame{ID: f.ID, Type: frameResult, Challenge: &data}, nil
}

func strictJSON(raw json.RawMessage, v any) error {
	if len(raw) == 0 {
		raw = json.RawMessage("{}")
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return err
	}
	if d.More() {
		return errors.New("trailing data")
	}
	return nil
}

func (c *conn) authorize(f *clientFrame) bool {
	b := c.b
	b.mu.Lock()
	now := time.Now()
	// Challenges are single-use: consumed before verification so a failed or
	// replayed proof cannot be tried again.
	ch := b.challenges[f.Challenge]
	if ch != nil {
		delete(b.challenges, f.Challenge)
	}
	valid := ch != nil && ch.owner == c && now.Before(ch.data.Expires) &&
		len(f.Signature) == ed25519.SignatureSize &&
		ed25519.Verify(b.key, signedMessage(b.pin, c.binding, &ch.data), f.Signature)
	if !valid {
		// The failure record stays bounded: once the limit trips, further
		// failures add nothing until the window ages out. One record per trip;
		// individual unauthenticated failures are not logged.
		b.pruneFailuresLocked(now)
		if len(b.failures) < b.lim.maxFailures {
			b.failures = append(b.failures, now)
			if len(b.failures) == b.lim.maxFailures {
				b.auditLocked("vault approval rate limit reached", "window", b.lim.failureWindow)
			}
		}
		c.failures++
		keep := c.failures < b.lim.maxConnFailures
		b.unlock()
		c.replyError(f.ID, &Error{Code: CodeApprovalFailed, Message: "approval proof rejected"})
		return keep
	}
	resp, err := b.applyLocked(ch, now)
	b.unlock()
	if err != nil {
		return c.replyError(f.ID, err)
	}
	resp.ID = f.ID
	return c.reply(resp)
}

func (b *broker) applyLocked(ch *challenge, now time.Time) (serverFrame, *Error) {
	switch ch.data.Action {
	case actionList:
		return serverFrame{Type: frameResult, Entries: b.listLocked(now)}, nil
	case actionGrant:
		target := b.conns[ch.target]
		if target != nil {
			b.refreshLocked(target, now)
		}
		if target == nil || target.state != StatePending || target.code != ch.code {
			return serverFrame{}, &Error{Code: CodeNotFound, Message: "the request is no longer pending"}
		}
		b.activateLocked(target, ch.ttl, now)
		b.auditLocked("vault grant approved", "connection", target.id, "approver_connection", ch.owner.id,
			"ttl", ch.ttl, "expires", target.grantExpires.UTC())
		entry := b.entryLocked(target, now)
		return serverFrame{Type: frameResult, Entry: &entry}, nil
	case actionRevoke:
		target := b.conns[ch.target]
		if target != nil {
			b.refreshLocked(target, now)
		}
		if target == nil {
			return serverFrame{}, &Error{Code: CodeNotFound, Message: "connection is gone"}
		}
		switch target.state {
		case StateActive:
			b.endGrantLocked(target, StateRevoked, errGrantRevoked)
			b.auditLocked("vault grant revoked", "connection", target.id, "approver_connection", ch.owner.id)
		case StatePending:
			delete(b.codes, target.code)
			target.code = ""
			target.state = StateDenied
			b.auditLocked("vault request denied", "connection", target.id, "approver_connection", ch.owner.id)
		default:
			return serverFrame{}, &Error{Code: CodeNotFound, Message: "no pending or active grant with that id"}
		}
		entry := b.entryLocked(target, now)
		return serverFrame{Type: frameResult, Entry: &entry}, nil
	}
	return serverFrame{}, &Error{Code: CodeInvalidRequest, Message: "unknown approval action"}
}

func (b *broker) activateLocked(c *conn, ttl time.Duration, now time.Time) {
	delete(b.codes, c.code)
	c.code = ""
	c.state = StateActive
	c.granted = true
	c.grantExpires = now.Add(ttl)
	gctx, gcancel := context.WithCancelCause(c.ctx)
	c.grantCtx, c.grantCancel = gctx, gcancel
	c.grantTimer = time.AfterFunc(ttl, func() {
		b.mu.Lock()
		if c.state == StateActive && c.grantCtx == gctx {
			b.endGrantLocked(c, StateExpired, errGrantExpired)
		}
		b.unlock()
	})
}

func (b *broker) endGrantLocked(c *conn, state string, cause error) {
	if state == StateExpired {
		b.auditLocked("vault grant expired", "connection", c.id, "expires", c.grantExpires.UTC())
	}
	c.state = state
	if c.grantTimer != nil {
		c.grantTimer.Stop()
	}
	if c.grantCancel != nil {
		c.grantCancel(cause)
	}
}

// refreshLocked applies expiry lazily so state is correct even if a timer has
// not fired yet.
func (b *broker) refreshLocked(c *conn, now time.Time) {
	switch c.state {
	case StatePending:
		if !now.Before(c.codeExpires) {
			if b.codes[c.code] == c {
				delete(b.codes, c.code)
			}
			c.code = ""
			c.state = StateNone
		}
	case StateActive:
		if !now.Before(c.grantExpires) {
			b.endGrantLocked(c, StateExpired, errGrantExpired)
		}
	}
}

func (b *broker) entryLocked(c *conn, now time.Time) Entry {
	b.refreshLocked(c, now)
	e := Entry{ID: c.id, Label: c.label, State: c.state}
	switch c.state {
	case StatePending:
		e.Code, e.Expires = c.code, c.codeExpires
	case StateActive, StateExpired:
		e.Expires = c.grantExpires
	}
	return e
}

func (b *broker) requestLocked(c *conn, now time.Time) (Entry, *Error) {
	b.refreshLocked(c, now)
	if c.state == StateNone {
		for _, other := range b.codes {
			b.refreshLocked(other, now)
		}
		if len(b.codes) >= b.lim.maxPending {
			return Entry{}, &Error{Code: CodeLimitExceeded, Message: "too many pending approval requests"}
		}
		c.code = b.uniqueLocked(func(code string) bool { return b.codes[code] != nil }, newCode)
		c.codeExpires = now.Add(b.lim.codeTTL)
		c.state = StatePending
		b.codes[c.code] = c
	}
	return b.entryLocked(c, now), nil
}

func (b *broker) listLocked(now time.Time) []Entry {
	conns := make([]*conn, 0, len(b.conns))
	for _, c := range b.conns {
		b.refreshLocked(c, now)
		if c.state == StatePending || c.state == StateActive {
			conns = append(conns, c)
		}
	}
	sort.Slice(conns, func(i, j int) bool {
		if !conns[i].created.Equal(conns[j].created) {
			return conns[i].created.Before(conns[j].created)
		}
		return conns[i].id < conns[j].id
	})
	entries := make([]Entry, 0, len(conns))
	for _, c := range conns {
		entries = append(entries, b.entryLocked(c, now))
	}
	return entries
}

func (b *broker) pruneFailuresLocked(now time.Time) {
	keep := b.failures[:0]
	for _, t := range b.failures {
		if now.Sub(t) < b.lim.failureWindow {
			keep = append(keep, t)
		}
	}
	b.failures = keep
}

func (b *broker) uniqueLocked(taken func(string) bool, gen func() string) string {
	for {
		if v := gen(); !taken(v) {
			return v
		}
	}
}

func (c *conn) replyError(id uint64, err *Error) bool {
	return c.reply(serverFrame{ID: id, Type: frameError, Error: err})
}

// reply reports whether the connection is still usable.
func (c *conn) reply(f serverFrame) bool {
	err := c.write(f)
	if errors.Is(err, errFrameTooLarge) {
		err = c.write(serverFrame{ID: f.ID, Type: frameError, Error: &Error{Code: CodeResultTooLarge, Outcome: OutcomeUnknown, Message: "response exceeds the frame limit; the operation may have completed"}})
	}
	if err != nil {
		c.cancel()
		return false
	}
	return true
}

func (c *conn) write(f serverFrame) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if c.ctx.Err() != nil {
		return ErrClosed
	}
	_ = c.tc.SetWriteDeadline(time.Now().Add(c.b.lim.writeTimeout))
	return writeFrame(c.tc, f, c.b.lim.maxFrame)
}

// randomBytes uses crypto/rand, which aborts the process rather than return
// weak randomness.
func randomBytes(n int) []byte {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return b
}

func randomHex() string { return hex.EncodeToString(randomBytes(8)) }

// newCode returns an unambiguous 8-character code, formatted XXXX-XXXX.
func newCode() string {
	out := make([]byte, 0, 9)
	for len(out) < 9 {
		if len(out) == 4 {
			out = append(out, '-')
			continue
		}
		for _, v := range randomBytes(8) {
			// Rejection sampling keeps every symbol equally likely.
			if v < byte(len(codeAlphabet)*(256/len(codeAlphabet))) {
				out = append(out, codeAlphabet[int(v)%len(codeAlphabet)])
				break
			}
		}
	}
	return string(out)
}
