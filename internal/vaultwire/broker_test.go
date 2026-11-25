package vaultwire

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/koltyakov/control/internal/model"
)

var (
	approvalOnce sync.Once
	approvalPriv ed25519.PrivateKey
	approvalErr  error
)

// testApprovalKey derives one synthetic passphrase key per test binary; the
// Argon2id cost makes per-test derivation needlessly slow.
func testApprovalKey(t *testing.T) ed25519.PrivateKey {
	t.Helper()
	approvalOnce.Do(func() {
		pass := []byte(randomHex() + randomHex())
		salt, err := NewApprovalSalt()
		if err != nil {
			approvalErr = err
			return
		}
		approvalPriv, approvalErr = NewApprovalKey(pass, salt)
	})
	if approvalErr != nil {
		t.Fatal(approvalErr)
	}
	return approvalPriv
}

func otherKey(t *testing.T) ed25519.PrivateKey {
	t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

type testHandler struct {
	connCtx context.Context
	closed  chan struct{}
	once    sync.Once
	calls   atomic.Int32
	do      func(ctx context.Context, op string, args json.RawMessage) (json.RawMessage, error)
}

func (h *testHandler) Do(ctx context.Context, op string, args json.RawMessage) (json.RawMessage, error) {
	h.calls.Add(1)
	if h.do != nil {
		return h.do(ctx, op, args)
	}
	return args, nil
}

func (h *testHandler) Close() { h.once.Do(func() { close(h.closed) }) }

type testBroker struct {
	addr     string
	pin      string
	cancel   context.CancelFunc
	done     chan error
	mu       sync.Mutex
	handlers []*testHandler
	do       func(ctx context.Context, op string, args json.RawMessage) (json.RawMessage, error)
}

func (b *testBroker) handlerList() []*testHandler {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]*testHandler(nil), b.handlers...)
}

func startBroker(t *testing.T, lim limits, do func(context.Context, string, json.RawMessage) (json.RawMessage, error)) *testBroker {
	t.Helper()
	_, brokerKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := NewCertificate(brokerKey)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	if lim.logger == nil {
		lim.logger = slog.New(slog.DiscardHandler)
	}
	ctx, cancel := context.WithCancel(context.Background())
	tb := &testBroker{addr: ln.Addr().String(), pin: Pin(cert.Certificate[0]), cancel: cancel, done: make(chan error, 1), do: do}
	factory := func(connCtx context.Context) Handler {
		h := &testHandler{connCtx: connCtx, closed: make(chan struct{}), do: tb.do}
		tb.mu.Lock()
		tb.handlers = append(tb.handlers, h)
		tb.mu.Unlock()
		return h
	}
	pub := testApprovalKey(t).Public().(ed25519.PublicKey)
	go func() { tb.done <- serve(ctx, ln, cert, pub, factory, lim) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-tb.done:
			if err != nil {
				t.Errorf("serve: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("serve did not stop")
		}
	})
	return tb
}

func (b *testBroker) dial(t *testing.T, label string) *Session {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s, err := Dial(ctx, b.addr, b.pin, label)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func testCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func errCode(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return ""
}

func waitClosed(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatalf("%s did not close", what)
	}
}

func grantSession(t *testing.T, b *testBroker, agent *Session, ttl time.Duration) Entry {
	t.Helper()
	ctx := testCtx(t)
	pending, err := agent.Request(ctx)
	if err != nil {
		t.Fatal(err)
	}
	approver := b.dial(t, "approver")
	entry, err := approver.Grant(ctx, testApprovalKey(t), pending.Code, ttl)
	if err != nil {
		t.Fatal(err)
	}
	return entry
}

