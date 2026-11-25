package vaultwire

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"
)

const (
	maxFrameSize        = 1 << 20
	clientDialTimeout   = 10 * time.Second
	clientWriteTimeout  = 30 * time.Second
	clientPingInterval  = 30 * time.Second
	challengeNonceBytes = 32
)

// Session is one connection principal. Grants never carry over to another
// Session, including a reconnect by the same caller. Methods are safe for
// concurrent use. No method retries a request.
type Session struct {
	conn    *tls.Conn
	pin     string
	id      string
	binding []byte

	wmu sync.Mutex

	mu    sync.Mutex
	next  uint64
	calls map[uint64]chan serverFrame

	done      chan struct{}
	closeOnce sync.Once
}

// Dial connects to a broker at a loopback address and verifies that its leaf
// certificate matches pin (see Pin). label is untrusted, informational caller
// metadata shown to approvers after sanitization.
func Dial(ctx context.Context, address, pin, label string) (*Session, error) {
	want, err := hex.DecodeString(strings.ToLower(strings.TrimSpace(pin)))
	if err != nil || len(want) != sha256.Size {
		return nil, errors.New("vault pin must be 64 hex characters")
	}
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return nil, fmt.Errorf("vault address: %w", err)
	}
	if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return nil, errors.New("vault broker address must be loopback")
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, clientDialTimeout)
		defer cancel()
	}
	var d net.Dialer
	raw, err := d.DialContext(ctx, "tcp", address)
	if err != nil {
		return nil, err
	}
	if tcp, ok := raw.RemoteAddr().(*net.TCPAddr); !ok || !tcp.IP.IsLoopback() {
		_ = raw.Close()
		return nil, errors.New("vault broker address must be loopback")
	}
	conn := tls.Client(raw, &tls.Config{
		MinVersion: tls.VersionTLS13,
		ServerName: serverName,
		NextProtos: []string{alpnProtocol},
		// The certificate is verified against the pin below; names, issuers,
		// and validity dates add no trust for a local broker.
		InsecureSkipVerify: true,
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				return errors.New("vault broker sent no certificate")
			}
			got := sha256.Sum256(cs.PeerCertificates[0].Raw)
			if subtle.ConstantTimeCompare(got[:], want) != 1 {
				return errors.New("vault broker certificate does not match pin")
			}
			return nil
		},
	})
	s, err := handshake(ctx, conn, hex.EncodeToString(want), label)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	go s.readLoop()
	go s.pingLoop()
	return s, nil
}

func handshake(ctx context.Context, conn *tls.Conn, pin, label string) (*Session, error) {
	if err := conn.HandshakeContext(ctx); err != nil {
		return nil, err
	}
	cs := conn.ConnectionState()
	if cs.Version != tls.VersionTLS13 || cs.NegotiatedProtocol != alpnProtocol {
		return nil, errors.New("vault broker protocol mismatch")
	}
	binding, err := cs.ExportKeyingMaterial(exporterLabel, nil, 32)
	if err != nil {
		return nil, err
	}
	deadline, _ := ctx.Deadline()
	_ = conn.SetDeadline(deadline)
	stop := context.AfterFunc(ctx, func() { _ = conn.SetDeadline(time.Unix(1, 0)) })
	defer stop()
	if err := writeFrame(conn, clientFrame{ID: 1, Type: frameHello, Version: protocolVersion, Label: SanitizeLabel(label)}, maxFrameSize); err != nil {
		return nil, err
	}
	var hello serverFrame
	if err := readFrame(conn, &hello, maxFrameSize); err != nil {
		return nil, err
	}
	if hello.Type != frameHello || hello.Version != protocolVersion || hello.Connection == "" || len(hello.Connection) > maxIDBytes {
		return nil, errors.New("vault broker protocol mismatch")
	}
	if !stop() {
		return nil, ctx.Err()
	}
	_ = conn.SetDeadline(time.Time{})
	return &Session{
		conn: conn, pin: pin, id: hello.Connection, binding: binding,
		next: 1, calls: map[uint64]chan serverFrame{}, done: make(chan struct{}),
	}, nil
}

// ID returns this connection's broker-assigned principal ID.
func (s *Session) ID() string { return s.id }

// Done is closed when the session ends.
func (s *Session) Done() <-chan struct{} { return s.done }

// Close ends the session. Its grant, if any, ends with it, and the broker
// cancels resources bound to the connection.
func (s *Session) Close() error {
	s.shutdown()
	return nil
}

func (s *Session) shutdown() {
	s.closeOnce.Do(func() {
		close(s.done)
		_ = s.conn.Close()
	})
}

func (s *Session) readLoop() {
	defer s.shutdown()
	for {
		var f serverFrame
		if err := readFrame(s.conn, &f, maxFrameSize); err != nil {
			return
		}
		s.mu.Lock()
		ch := s.calls[f.ID]
		delete(s.calls, f.ID)
		s.mu.Unlock()
		if ch != nil {
			ch <- f
		}
	}
}

