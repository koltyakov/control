package transport

import (
	"context"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/koltyakov/control/internal/identity"
)

func TestGatewayStartupErrorClassification(t *testing.T) {
	for _, tc := range []struct {
		name      string
		err       error
		retryable bool
	}{
		{"request timeout", gatewayNetworkError(&url.Error{Op: "Post", URL: "https://gateway/v1/node/state", Err: context.DeadlineExceeded}), true},
		{"connection refused", gatewayNetworkError(&net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}), true},
		{"DNS unavailable", gatewayNetworkError(&net.DNSError{Err: "no such host", Name: "gateway"}), true},
		{"closed connection", gatewayNetworkError(io.EOF), true},
		{"truncated body", gatewayNetworkError(io.ErrUnexpectedEOF), true},
		{"service restart", gatewayNetworkError(websocket.CloseError{Code: websocket.StatusServiceRestart}), true},
		{"websocket policy rejection", gatewayNetworkError(websocket.CloseError{Code: websocket.StatusPolicyViolation}), false},
		{"untrusted TLS certificate", gatewayNetworkError(&url.Error{Op: "Get", URL: "https://gateway/v1/auth", Err: x509.UnknownAuthorityError{}}), false},
		{"TLS certificate wrapped in network error", gatewayNetworkError(&net.OpError{Op: "dial", Net: "tcp", Err: x509.UnknownAuthorityError{}}), false},
		{"malformed URL", gatewayNetworkError(&url.Error{Op: "Get", URL: "bad", Err: errors.New("unsupported protocol scheme")}), false},
		{"local persistence", &os.PathError{Op: "open", Path: "machine-state.json", Err: os.ErrPermission}, false},
		{"protocol error", errors.New("invalid gateway challenge"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var transient transientGatewayError
			if got := errors.As(tc.err, &transient); got != tc.retryable {
				t.Fatalf("retryable = %v, want %v: %v", got, tc.retryable, tc.err)
			}
		})
	}
	for _, code := range []int{400, 401, 403, 404, 408, 409, 429, 500, 502, 503, 504} {
		var transient transientGatewayError
		err := gatewayResponseError(code, "test")
		want := code == 408 || code == 429 || code >= 500
		if got := errors.As(err, &transient); got != want {
			t.Fatalf("HTTP %d retryable = %v, want %v", code, got, want)
		}
	}
}

func TestWaitForGatewayCancellation(t *testing.T) {
	for _, duringRequest := range []bool{false, true} {
		t.Run(map[bool]string{false: "backoff", true: "request"}[duringRequest], func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			calls := 0
			err := WaitForGateway(ctx, "test", func(ctx context.Context) error {
				calls++
				if duringRequest {
					<-ctx.Done()
					return gatewayNetworkError(ctx.Err())
				}
				return gatewayResponseError(http.StatusServiceUnavailable, "test")
			})
			if !errors.Is(err, context.DeadlineExceeded) || calls != 1 {
				t.Fatalf("cancellation did not stop startup: calls=%d, error=%v", calls, err)
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := WaitForGateway(ctx, "test", func(context.Context) error {
		t.Fatal("cancelled startup made a request")
		return nil
	}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestWaitForGatewayPermanentFailure(t *testing.T) {
	want := errors.New("local persistence failed")
	calls := 0
	if err := WaitForGateway(context.Background(), "test", func(context.Context) error {
		calls++
		return want
	}); err != want || calls != 1 {
		t.Fatalf("permanent error was retried: calls=%d, error=%v", calls, err)
	}
}

func TestClientStartupDoesNotRetryGatewayFailure(t *testing.T) {
	var calls atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	defer s.Close()
	id, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	p := New(Config{Client: true, Gateway: s.URL, Token: "test", Identity: id}, nil)
	defer func() { _ = p.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := p.Start(ctx); err == nil || errors.Is(err, context.DeadlineExceeded) || calls.Load() != 1 {
		t.Fatalf("command-scoped client did not fail fast: calls=%d, error=%v", calls.Load(), err)
	}
}