func TestFirstOperationRequiresApprovalWithoutDispatch(t *testing.T) {
	b := startBroker(t, defaultLimits(), nil)
	ctx := testCtx(t)
	agent := b.dial(t, "agent \x1b[31mred\u202e\u200b  tab\tend")

	_, err := agent.Do(ctx, "exec.run", json.RawMessage(`{"cmd":"true"}`))
	pending, ok := ApprovalRequired(err)
	if !ok {
		t.Fatalf("Do error = %v, want approval_required", err)
	}
	if pending.State != StatePending || normalizeCode(pending.Code) != pending.Code || pending.ID != agent.ID() {
		t.Fatalf("pending entry = %+v", pending)
	}
	if left := time.Until(pending.Expires); left <= 4*time.Minute || left > CodeTTL {
		t.Fatalf("code expiry in %v, want about 5m", left)
	}
	if pending.Label != "agent [31mred tab end" {
		t.Fatalf("label = %q", pending.Label)
	}
	_, err = agent.Do(ctx, "exec.run", nil)
	again, _ := ApprovalRequired(err)
	if again.Code != pending.Code {
		t.Fatalf("second attempt code %q, want %q", again.Code, pending.Code)
	}
	for _, read := range []func(context.Context) (Entry, error){agent.Request, agent.Status} {
		e, err := read(ctx)
		if err != nil || e.Code != pending.Code || e.State != StatePending {
			t.Fatalf("entry = %+v, %v", e, err)
		}
	}
	if n := len(b.handlerList()); n != 0 {
		t.Fatalf("factory called %d times before approval", n)
	}

	approver := b.dial(t, "approver")
	key := testApprovalKey(t)
	list, err := approver.List(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].ID != agent.ID() || list[0].Code != pending.Code || list[0].Label != pending.Label {
		t.Fatalf("list = %+v", list)
	}
	granted, err := approver.Grant(ctx, key, strings.ToLower(strings.ReplaceAll(pending.Code, "-", "")), 0)
	if err != nil {
		t.Fatal(err)
	}
	if granted.State != StateActive || granted.ID != agent.ID() || granted.Code != "" {
		t.Fatalf("granted = %+v", granted)
	}
	if left := time.Until(granted.Expires); left < DefaultGrantTTL-time.Minute || left > DefaultGrantTTL {
		t.Fatalf("grant expiry in %v, want 24h", left)
	}
	for range 2 {
		out, err := agent.Do(ctx, "echo", json.RawMessage(`{"a": 1}`))
		if err != nil || string(out) != `{"a":1}` {
			t.Fatalf("Do = %s, %v", out, err)
		}
	}
	if hs := b.handlerList(); len(hs) != 1 || hs[0].calls.Load() != 2 {
		t.Fatalf("handlers = %d", len(hs))
	}
	if _, err := approver.Grant(ctx, key, pending.Code, 0); errCode(err) != CodeNotFound {
		t.Fatalf("second grant error = %v", err)
	}
	if _, err := approver.Do(ctx, "echo", nil); errCode(err) != CodeApprovalRequired {
		t.Fatalf("approver connection is a separate principal: %v", err)
	}
}

func TestApprovalProofIsProtectedAndSingleUse(t *testing.T) {
	lim := defaultLimits()
	lim.maxConnFailures = 100
	b := startBroker(t, lim, nil)
	ctx := testCtx(t)
	key := testApprovalKey(t)
	agent := b.dial(t, "agent")
	pending, err := agent.Request(ctx)
	if err != nil {
		t.Fatal(err)
	}
	a := b.dial(t, "approver a")
	if _, err := a.Grant(ctx, otherKey(t), pending.Code, 0); errCode(err) != CodeApprovalFailed {
		t.Fatalf("wrong key grant = %v", err)
	}
	if _, err := a.List(ctx, otherKey(t)); errCode(err) != CodeApprovalFailed {
		t.Fatalf("wrong key list = %v", err)
	}
	if err := a.Revoke(ctx, otherKey(t), agent.ID()); errCode(err) != CodeApprovalFailed {
		t.Fatalf("wrong key revoke = %v", err)
	}
	if st, _ := agent.Status(ctx); st.State != StatePending {
		t.Fatalf("status after failures = %+v", st)
	}

	challengeFor := func(s *Session) *challengeData {
		r, err := s.call(ctx, clientFrame{Type: frameChallenge, Action: actionList, Params: json.RawMessage(`{}`)}, false)
		if err != nil || r.Challenge == nil {
			t.Fatalf("challenge: %+v %v", r, err)
		}
		return r.Challenge
	}
	authorize := func(s *Session, id string, sig []byte) error {
		_, err := s.call(ctx, clientFrame{Type: frameAuthorize, Challenge: id, Signature: sig}, false)
		return err
	}

	// A valid proof succeeds once; replaying it fails.
	ch := challengeFor(a)
	sig := ed25519.Sign(key, signedMessage(a.pin, a.binding, ch))
	if err := authorize(a, ch.ID, sig); err != nil {
		t.Fatal(err)
	}
	if err := authorize(a, ch.ID, sig); errCode(err) != CodeApprovalFailed {
		t.Fatalf("replay = %v", err)
	}

	// A proof is bound to the TLS session and challenge owner.
	bSession := b.dial(t, "approver b")
	ch = challengeFor(a)
	if err := authorize(bSession, ch.ID, ed25519.Sign(key, signedMessage(bSession.pin, bSession.binding, ch))); errCode(err) != CodeApprovalFailed {
		t.Fatalf("cross-connection proof = %v", err)
	}
	ch = challengeFor(a)
	if err := authorize(a, ch.ID, ed25519.Sign(key, signedMessage(a.pin, bSession.binding, ch))); errCode(err) != CodeApprovalFailed {
		t.Fatalf("wrong channel binding = %v", err)
	}
	ch = challengeFor(a)
	if err := authorize(a, ch.ID, ed25519.Sign(key, signedMessage(strings.Repeat("0", 64), a.binding, ch))); errCode(err) != CodeApprovalFailed {
		t.Fatalf("wrong broker pin = %v", err)
	}
	// Changing the bound action or parameters invalidates the signature.
	ch = challengeFor(a)
	forged := *ch
	forged.Params = json.RawMessage(`{"id":"x"}`)
	if err := authorize(a, ch.ID, ed25519.Sign(key, signedMessage(a.pin, a.binding, &forged))); errCode(err) != CodeApprovalFailed {
		t.Fatalf("forged params = %v", err)
	}
	// A newer challenge replaces the connection's previous one.
	first := challengeFor(a)
	_ = challengeFor(a)
	if err := authorize(a, first.ID, ed25519.Sign(key, signedMessage(a.pin, a.binding, first))); errCode(err) != CodeApprovalFailed {
		t.Fatalf("superseded challenge = %v", err)
	}
	if st, _ := agent.Status(ctx); st.State != StatePending {
		t.Fatalf("agent state changed: %+v", st)
	}
}