// pingLoop keeps an idle but live session from reaching the broker's idle
// timeout; a dead caller stops pinging and is closed.
func (s *Session) pingLoop() {
	t := time.NewTicker(clientPingInterval)
	defer t.Stop()
	for {
		select {
		case <-s.done:
			return
		case <-t.C:
			if err := s.write(context.Background(), clientFrame{Type: framePing}); err != nil {
				return
			}
		}
	}
}

// write sends one frame. A request whose ctx ended while it waited for the
// writer is not sent, so a canceled call never reaches the broker.
func (s *Session) write(ctx context.Context, f clientFrame) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	select {
	case <-s.done:
		return ErrClosed
	default:
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	_ = s.conn.SetWriteDeadline(time.Now().Add(clientWriteTimeout))
	err := writeFrame(s.conn, f, maxFrameSize)
	if errors.Is(err, errFrameTooLarge) {
		// Nothing was written; the session stays usable.
		return &Error{Code: CodeInvalidRequest, Message: "request exceeds the frame limit"}
	}
	if err != nil {
		s.shutdown()
		return ErrClosed
	}
	return nil
}

// call sends one request and waits for its response. If ctx ends first and
// cancelRemote is set, the broker is asked to cancel the operation; the
// outcome is then unknown and the call is not retried.
func (s *Session) call(ctx context.Context, f clientFrame, cancelRemote bool) (serverFrame, error) {
	if err := ctx.Err(); err != nil {
		return serverFrame{}, err
	}
	ch := make(chan serverFrame, 1)
	s.mu.Lock()
	select {
	case <-s.done:
		s.mu.Unlock()
		return serverFrame{}, ErrClosed
	default:
	}
	s.next++
	f.ID = s.next
	s.calls[f.ID] = ch
	s.mu.Unlock()
	forget := func() {
		s.mu.Lock()
		delete(s.calls, f.ID)
		s.mu.Unlock()
	}
	if err := s.write(ctx, f); err != nil {
		forget()
		return serverFrame{}, err
	}
	select {
	case r := <-ch:
		if r.Type == frameError {
			if r.Error == nil {
				return r, &Error{Code: CodeInvalidRequest, Message: "malformed broker error"}
			}
			if r.Error.Outcome != "" && r.Error.Outcome != OutcomeNotSent {
				r.Error.Outcome = OutcomeUnknown
			}
			return r, r.Error
		}
		if r.Type != frameResult {
			return r, errors.New("vault broker protocol mismatch")
		}
		return r, nil
	case <-ctx.Done():
		forget()
		if cancelRemote {
			_ = s.write(context.Background(), clientFrame{ID: f.ID, Type: frameCancel})
		}
		return serverFrame{}, ctx.Err()
	case <-s.done:
		return serverFrame{}, ErrClosed
	}
}

// Do runs a fleet operation under this connection's grant. Without an active
// grant it returns an *Error with code approval_required carrying the pending
// code; the operation is neither dispatched nor queued. Errors from a
// dispatched operation leave its outcome unknown.
func (s *Session) Do(ctx context.Context, op string, args json.RawMessage) (json.RawMessage, error) {
	if !validOp(op) {
		return nil, &Error{Code: CodeInvalidRequest, Message: "invalid operation name"}
	}
	if len(args) == 0 {
		args = json.RawMessage("null")
	} else if !json.Valid(args) {
		return nil, &Error{Code: CodeInvalidRequest, Message: "operation arguments are not valid JSON"}
	}
	r, err := s.call(ctx, clientFrame{Type: frameDo, Op: op, Args: args}, true)
	if err != nil {
		return nil, err
	}
	return r.Result, nil
}

// Status reports this connection's state without creating a request.
func (s *Session) Status(ctx context.Context) (Entry, error) {
	return s.entry(ctx, frameStatus)
}

// Request returns this connection's pending code, creating one when the
// connection has no grant yet. It never dispatches an operation.
func (s *Session) Request(ctx context.Context) (Entry, error) {
	return s.entry(ctx, frameRequest)
}

func (s *Session) entry(ctx context.Context, kind string) (Entry, error) {
	r, err := s.call(ctx, clientFrame{Type: kind}, false)
	if err != nil {
		return Entry{}, err
	}
	if r.Entry == nil {
		return Entry{}, errors.New("vault broker protocol mismatch")
	}
	return *r.Entry, nil
}

// List returns pending and active connections after proving the approval
// key. Approvers can inspect broker-reported details here before Grant.
func (s *Session) List(ctx context.Context, key ed25519.PrivateKey) ([]Entry, error) {
	r, err := s.authorize(ctx, key, actionList, struct{}{}, func(raw json.RawMessage) error {
		return strictJSON(raw, &struct{}{})
	})
	if err != nil {
		return nil, err
	}
	if r.Entries == nil {
		return []Entry{}, nil
	}
	return r.Entries, nil
}

