package transport

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"time"

	"github.com/coder/websocket"
)

type transientGatewayError struct{ error }

func (e transientGatewayError) Unwrap() error { return e.error }

func gatewayNetworkError(err error) error {
	if err == nil {
		return nil
	}
	// url.Error implements net.Error even when the underlying failure is a
	// permanent TLS or URL error. Classify its cause, not the wrapper.
	cause := err
	var requestErr *url.Error
	if errors.As(err, &requestErr) {
		cause = requestErr.Err
	}
	var certificateErr *tls.CertificateVerificationError
	var authorityErr x509.UnknownAuthorityError
	var hostnameErr x509.HostnameError
	var invalidCertErr x509.CertificateInvalidError
	if errors.As(cause, &certificateErr) || errors.As(cause, &authorityErr) || errors.As(cause, &hostnameErr) || errors.As(cause, &invalidCertErr) {
		return err
	}
	var networkErr net.Error
	if errors.As(cause, &networkErr) || errors.Is(cause, io.EOF) || errors.Is(cause, io.ErrUnexpectedEOF) {
		return transientGatewayError{err}
	}
	switch websocket.CloseStatus(cause) {
	case websocket.StatusAbnormalClosure, websocket.StatusInternalError, websocket.StatusServiceRestart, websocket.StatusTryAgainLater:
		return transientGatewayError{err}
	}
	return err
}

func gatewayResponseError(code int, message string) error {
	err := fmt.Errorf("%s: gateway returned %d %s", message, code, http.StatusText(code))
	if code == http.StatusRequestTimeout || code == http.StatusTooManyRequests || code >= 500 && code <= 599 {
		return transientGatewayError{err}
	}
	return err
}

// WaitForGateway retries transient connection and policy-query failures during
// node startup. It must not be used to replay execution or other side effects.
// Authentication, protocol, TLS validation, and local persistence errors fail
// immediately. The caller's context owns the entire wait.
func WaitForGateway(ctx context.Context, operation string, call func(context.Context) error) error {
	return waitForGateway(ctx, slog.Default(), operation, call)
}

func waitForGateway(ctx context.Context, log *slog.Logger, operation string, call func(context.Context) error) error {
	delay := reconnectInitial
	for failures := 0; ; {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := call(ctx)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err == nil {
			if failures > 0 {
				log.Info("gateway startup recovered", "operation", operation, "attempts", failures+1)
			}
			return nil
		}
		var transient transientGatewayError
		if !errors.As(err, &transient) {
			return err
		}
		failures++
		if failures >= reconnectFastAttempts {
			delay = min(delay*2, reconnectMax)
		}
		retryAfter := min(delay/2+rand.N(delay), reconnectMax)
		if failures == 1 || failures%10 == 0 {
			log.Warn("gateway startup waiting; retrying", "operation", operation, "attempts", failures, "retryAfter", retryAfter, "error", err)
		} else {
			log.Debug("gateway startup waiting; retrying", "operation", operation, "attempts", failures, "retryAfter", retryAfter, "error", err)
		}
		timer := time.NewTimer(retryAfter)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}