func TestRepeatedFailuresCloseConnectionAndRateLimit(t *testing.T) {
	lim := defaultLimits()
	lim.maxFailures = 4
	b := startBroker(t, lim, nil)
	ctx := testCtx(t)
	a := b.dial(t, "guesser")
	for range lim.maxConnFailures {
		if _, err := a.List(ctx, otherKey(t)); errCode(err) != CodeApprovalFailed {
			t.Fatalf("list = %v", err)
		}
	}
	waitClosed(t, a.Done(), "failing approver session")
	c := b.dial(t, "second guesser")
	if _, err := c.List(ctx, otherKey(t)); errCode(err) != CodeApprovalFailed {
		t.Fatalf("list = %v", err)
	}
	if _, err := c.List(ctx, testApprovalKey(t)); errCode(err) != CodeRateLimited {
		t.Fatalf("rate limit = %v", err)
	}
}

func TestRevokeCancelsOperationsButKeepsConnectionContext(t *testing.T) {
	started := make(chan struct{}, 1)
	b := startBroker(t, defaultLimits(), func(ctx context.Context, op string, args json.RawMessage) (json.RawMessage, error) {
		if op == "block" {
			started <- struct{}{}
			<-ctx.Done()
			return nil, ctx.Err()
		}
		return args, nil
	})
	ctx := testCtx(t)
	agent := b.dial(t, "agent")
	grantSession(t, b, agent, 0)

	errc := make(chan error, 1)
	go func() {
		_, err := agent.Do(ctx, "block", nil)
		errc <- err
	}()
	<-started
	approver := b.dial(t, "approver")
	if err := approver.Revoke(ctx, testApprovalKey(t), agent.ID()); err != nil {
		t.Fatal(err)
	}
	if err := <-errc; errCode(err) != CodeGrantRevoked {
		t.Fatalf("in-flight Do = %v", err)
	}
	h := b.handlerList()[0]
	if h.connCtx.Err() != nil {
		t.Fatal("connection context canceled by revoke")
	}
	if _, err := agent.Do(ctx, "echo", nil); errCode(err) != CodeGrantRevoked {
		t.Fatalf("Do after revoke = %v", err)
	}
	if e, err := agent.Request(ctx); err != nil || e.State != StateRevoked || e.Code != "" {
		t.Fatalf("request after revoke = %+v, %v", e, err)
	}
	if list, err := approver.List(ctx, testApprovalKey(t)); err != nil || len(list) != 0 {
		t.Fatalf("list after revoke = %+v, %v", list, err)
	}
	if err := approver.Revoke(ctx, testApprovalKey(t), agent.ID()); errCode(err) != CodeNotFound {
		t.Fatalf("second revoke = %v", err)
	}
	_ = agent.Close()
	waitClosed(t, h.connCtx.Done(), "connection context")
	waitClosed(t, h.closed, "handler")
}

func TestGrantExpiryCancelsOperations(t *testing.T) {
	started := make(chan struct{}, 1)
	b := startBroker(t, defaultLimits(), func(ctx context.Context, _ string, _ json.RawMessage) (json.RawMessage, error) {
		started <- struct{}{}
		<-ctx.Done()
		return nil, ctx.Err()
	})
	ctx := testCtx(t)
	agent := b.dial(t, "agent")
	entry := grantSession(t, b, agent, 300*time.Millisecond)
	if left := time.Until(entry.Expires); left > 300*time.Millisecond || left <= 0 {
		t.Fatalf("expiry in %v", left)
	}
	if _, err := agent.Do(ctx, "block", nil); errCode(err) != CodeGrantExpired {
		t.Fatalf("Do = %v", err)
	}
	<-started
	if e, _ := agent.Status(ctx); e.State != StateExpired {
		t.Fatalf("status = %+v", e)
	}
	if _, err := agent.Do(ctx, "block", nil); errCode(err) != CodeGrantExpired {
		t.Fatalf("Do after expiry = %v", err)
	}
}