// Grant approves the connection holding code for ttl, or DefaultGrantTTL when
// ttl is zero. The grant ends at the absolute expiry, on Revoke, or when that
// connection closes. Prefer GrantEntry after showing the operator an entry
// from List, so the signature cannot cover a different connection.
func (s *Session) Grant(ctx context.Context, key ed25519.PrivateKey, code string, ttl time.Duration) (Entry, error) {
	return s.grant(ctx, key, code, ttl, nil)
}

// GrantEntry approves exactly the pending entry an operator previewed,
// typically from List. Before signing, it requires the broker's challenge to
// bind entry.ID and entry.Code and, when entry.Label is set, the same
// sanitized label. A mismatch leaves the challenge unsigned and unused.
func (s *Session) GrantEntry(ctx context.Context, key ed25519.PrivateKey, entry Entry, ttl time.Duration) (Entry, error) {
	if entry.ID == "" || len(entry.ID) > maxIDBytes || entry.Code == "" {
		return Entry{}, &Error{Code: CodeInvalidRequest, Message: "entry needs the pending connection id and code"}
	}
	if entry.State != "" && entry.State != StatePending {
		return Entry{}, &Error{Code: CodeInvalidRequest, Message: "entry is not pending"}
	}
	return s.grant(ctx, key, entry.Code, ttl, &entry)
}

func (s *Session) grant(ctx context.Context, key ed25519.PrivateKey, code string, ttl time.Duration, expect *Entry) (Entry, error) {
	normalized := normalizeCode(code)
	if normalized == "" {
		return Entry{}, &Error{Code: CodeInvalidRequest, Message: "invalid approval code"}
	}
	ms := ttl.Milliseconds()
	if ttl < 0 || ttl > MaxGrantTTL || (ttl > 0 && ms == 0) {
		return Entry{}, &Error{Code: CodeInvalidRequest, Message: "grant TTL must be between 1ms and 24h"}
	}
	r, err := s.authorize(ctx, key, actionGrant, grantParams{Code: normalized, TTLMillis: ms}, func(raw json.RawMessage) error {
		var p grantParams
		if err := strictJSON(raw, &p); err != nil {
			return err
		}
		maxMS := MaxGrantTTL.Milliseconds()
		if p.Code != normalized || p.Connection == "" || p.TTLMillis <= 0 || p.TTLMillis > maxMS || (ms != 0 && p.TTLMillis != ms) {
			return errors.New("broker bound different grant parameters")
		}
		if expect != nil {
			if p.Connection != expect.ID {
				return errors.New("code belongs to a different connection than the previewed entry")
			}
			if expect.Label != "" && p.Label != expect.Label {
				return errors.New("connection label differs from the previewed entry")
			}
		}
		return nil
	})
	if err != nil {
		return Entry{}, err
	}
	if r.Entry == nil {
		return Entry{}, errors.New("vault broker protocol mismatch")
	}
	if expect != nil && r.Entry.ID != expect.ID {
		return Entry{}, errors.New("vault broker granted a different connection")
	}
	return *r.Entry, nil
}

// Revoke ends the active grant for id or denies its pending request.
// Operations running under the grant are canceled; resources the handler
// bound to the connection remain until that caller disconnects.
func (s *Session) Revoke(ctx context.Context, key ed25519.PrivateKey, id string) error {
	if id == "" || len(id) > maxIDBytes {
		return &Error{Code: CodeInvalidRequest, Message: "invalid grant id"}
	}
	_, err := s.authorize(ctx, key, actionRevoke, revokeParams{ID: id}, func(raw json.RawMessage) error {
		var p revokeParams
		if err := strictJSON(raw, &p); err != nil {
			return err
		}
		if p.ID != id {
			return errors.New("broker bound a different revoke target")
		}
		return nil
	})
	return err
}

// authorize fetches a single-use challenge, checks that the broker bound the
// requested action and parameters, and signs it for this TLS session only.
func (s *Session) authorize(ctx context.Context, key ed25519.PrivateKey, action string, params any, check func(json.RawMessage) error) (serverFrame, error) {
	if len(key) != ed25519.PrivateKeySize {
		return serverFrame{}, errors.New("invalid approval key")
	}
	raw, err := json.Marshal(params)
	if err != nil {
		return serverFrame{}, err
	}
	r, err := s.call(ctx, clientFrame{Type: frameChallenge, Action: action, Params: raw}, false)
	if err != nil {
		return serverFrame{}, err
	}
	ch := r.Challenge
	if ch == nil || ch.ID == "" || len(ch.ID) > maxIDBytes || ch.Action != action || len(ch.Nonce) != challengeNonceBytes || !json.Valid(ch.Params) {
		return serverFrame{}, errors.New("vault broker sent an invalid challenge")
	}
	if err := check(bytes.Clone(ch.Params)); err != nil {
		return serverFrame{}, fmt.Errorf("vault challenge rejected: %w", err)
	}
	sig := ed25519.Sign(key, signedMessage(s.pin, s.binding, ch))
	return s.call(ctx, clientFrame{Type: frameAuthorize, Challenge: ch.ID, Signature: sig}, false)
}
