package transport

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hashicorp/yamux"
	"github.com/koltyakov/control/internal/gateway"
	"github.com/koltyakov/control/internal/identity"
	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/protocol"
	"github.com/koltyakov/control/internal/update"
	"github.com/koltyakov/control/internal/wire"
)

type timeoutError struct{}

func (timeoutError) Error() string   { return "i/o timeout" }
func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return true }

type refusedError struct{}

func (refusedError) Error() string   { return "connection refused" }
func (refusedError) Timeout() bool   { return false }
func (refusedError) Temporary() bool { return false }

func TestRetryableDeliveryClassification(t *testing.T) {
	status := func(code int) error {
		return directoryStatusError("directory", &http.Response{StatusCode: code, Status: fmt.Sprintf("%d %s", code, http.StatusText(code))})
	}
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"relay drain", transientSetup("gateway relay is draining for maintenance"), true},
		{"wrapped setup failure", fmt.Errorf("open: %w", transientSetup("network changed during peer setup")), true},
		{"setup timeout while waiting", transientSetupCause(context.DeadlineExceeded), true},
		{"caller deadline", context.DeadlineExceeded, false},
		{"caller cancellation", fmt.Errorf("open: %w", context.Canceled), false},
		{"stream ended", io.EOF, true},
		{"truncated frame", io.ErrUnexpectedEOF, true},
		{"multiplexer shutdown", yamux.ErrSessionShutdown, true},
		{"keepalive timeout", fmt.Errorf("read: %w", yamux.ErrKeepAliveTimeout), true},
		{"network timeout", &net.OpError{Op: "read", Err: timeoutError{}}, true},
		{"refused connect", &net.OpError{Op: "dial", Err: refusedError{}}, false},
		{"broken socket", &net.OpError{Op: "write", Err: brokenSocketErrors[0]}, true},
		{"oversized frame", wire.ErrFrameTooLarge, false},
		{"tls alert", tls.AlertError(42), false},
		{"certificate", x509.UnknownAuthorityError{}, false},
		{"gateway restart status", status(http.StatusServiceUnavailable), true},
		{"gateway auth status", status(http.StatusUnauthorized), false},
		{"gateway network failure", gatewayNetworkError(&net.OpError{Op: "dial", Err: refusedError{}}), true},
		{"unknown node", fmt.Errorf("unknown node %q", "gone"), false},
		{"capability negotiation", errors.New("target does not support instruction-bound delegation; upgrade it before executing work"), false},
		{"nil", nil, false},
	} {
		if got := RetryableDelivery(tc.err); got != tc.want {
			t.Errorf("%s: retryable = %v, want %v (%v)", tc.name, got, tc.want, tc.err)
		}
	}
	// Classification never changes the message operators see.
	if err := transientSetup("peer closed session"); err.Error() != "peer closed session" {
		t.Fatal(err)
	}
	if err := transientSetupCause(context.DeadlineExceeded); err.Error() != context.DeadlineExceeded.Error() || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("setup timeout lost its cause", err)
	}
}

func TestSetupSourcesReportTransientFailures(t *testing.T) {
	p := &Peer{relayDrainUntil: time.Now().Add(time.Minute)}
	if _, err := p.dial(t.Context(), "remote", "relay", ControlLane); !errors.Is(err, ErrTransientSetup) || err.Error() != "gateway relay is draining for maintenance" {
		t.Fatal("relay drain was not a transient setup failure", err)
	}
	running, stop := context.WithCancel(t.Context())
	p = &Peer{ctx: running}
	if err := p.send(t.Context(), &protocol.Packet{Kind: "open"}); !errors.Is(err, ErrTransientSetup) {
		t.Fatal("gateway reconnect was not a transient setup failure", err)
	}
	stop()
	if err := p.send(t.Context(), &protocol.Packet{Kind: "open"}); err == nil || RetryableDelivery(err) {
		t.Fatal("a stopped peer must not look retryable", err)
	}
}

// A real relay drain refuses new setups before any request byte is written.
// The caller sees a not-sent delivery error with a retryable local cause, and
// the same request succeeds after the gateway resumes relay admission.
func TestRelayDrainSetupIsRetryableAndNotSent(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	const token = "relay-drain-retry-fixture-token-123456"
	g, err := gateway.New(t.TempDir(), token)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = g.Close() }()
	server := httptest.NewServer(g.Handler())
	defer server.Close()
	var served atomic.Int32
	peers := []*Peer{}
	for _, name := range []string{"caller", "receiver"} {
		id, err := identity.Generate()
		if err != nil {
			t.Fatal(err)
		}
		p := New(Config{Gateway: server.URL, Token: token, Identity: id, RelayOnly: true, Node: model.Node{ID: id.ID, Name: name, PublicKey: id.Public}}, func(_ string, conn net.Conn) {
			served.Add(1)
			var request model.Request
			if wire.ReadFrame(conn, &request) == nil {
				_ = wire.WriteFrame(conn, model.Response{Result: model.JSON("ok")})
			}
			_ = conn.Close()
		})
		p.networkSnapshot = func() (string, error) { return "unchanged", nil }
		if err := p.Start(ctx); err != nil {
			t.Fatal(err)
		}
		defer func() { _ = p.Close() }()
		peers = append(peers, p)
	}
	a, b := peers[0], peers[1]
	packet := &protocol.Packet{Kind: "gateway.drain", From: update.GatewaySender, Session: identity.NewID(), Data: model.JSON(gateway.RelayDrainRequest{Until: time.Now().Add(10 * time.Second)})}
	a.receive(packet)
	_, _, err = a.OpenRPC(ctx, b.IdentityID(), "tasks.get", map[string]any{"id": "child"})
	var rpc *model.RPCError
	if !errors.As(err, &rpc) || rpc.Outcome != "not_sent" || rpc.Cause == nil || !RetryableDelivery(rpc.Cause) {
		t.Fatal("drained relay setup was not a retryable not-sent failure", err)
	}
	if served.Load() != 0 {
		t.Fatal("drained setup reached the receiver")
	}
	packet.Kind = "gateway.resume"
	a.receive(packet)
	conn, result, err := a.OpenRPC(ctx, b.IdentityID(), "tasks.get", map[string]any{"id": "child"})
	if err != nil || string(result) != `"ok"` {
		t.Fatal("request did not succeed after relay resume", string(result), err)
	}
	_ = conn.Close()
}