func TestGrantTTLBounds(t *testing.T) {
	b := startBroker(t, defaultLimits(), nil)
	ctx := testCtx(t)
	agent := b.dial(t, "agent")
	pending, _ := agent.Request(ctx)
	approver := b.dial(t, "approver")
	key := testApprovalKey(t)
	for _, ttl := range []time.Duration{-time.Second, MaxGrantTTL + time.Second, time.Microsecond} {
		if _, err := approver.Grant(ctx, key, pending.Code, ttl); errCode(err) != CodeInvalidRequest {
			t.Fatalf("ttl %v: %v", ttl, err)
		}
	}
	params := json.RawMessage(`{"code":"` + pending.Code + `","ttl_ms":` + "90000000" + `}`)
	if _, err := approver.call(ctx, clientFrame{Type: frameChallenge, Action: actionGrant, Params: params}, false); errCode(err) != CodeInvalidRequest {
		t.Fatalf("server ttl bound = %v", err)
	}
	if _, err := approver.Grant(ctx, key, "ZZZZ-ZZZZ", 0); errCode(err) != CodeNotFound {
		t.Fatalf("unknown code = %v", err)
	}
	if e, err := approver.Grant(ctx, key, pending.Code, time.Hour); err != nil || time.Until(e.Expires) > time.Hour {
		t.Fatalf("grant = %+v, %v", e, err)
	}
}

func TestReconnectNeedsNewApproval(t *testing.T) {
	b := startBroker(t, defaultLimits(), nil)
	ctx := testCtx(t)
	agent := b.dial(t, "agent")
	grantSession(t, b, agent, 0)
	if _, err := agent.Do(ctx, "echo", nil); err != nil {
		t.Fatal(err)
	}
	old := agent.ID()
	_ = agent.Close()
	waitClosed(t, b.handlerList()[0].closed, "handler")

	again := b.dial(t, "agent")
	if again.ID() == old {
		t.Fatal("connection id reused")
	}
	if _, err := again.Do(ctx, "echo", nil); errCode(err) != CodeApprovalRequired {
		t.Fatalf("reconnected Do = %v", err)
	}
	approver := b.dial(t, "approver")
	if err := approver.Revoke(ctx, testApprovalKey(t), old); errCode(err) != CodeNotFound {
		t.Fatalf("revoke closed connection = %v", err)
	}
}

func TestPendingCodeExpiresAndDeny(t *testing.T) {
	lim := defaultLimits()
	lim.codeTTL = 200 * time.Millisecond
	b := startBroker(t, lim, nil)
	ctx := testCtx(t)
	key := testApprovalKey(t)
	agent := b.dial(t, "agent")
	first, _ := agent.Request(ctx)
	time.Sleep(300 * time.Millisecond)
	if e, _ := agent.Status(ctx); e.State != StateNone || e.Code != "" {
		t.Fatalf("status after code expiry = %+v", e)
	}
	approver := b.dial(t, "approver")
	if _, err := approver.Grant(ctx, key, first.Code, 0); errCode(err) != CodeNotFound {
		t.Fatalf("expired code grant = %v", err)
	}
	_, err := agent.Do(ctx, "echo", nil)
	second, ok := ApprovalRequired(err)
	if !ok || second.Code == first.Code {
		t.Fatalf("new request = %+v, %v", second, err)
	}
	if err := approver.Revoke(ctx, key, agent.ID()); err != nil {
		t.Fatal(err)
	}
	if _, err := agent.Do(ctx, "echo", nil); errCode(err) != CodeApprovalDenied {
		t.Fatalf("Do after deny = %v", err)
	}
	if n := len(b.handlerList()); n != 0 {
		t.Fatalf("factory called %d times", n)
	}
}

func TestPendingRequestLimit(t *testing.T) {
	lim := defaultLimits()
	lim.maxPending = 1
	b := startBroker(t, lim, nil)
	ctx := testCtx(t)
	if _, err := b.dial(t, "one").Request(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := b.dial(t, "two").Do(ctx, "echo", nil); errCode(err) != CodeLimitExceeded {
		t.Fatalf("second pending = %v", err)
	}
}

func TestCallerCancelAndInFlightLimit(t *testing.T) {
	lim := defaultLimits()
	lim.maxInFlight = 1
	started := make(chan struct{}, 2)
	canceled := make(chan error, 2)
	b := startBroker(t, lim, func(ctx context.Context, _ string, _ json.RawMessage) (json.RawMessage, error) {
		started <- struct{}{}
		<-ctx.Done()
		canceled <- context.Cause(ctx)
		return nil, ctx.Err()
	})
	ctx := testCtx(t)
	agent := b.dial(t, "agent")
	grantSession(t, b, agent, 0)

	callCtx, cancel := context.WithCancel(ctx)
	errc := make(chan error, 1)
	go func() {
		_, err := agent.Do(callCtx, "block", nil)
		errc <- err
	}()
	<-started
	if _, err := agent.Do(ctx, "block", nil); errCode(err) != CodeLimitExceeded {
		t.Fatalf("second in-flight = %v", err)
	}
	cancel()
	if err := <-errc; !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled Do = %v", err)
	}
	if cause := <-canceled; !errors.Is(cause, context.Canceled) {
		t.Fatalf("handler cause = %v", cause)
	}
	if n := b.handlerList()[0].calls.Load(); n != 1 {
		t.Fatalf("handler calls = %d", n)
	}
}

