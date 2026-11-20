// Package gatewayroute carries gateway-bound HTTP traffic either directly or
// through one explicitly configured CONNECT proxy.
//
// A proxied route never falls back to a direct connection and ignores the
// inherited HTTP_PROXY, HTTPS_PROXY, and NO_PROXY variables. It changes no
// global HTTP defaults or child process environments, so provider requests
// keep their own proxy behavior. Gateway TLS verification is unchanged.
package gatewayroute

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/koltyakov/control/internal/model"
)

// Route selects how this process reaches its gateway. The zero value is direct.
type Route struct {
	proxy       string
	maintenance *routeMaintenance
}

// MarshalJSON preserves the next hop in saved client and tunnel-service profiles.
func (r Route) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Proxy string `json:"proxy,omitempty"`
	}{Proxy: r.proxy})
}

// UnmarshalJSON validates saved routes before they can carry gateway credentials.
// Legacy direct profiles omitted the proxy field or stored an empty object.
func (r *Route) UnmarshalJSON(data []byte) error {
	var saved struct {
		Proxy string `json:"proxy,omitempty"`
	}
	if err := json.Unmarshal(data, &saved); err != nil {
		return err
	}
	route, err := New(saved.Proxy)
	if err != nil {
		return err
	}
	*r = route
	return nil
}

// New returns a direct route for an empty proxy, otherwise a normalized
// proxied route.
func New(proxy string) (Route, error) {
	if strings.TrimSpace(proxy) == "" {
		return Route{}, nil
	}
	normalized, err := model.NormalizeProxyURL(proxy)
	if err != nil {
		return Route{}, err
	}
	return Route{proxy: normalized, maintenance: &routeMaintenance{changed: make(chan struct{})}}, nil
}

// FromConfig accepts a persisted child profile selection.
func FromConfig(proxy *model.GatewayProxy) (Route, error) {
	if proxy == nil {
		return Route{}, nil
	}
	if strings.TrimSpace(proxy.URL) == "" {
		return Route{}, errors.New("gatewayProxy.url is required when gatewayProxy is set")
	}
	return New(proxy.URL)
}

// Proxy returns the normalized proxy URL, or "" for a direct route.
func (r Route) Proxy() string { return r.proxy }

// Proxied reports whether gateway traffic uses an explicit proxy.
func (r Route) Proxied() bool { return r.proxy != "" }

// CheckGateway requires HTTPS for proxied routes. The proxy then sees only a
// CONNECT target, and the child verifies the gateway certificate end to end.
func (r Route) CheckGateway(gateway string) error {
	if !r.Proxied() {
		return nil
	}
	u, err := url.Parse(gateway)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return errHTTPSRequired
	}
	return nil
}

// MonitorHost names the next hop whose local route should be watched: the
// proxy when one is configured, otherwise the gateway itself.
func (r Route) MonitorHost(gateway string) (host, port string) {
	target := gateway
	if r.Proxied() {
		target = r.proxy
	}
	u, err := url.Parse(target)
	if err != nil {
		return "", ""
	}
	port = u.Port()
	if port == "" {
		port = "443"
		if u.Scheme == "http" {
			port = "80"
		}
	}
	return u.Hostname(), port
}

// Transport returns the shared round tripper for this route. Direct routes keep
// the process default, including its environment proxy settings.
func (r Route) Transport() http.RoundTripper {
	base := r.baseTransport()
	if r.maintenance != nil {
		return maintenanceTransport{base: base, state: r.maintenance}
	}
	return base
}

func (r Route) baseTransport() http.RoundTripper {
	mu.Lock()
	defer mu.Unlock()
	if roots == nil && !r.Proxied() {
		return http.DefaultTransport
	}
	if t := transports[r.proxy]; t != nil {
		return t
	}
	t := newTransport(r.proxy, true)
	transports[r.proxy] = t
	return t
}

// CloseIdleConnections drops pooled gateway connections without interrupting
// requests in flight. After a network change or a lost gateway socket, pooled
// connections may be bound to an abandoned path; reusing one would stall the
// next request until its timeout instead of dialing over the current path.
func (r Route) CloseIdleConnections() {
	if t, ok := r.baseTransport().(interface{ CloseIdleConnections() }); ok {
		t.CloseIdleConnections()
	}
}

// Client builds a gateway client preserving the caller's timeout and redirect
// policy. A zero timeout leaves deadlines to the request context.
func (r Route) Client(timeout time.Duration, followRedirects bool) *http.Client {
	c := &http.Client{Timeout: timeout, Transport: r.Transport()}
	if !followRedirects {
		c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	}
	return c
}

// Do sends one request with the default client policy on this route.
func (r Route) Do(req *http.Request) (*http.Response, error) {
	return (&http.Client{Transport: r.Transport()}).Do(req)
}

