package transport

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"os"

	"github.com/hashicorp/yamux"
	"github.com/koltyakov/control/internal/wire"
)

// ErrTransientSetup matches peer setup failures that left the request unsent
// and are expected to clear when the gateway relay, the network path, or the
// remote peer recovers: relay maintenance drains, network changes during
// setup, peer sessions closed during setup, gateway reconnects, and setup
// timeouts while the caller is still waiting. It is a classification for
// idempotent observation only; it never authorizes replaying effects.
var ErrTransientSetup = errors.New("transient peer setup failure")

// transientSetupError keeps the original message, so operators and existing
// diagnostics see the same text, while making the class detectable.
type transientSetupError struct {
	message string
	cause   error
}

func (e *transientSetupError) Error() string        { return e.message }
func (e *transientSetupError) Unwrap() error        { return e.cause }
func (e *transientSetupError) Is(target error) bool { return target == ErrTransientSetup }

func transientSetup(message string) error { return &transientSetupError{message: message} }

// transientSetupCause marks a lower-level setup failure as transient while
// preserving its message and cause chain.
func transientSetupCause(err error) error {
	if err == nil || errors.Is(err, ErrTransientSetup) {
		return err
	}
	return &transientSetupError{message: err.Error(), cause: err}
}

// RetryableDelivery reports whether err is a local delivery failure that may
// clear on reconnect: a transient setup failure, a stream or multiplexer that
// broke, or a network timeout. Callers must pass only locally produced causes,
// never errors decoded from a remote reply, and must check their own context
// first. Use it only to repeat idempotent reads to a target that has already
// authenticated and accepted the related request; it is not permission to
// resend anything with effects.
//
// Context errors (other than a setup timeout already classified at its
// source), TLS and certificate failures, oversized frames, and gateway
// authentication or protocol errors are not retryable.
func RetryableDelivery(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrTransientSetup) {
		return true
	}
	var gatewayTransient transientGatewayError
	if errors.As(err, &gatewayTransient) {
		return true
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, wire.ErrFrameTooLarge) {
		return false
	}
	var alert tls.AlertError
	var verification *tls.CertificateVerificationError
	var header tls.RecordHeaderError
	var authority x509.UnknownAuthorityError
	var hostname x509.HostnameError
	var invalid x509.CertificateInvalidError
	if errors.As(err, &alert) || errors.As(err, &verification) || errors.As(err, &header) ||
		errors.As(err, &authority) || errors.As(err, &hostname) || errors.As(err, &invalid) {
		return false
	}
	for _, broken := range brokenStreamErrors {
		if errors.Is(err, broken) {
			return true
		}
	}
	for _, broken := range brokenSocketErrors {
		if errors.Is(err, broken) {
			return true
		}
	}
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

// A stream that ends or a multiplexer that shuts down mid-request.
var brokenStreamErrors = []error{
	io.EOF, io.ErrUnexpectedEOF, io.ErrClosedPipe, net.ErrClosed, os.ErrDeadlineExceeded,
	yamux.ErrSessionShutdown, yamux.ErrStreamClosed, yamux.ErrConnectionReset,
	yamux.ErrKeepAliveTimeout, yamux.ErrConnectionWriteTimeout, yamux.ErrRemoteGoAway, yamux.ErrTimeout,
}