func TestHandlerResultsAndErrors(t *testing.T) {
	lim := defaultLimits()
	lim.maxFrame = 4096
	b := startBroker(t, lim, func(_ context.Context, op string, _ json.RawMessage) (json.RawMessage, error) {
		switch op {
		case "big":
			return json.Marshal(strings.Repeat("x", 8192))
		case "custom":
			return nil, &Error{Code: "busy", Message: "try later"}
		case "reserved":
			return nil, &Error{Code: CodeGrantRevoked, Message: "spoof"}
		case "invalid":
			return json.RawMessage(`{`), nil
		case "empty":
			return nil, nil
		}
		return nil, errors.New("boom")
	})
	ctx := testCtx(t)
	agent := b.dial(t, "agent")
	grantSession(t, b, agent, 0)
	for op, want := range map[string]string{"big": CodeResultTooLarge, "custom": "busy", "reserved": CodeOpFailed, "invalid": CodeOpFailed, "other": CodeOpFailed} {
		if _, err := agent.Do(ctx, op, nil); errCode(err) != want {
			t.Fatalf("%s = %v, want %s", op, err, want)
		}
	}
	if out, err := agent.Do(ctx, "empty", nil); err != nil || string(out) != "null" {
		t.Fatalf("empty = %s, %v", out, err)
	}
	if _, err := agent.Do(ctx, "bad op", nil); errCode(err) != CodeInvalidRequest {
		t.Fatalf("bad op = %v", err)
	}
	if _, err := agent.Do(ctx, "echo", json.RawMessage(`{`)); errCode(err) != CodeInvalidRequest {
		t.Fatalf("bad args = %v", err)
	}
	// An oversized request frame closes the connection without dispatch.
	calls := b.handlerList()[0].calls.Load()
	if _, err := agent.Do(ctx, "echo", json.RawMessage(`"`+strings.Repeat("y", 8192)+`"`)); !errors.Is(err, ErrClosed) {
		t.Fatalf("oversized frame = %v", err)
	}
	waitClosed(t, agent.Done(), "session after oversized frame")
	if b.handlerList()[0].calls.Load() != calls {
		t.Fatal("oversized frame dispatched")
	}
}

func TestDialVerifiesPinAndLoopback(t *testing.T) {
	b := startBroker(t, defaultLimits(), nil)
	ctx := testCtx(t)
	if _, err := Dial(ctx, b.addr, strings.Repeat("ab", 32), "x"); err == nil || !strings.Contains(err.Error(), "pin") {
		t.Fatalf("wrong pin = %v", err)
	}
	if _, err := Dial(ctx, b.addr, "short", "x"); err == nil {
		t.Fatal("malformed pin accepted")
	}
	if _, err := Dial(ctx, "192.0.2.1:1", b.pin, "x"); err == nil || !strings.Contains(err.Error(), "loopback") {
		t.Fatalf("non-loopback = %v", err)
	}
	if s, err := Dial(ctx, b.addr, strings.ToUpper(b.pin), "x"); err != nil {
		t.Fatalf("uppercase pin = %v", err)
	} else {
		_ = s.Close()
	}
}

type fakeListener struct{ addr net.Addr }

func (l fakeListener) Accept() (net.Conn, error) { return nil, net.ErrClosed }
func (l fakeListener) Close() error              { return nil }
func (l fakeListener) Addr() net.Addr            { return l.addr }

func TestServeValidatesInputs(t *testing.T) {
	_, brokerKey, _ := ed25519.GenerateKey(rand.Reader)
	cert, err := NewCertificate(brokerKey)
	if err != nil {
		t.Fatal(err)
	}
	pub := otherKey(t).Public().(ed25519.PublicKey)
	factory := func(context.Context) Handler { return nil }
	ctx := testCtx(t)
	public := fakeListener{addr: &net.TCPAddr{IP: net.IPv4(192, 0, 2, 1), Port: 1}}
	if err := Serve(ctx, public, cert, pub, factory); err == nil || !strings.Contains(err.Error(), "loopback") {
		t.Fatalf("public listener = %v", err)
	}
	loop := fakeListener{addr: &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1}}
	if err := Serve(ctx, loop, cert, pub[:5], factory); err == nil {
		t.Fatal("short approval key accepted")
	}
	if err := Serve(ctx, loop, cert, pub, nil); err == nil {
		t.Fatal("nil factory accepted")
	}
}

func TestIdleHandshakeAndUnapprovedLifetimes(t *testing.T) {
	lim := defaultLimits()
	lim.handshakeTimeout = 200 * time.Millisecond
	lim.idleTimeout = 300 * time.Millisecond
	b := startBroker(t, lim, nil)

	raw, err := net.Dial("tcp", b.addr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = raw.Close() }()
	_ = raw.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := raw.Read(make([]byte, 1)); err == nil || errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("silent raw connection read = %v", err)
	}

	idle := b.dial(t, "idle")
	waitClosed(t, idle.Done(), "idle session")

	lim = defaultLimits()
	lim.unapprovedLifetime = 700 * time.Millisecond
	b = startBroker(t, lim, nil)
	waiting := b.dial(t, "waiting")
	granted := b.dial(t, "granted")
	grantSession(t, b, granted, 0)
	waitClosed(t, waiting.Done(), "unapproved session")
	if _, err := granted.Status(testCtx(t)); err != nil {
		t.Fatalf("granted session closed: %v", err)
	}
}