// Probe performs one bounded proxy-to-gateway check without reusing pooled
// connections: it establishes CONNECT, completes gateway TLS verification,
// and fetches the gateway health endpoint. The returned error is sanitized.
func Probe(ctx context.Context, proxy, gateway string) error {
	route, err := New(proxy)
	if err != nil || !route.Proxied() {
		return errors.New("invalid proxy endpoint")
	}
	if err = route.CheckGateway(gateway); err != nil {
		return errors.New("gateway is not HTTPS")
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(gateway, "/")+"/healthz", nil)
	if err != nil {
		return errors.New("invalid gateway URL")
	}
	mu.Lock()
	t := newTransport(route.proxy, false)
	mu.Unlock()
	defer t.CloseIdleConnections()
	client := &http.Client{Transport: t, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		return sanitize(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return errors.New("gateway health check returned " + resp.Status)
	}
	return nil
}

// ProbeDirect checks the direct path a Control-managed listener uses: it
// dials the gateway with dial, completes gateway TLS verification, and fetches
// the gateway health endpoint without inherited proxy settings or pooling.
func ProbeDirect(ctx context.Context, gateway string, dial func(context.Context, string, string) (net.Conn, error)) error {
	u, err := url.Parse(gateway)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return errors.New("gateway is not HTTPS")
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(gateway, "/")+"/healthz", nil)
	if err != nil {
		return errors.New("invalid gateway URL")
	}
	mu.Lock()
	t := newTransport("", false)
	mu.Unlock()
	t.Proxy, t.DialContext = nil, dial
	defer t.CloseIdleConnections()
	client := &http.Client{Transport: t, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		var certificate *tls.CertificateVerificationError
		var unknown x509.UnknownAuthorityError
		var hostname x509.HostnameError
		if errors.As(err, &certificate) || errors.As(err, &unknown) || errors.As(err, &hostname) {
			return errors.New("gateway TLS verification failed")
		}
		if errors.Is(err, context.DeadlineExceeded) {
			return errors.New("direct gateway check timed out")
		}
		return errors.New("direct gateway connection failed")
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return errors.New("gateway health check returned " + resp.Status)
	}
	return nil
}

// sanitize reduces transport errors to a bounded category without echoing
// addresses, certificate subjects, or proxy response bodies.
func sanitize(err error) error {
	var certificate *tls.CertificateVerificationError
	var unknown x509.UnknownAuthorityError
	var hostname x509.HostnameError
	var invalid x509.CertificateInvalidError
	var netErr net.Error
	var dns *net.DNSError
	var op *net.OpError
	text := err.Error()
	switch {
	case errors.As(err, &certificate), errors.As(err, &unknown), errors.As(err, &hostname), errors.As(err, &invalid):
		return errors.New("gateway TLS verification failed")
	case strings.Contains(text, "proxyconnect"):
		if errors.As(err, &dns) {
			return errors.New("proxy hostname did not resolve")
		}
		if errors.As(err, &netErr) && netErr.Timeout() {
			return errors.New("proxy connection timed out")
		}
		return errors.New("proxy connection failed")
	case strings.Contains(text, "Proxy Authentication Required"):
		return errors.New("proxy requires authentication")
	case errors.As(err, &netErr) && netErr.Timeout(), errors.Is(err, context.DeadlineExceeded):
		return errors.New("proxy-to-gateway check timed out")
	case errors.As(err, &op) && op.Op == "proxyconnect":
		return errors.New("proxy connection failed")
	}
	// net/http reports a non-200 CONNECT response as the bare status text.
	for _, status := range []string{"Forbidden", "Bad Gateway", "Service Unavailable", "Gateway Timeout", "Method Not Allowed", "Not Found"} {
		if strings.Contains(text, status) {
			return errors.New("proxy refused CONNECT: " + strings.ToLower(status))
		}
	}
	return errors.New("proxy-to-gateway check failed")
}

var (
	mu               sync.Mutex
	transports       = map[string]*http.Transport{}
	roots            *x509.CertPool
	errHTTPSRequired = errors.New("a gateway proxy requires an https:// gateway URL; TLS verification is mandatory")
)

func newTransport(proxy string, keepAlive bool) *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	if proxy != "" {
		u, _ := url.Parse(proxy)
		// Check every request, including redirects and callers supplying their
		// own redirect policy. No plaintext request may expose credentials or
		// installation tickets to the proxy. This also ignores NO_PROXY.
		t.Proxy = func(req *http.Request) (*url.URL, error) {
			if req.URL.Scheme != "https" {
				return nil, errHTTPSRequired
			}
			return u, nil
		}
	}
	if roots != nil {
		t.TLSClientConfig = &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}
	}
	t.DisableKeepAlives = !keepAlive
	return t
}

// SetRootCAsForTesting trusts a test CA for gateway routes created afterward.
// Certificate verification stays enabled. The returned function restores the
// system roots and drops cached transports.
func SetRootCAsForTesting(pool *x509.CertPool) func() {
	mu.Lock()
	previous := roots
	roots = pool
	reset()
	mu.Unlock()
	return func() {
		mu.Lock()
		roots = previous
		reset()
		mu.Unlock()
	}
}

func reset() {
	for _, t := range transports {
		t.CloseIdleConnections()
	}
	transports = map[string]*http.Transport{}
}