func TestServeShutdownClosesConnectionsAndHandlers(t *testing.T) {
	b := startBroker(t, defaultLimits(), nil)
	ctx := testCtx(t)
	agent := b.dial(t, "agent")
	grantSession(t, b, agent, 0)
	if _, err := agent.Do(ctx, "echo", nil); err != nil {
		t.Fatal(err)
	}
	b.cancel()
	select {
	case err := <-b.done:
		if err != nil {
			t.Fatal(err)
		}
		b.done <- nil
	case <-time.After(5 * time.Second):
		t.Fatal("serve did not return")
	}
	waitClosed(t, b.handlerList()[0].closed, "handler")
	waitClosed(t, agent.Done(), "session")
}

func TestNewApprovalKey(t *testing.T) {
	salt := bytes.Repeat([]byte{7}, MinApprovalSaltSize)
	pass := []byte("synthetic passphrase " + randomHex())
	a, err := NewApprovalKey(pass, salt)
	if err != nil {
		t.Fatal(err)
	}
	again, _ := NewApprovalKey(pass, salt)
	if !bytes.Equal(a, again) {
		t.Fatal("derivation is not deterministic")
	}
	other, _ := NewApprovalKey(pass, append([]byte{8}, salt[1:]...))
	if bytes.Equal(a, other) {
		t.Fatal("salt did not change the key")
	}
	if _, err := NewApprovalKey(pass, salt[:MinApprovalSaltSize-1]); err == nil {
		t.Fatal("short salt accepted")
	}
	if _, err := NewApprovalKey(nil, salt); err == nil {
		t.Fatal("empty passphrase accepted")
	}
}

func TestHelpers(t *testing.T) {
	for in, want := range map[string]string{
		"abcd-efgh": "ABCD-EFGH", "ABCDEFGH": "ABCD-EFGH", "ab cd ef gh": "ABCD-EFGH",
		"ABCD-EFG": "", "ABCD-EFGHJ": "", "ABCD-EFG1": "", "ABCD-EFGO": "",
	} {
		if got := normalizeCode(in); got != want {
			t.Errorf("normalizeCode(%q) = %q, want %q", in, got, want)
		}
	}
	for range 100 {
		if c := newCode(); normalizeCode(c) != c {
			t.Fatalf("generated code %q", c)
		}
	}
	for in, want := range map[string]string{
		"": "unnamed", "\x00\u202e": "unnamed", "  a \n b  ": "a b", "\xff ok": "ok",
		strings.Repeat("z", 100): strings.Repeat("z", maxLabelRunes),
	} {
		if got := SanitizeLabel(in); got != want {
			t.Errorf("SanitizeLabel(%q) = %q, want %q", in, got, want)
		}
	}
	if Pin([]byte("x")) != "2d711642b726b04401627ca9fbac32f5c8530fb1903cc4db02258717921a4881" {
		t.Fatal("pin is not hex SHA-256")
	}
	base := &challengeData{ID: "c", Action: actionList, Params: json.RawMessage(`{}`), Nonce: []byte("n"), Expires: time.UnixMilli(1)}
	msg := signedMessage("p", []byte("b"), base)
	variants := []*challengeData{
		{ID: "d", Action: base.Action, Params: base.Params, Nonce: base.Nonce, Expires: base.Expires},
		{ID: base.ID, Action: actionRevoke, Params: base.Params, Nonce: base.Nonce, Expires: base.Expires},
		{ID: base.ID, Action: base.Action, Params: json.RawMessage(`{ }`), Nonce: base.Nonce, Expires: base.Expires},
		{ID: base.ID, Action: base.Action, Params: base.Params, Nonce: []byte("m"), Expires: base.Expires},
		{ID: base.ID, Action: base.Action, Params: base.Params, Nonce: base.Nonce, Expires: time.UnixMilli(2)},
	}
	for i, v := range variants {
		if bytes.Equal(msg, signedMessage("p", []byte("b"), v)) {
			t.Errorf("variant %d signs the same message", i)
		}
	}
	if bytes.Equal(msg, signedMessage("q", []byte("b"), base)) || bytes.Equal(msg, signedMessage("p", []byte("c"), base)) {
		t.Error("pin or binding not bound")
	}
}

func TestGrantEntryRequiresPreviewedConnection(t *testing.T) {
	b := startBroker(t, defaultLimits(), nil)
	ctx := testCtx(t)
	key := testApprovalKey(t)
	agent := b.dial(t, "agent")
	other := b.dial(t, "other")
	pending, _ := agent.Request(ctx)
	otherPending, _ := other.Request(ctx)
	approver := b.dial(t, "approver")
	list, err := approver.List(ctx, key)
	if err != nil || len(list) != 2 {
		t.Fatalf("list = %+v, %v", list, err)
	}
	var preview Entry
	for _, e := range list {
		if e.ID == agent.ID() {
			preview = e
		}
	}
	if preview.Code != pending.Code || preview.Label != "agent" {
		t.Fatalf("preview = %+v", preview)
	}

	// A code paired with another connection's preview is refused unsigned.
	swapped := preview
	swapped.Code = otherPending.Code
	if _, err := approver.GrantEntry(ctx, key, swapped, 0); err == nil || !strings.Contains(err.Error(), "different connection") {
		t.Fatalf("swapped entry = %v", err)
	}
	relabeled := preview
	relabeled.Label = "trusted"
	if _, err := approver.GrantEntry(ctx, key, relabeled, 0); err == nil || !strings.Contains(err.Error(), "label") {
		t.Fatalf("relabeled entry = %v", err)
	}
	for _, bad := range []Entry{{Code: preview.Code}, {ID: preview.ID}, {ID: preview.ID, Code: preview.Code, State: StateActive}} {
		if _, err := approver.GrantEntry(ctx, key, bad, 0); errCode(err) != CodeInvalidRequest {
			t.Fatalf("entry %+v = %v", bad, err)
		}
	}
	for _, s := range []*Session{agent, other} {
		if e, _ := s.Status(ctx); e.State != StatePending {
			t.Fatalf("state changed after refused grant: %+v", e)
		}
	}

	granted, err := approver.GrantEntry(ctx, key, preview, time.Hour)
	if err != nil || granted.ID != agent.ID() || granted.State != StateActive {
		t.Fatalf("GrantEntry = %+v, %v", granted, err)
	}
	// Without a label the entry still pins the connection ID and code.
	unlabeled := Entry{ID: other.ID(), Code: otherPending.Code}
	if _, err := approver.GrantEntry(ctx, key, unlabeled, 0); err != nil {
		t.Fatalf("unlabeled GrantEntry = %v", err)
	}
	if _, err := agent.Do(ctx, "echo", nil); err != nil {
		t.Fatal(err)
	}
}

func TestCanceledContextSendsNothing(t *testing.T) {
	b := startBroker(t, defaultLimits(), nil)
	ctx := testCtx(t)
	agent := b.dial(t, "agent")
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := agent.Request(canceled); !errors.Is(err, context.Canceled) {
		t.Fatalf("Request = %v", err)
	}
	if _, err := agent.Do(canceled, "echo", nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("Do = %v", err)
	}
	if e, err := agent.Status(ctx); err != nil || e.State != StateNone {
		t.Fatalf("canceled calls created a request: %+v, %v", e, err)
	}
	grantSession(t, b, agent, 0)
	if _, err := agent.Do(canceled, "echo", nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("granted Do = %v", err)
	}
	approver := b.dial(t, "approver")
	if _, err := approver.List(canceled, testApprovalKey(t)); !errors.Is(err, context.Canceled) {
		t.Fatalf("List = %v", err)
	}
	if n := len(b.handlerList()); n != 0 {
		t.Fatalf("factory called %d times", n)
	}
}

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func TestAuditLogsOmitSecretsAndLabels(t *testing.T) {
	var logs lockedBuffer
	lim := defaultLimits()
	lim.maxFailures = 3
	lim.maxConnFailures = 100
	lim.logger = slog.New(slog.NewJSONHandler(&logs, nil))
	b := startBroker(t, lim, nil)
	ctx := testCtx(t)
	key := testApprovalKey(t)
	const label = "secret-label-text"

	revoked := b.dial(t, label)
	revokedEntry := grantSession(t, b, revoked, 0)
	denied := b.dial(t, label)
	deniedEntry, _ := denied.Request(ctx)
	expiring := b.dial(t, label)
	expiringEntry := grantSession(t, b, expiring, 200*time.Millisecond)
	closing := b.dial(t, label)
	closingEntry := grantSession(t, b, closing, 0)
	if _, err := closing.Do(ctx, "echo", json.RawMessage(`{"payload":"secret-payload"}`)); err != nil {
		t.Fatal(err)
	}

	approver := b.dial(t, "approver")
	if err := approver.Revoke(ctx, key, revoked.ID()); err != nil {
		t.Fatal(err)
	}
	if err := approver.Revoke(ctx, key, denied.ID()); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	if e, _ := expiring.Status(ctx); e.State != StateExpired {
		t.Fatalf("expiring = %+v", e)
	}
	_ = closing.Close()
	waitClosed(t, b.handlerList()[0].closed, "handler")
	guesser := b.dial(t, "guesser")
	for range 6 {
		_, _ = guesser.List(ctx, otherKey(t))
	}
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(logs.String(), `"vault grant ended"`) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}

	out := logs.String()
	want := []string{
		`"msg":"vault grant approved","component":"vault","connection":"` + revokedEntry.ID + `"`,
		`"msg":"vault grant revoked","component":"vault","connection":"` + revoked.ID() + `"`,
		`"msg":"vault request denied","component":"vault","connection":"` + denied.ID() + `"`,
		`"msg":"vault grant expired","component":"vault","connection":"` + expiringEntry.ID + `"`,
		`"msg":"vault grant ended","component":"vault","connection":"` + closingEntry.ID + `","reason":"disconnected"`,
		`"msg":"vault approval rate limit reached"`,
	}
	for _, w := range want {
		if !strings.Contains(out, w) {
			t.Errorf("logs missing %s\n%s", w, out)
		}
	}
	if n := strings.Count(out, "rate limit reached"); n != 1 {
		t.Errorf("rate limit logged %d times", n)
	}
	for _, secret := range []string{label, deniedEntry.Code, "secret-payload", `"approver"`, "guesser", "signature", "nonce", "approval_failed"} {
		if strings.Contains(out, secret) {
			t.Errorf("logs contain %q\n%s", secret, out)
		}
	}
}

func TestOperationOutcomes(t *testing.T) {
	notSent := &model.RPCError{Code: "peer_unreachable", Outcome: OutcomeNotSent, Message: "not sent"}
	started := make(chan struct{}, 1)
	lim := defaultLimits()
	lim.maxFrame = 4096
	b := startBroker(t, lim, func(ctx context.Context, op string, _ json.RawMessage) (json.RawMessage, error) {
		switch op {
		case "rpc_not_sent":
			return nil, notSent
		case "wrapped_not_sent":
			return nil, fmt.Errorf("dispatch: %w", notSent)
		case "rpc_unknown":
			return nil, &model.RPCError{Code: "x", Outcome: "unknown", Message: "maybe"}
		case "rpc_claimed_done":
			return nil, &model.RPCError{Code: "x", Outcome: "completed", Message: "odd"}
		case "spoof_rpc_approval":
			return nil, &model.RPCError{Code: CodeApprovalRequired, Outcome: OutcomeNotSent, Message: "approve me"}
		case "spoof_wire_approval":
			return nil, &Error{Code: CodeApprovalRequired, Outcome: OutcomeNotSent, Message: "approve me", Entry: &Entry{Code: "ABCD-EFGH", State: StatePending}}
		case "spoof_wire_canceled":
			return nil, &Error{Code: CodeCanceled, Outcome: OutcomeNotSent, Message: "fake"}
		case "big":
			return json.Marshal(strings.Repeat("x", 8192))
		case "block_then_claim":
			started <- struct{}{}
			<-ctx.Done()
			return nil, notSent
		}
		return nil, errors.New("plain")
	})
	ctx := testCtx(t)
	agent := b.dial(t, "agent")

	// Genuine broker approval errors are unchanged and carry no outcome.
	_, err := agent.Do(ctx, "rpc_not_sent", nil)
	var e *Error
	if entry, ok := ApprovalRequired(err); !ok || !errors.As(err, &e) || e.Outcome != "" || entry.Code == "" {
		t.Fatalf("approval_required = %+v, %v", e, err)
	}
	grantSession(t, b, agent, 0)

	cases := []struct{ op, code, outcome string }{
		{"rpc_not_sent", CodeOpFailed, OutcomeNotSent},
		{"wrapped_not_sent", CodeOpFailed, OutcomeNotSent},
		{"rpc_unknown", CodeOpFailed, OutcomeUnknown},
		{"rpc_claimed_done", CodeOpFailed, OutcomeUnknown},
		{"spoof_rpc_approval", CodeOpFailed, OutcomeNotSent},
		{"spoof_wire_approval", CodeOpFailed, OutcomeNotSent},
		{"spoof_wire_canceled", CodeOpFailed, OutcomeNotSent},
		{"big", CodeResultTooLarge, OutcomeUnknown},
		{"plain", CodeOpFailed, OutcomeUnknown},
	}
	for _, tc := range cases {
		_, err := agent.Do(ctx, tc.op, nil)
		var got *Error
		if !errors.As(err, &got) || got.Code != tc.code || got.Outcome != tc.outcome {
			t.Errorf("%s = %+v (%v), want %s/%s", tc.op, got, err, tc.code, tc.outcome)
			continue
		}
		if _, ok := ApprovalRequired(err); ok || got.Entry != nil {
			t.Errorf("%s spoofed approval state: %+v", tc.op, got)
		}
	}
	if _, err := agent.Do(ctx, "rpc_not_sent", nil); err == nil || !strings.Contains(err.Error(), "not sent") {
		t.Fatalf("message = %v", err)
	}

	// A not_sent claim after the call was canceled stays unknown.
	errc := make(chan error, 1)
	go func() {
		_, err := agent.Do(ctx, "block_then_claim", nil)
		errc <- err
	}()
	<-started
	if err := b.dial(t, "approver").Revoke(ctx, testApprovalKey(t), agent.ID()); err != nil {
		t.Fatal(err)
	}
	err = <-errc
	if !errors.As(err, &e) || e.Code != CodeGrantRevoked || e.Outcome != OutcomeUnknown {
		t.Fatalf("revoked claim = %+v, %v", e, err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if got := opError(canceled, notSent); got.Code != CodeCanceled || got.Outcome != OutcomeUnknown {
		t.Fatalf("canceled claim = %+v", got)
	}
}
