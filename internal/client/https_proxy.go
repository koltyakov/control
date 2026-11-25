package client

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"github.com/koltyakov/control/internal/transport"
)

const (
	// Chrome can open separate HTTP/2 proxy connections for different
	// network partitions, even when only a few tabs are open.
	httpsProxyCarriers = 32
	httpsProxyWaiters  = 32
	httpsProxySlotWait = 500 * time.Millisecond
)

var (
	errHTTPSProxyNonLoopback = errors.New("HTTPS proxy listeners must bind to loopback")
	errHTTPSProxyCapacity    = errors.New("HTTPS proxy connection limit reached")
)

func validateHTTPSForward(spec ForwardSpec) error {
	if spec.Reverse || spec.Address != "" {
		return errors.New("httpsProxy takes no destination address and cannot be reversed")
	}
	if spec.AllowNonLoopback {
		return errHTTPSProxyNonLoopback
	}
	if err := validateProxyConnectionLimit(spec.MaxConnections); err != nil {
		return err
	}
	return validateProxyLoopback(spec.Listen, errHTTPSProxyNonLoopback)
}

// Each listener gets a fresh leaf and key. Chrome can pin this key in one
// profile without installing a CA or disabling verification for website TLS.
func newHTTPSProxyCertificate(ip net.IP) (tls.Certificate, string, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, "", err
	}
	now := time.Now()
	template := &x509.Certificate{
		Subject:   pkix.Name{CommonName: "Control loopback HTTPS proxy"},
		NotBefore: now.Add(-time.Minute), NotAfter: now.Add(48 * time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames: []string{"localhost"}, IPAddresses: []net.IP{ip},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, "", err
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return tls.Certificate{}, "", err
	}
	hash := sha256.Sum256(leaf.RawSubjectPublicKeyInfo)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}, base64.StdEncoding.EncodeToString(hash[:]), nil
}

type httpsForwardProxy struct {
	server  *http.Server
	slots   chan struct{}
	waiters chan struct{}
}

func newHTTPSForwardProxy(c Client, f *Forward, limit int, cert tls.Certificate) *httpsForwardProxy {
	proxy := &httpsForwardProxy{slots: make(chan struct{}, limit), waiters: make(chan struct{}, httpsProxyWaiters)}
	protocols := new(http.Protocols)
	protocols.SetHTTP2(true)
	proxy.server = &http.Server{
		Handler:   http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { f.serveHTTPSRequest(c, w, r) }),
		Protocols: protocols, TLSConfig: &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{cert}},
		HTTP2: &http.HTTP2Config{MaxConcurrentStreams: limit + httpsProxyWaiters, MaxReceiveBufferPerStream: 64 << 10,
			MaxReceiveBufferPerConnection: 1 << 20, WriteByteTimeout: 15 * time.Second},
		ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 2 * time.Minute, MaxHeaderBytes: 16 << 10,
		BaseContext: func(net.Listener) context.Context { return f.ctx },
		ConnState:   f.httpsProxyConnState,
	}
	return proxy
}

func (f *Forward) serveHTTPSProxy() {
	defer f.wg.Done()
	defer func() { go f.Close() }()
	err := f.httpsProxy.server.ServeTLS(f.listener, "", "")
	if err != nil && f.ctx.Err() == nil && !errors.Is(err, http.ErrServerClosed) {
		f.recordError(err)
	}
	f.cancel()
}

// Track the underlying TLS connections separately from their CONNECT streams.
// StateNew is called synchronously by Serve before it starts a handler.
func (f *Forward) httpsProxyConnState(conn net.Conn, state http.ConnState) {
	switch state {
	case http.StateNew:
		f.mu.Lock()
		if f.ctx.Err() == nil && len(f.connections) < httpsProxyCarriers {
			f.connections[conn] = nil
			f.mu.Unlock()
			return
		}
		if f.ctx.Err() == nil {
			f.info.CapacityRejections++
			f.info.LastError = "HTTPS proxy TLS connection limit reached"
		}
		f.mu.Unlock()
		_ = conn.Close()
	case http.StateClosed:
		f.mu.Lock()
		delete(f.connections, conn)
		f.mu.Unlock()
	}
}

func (p *httpsForwardProxy) acquire(ctx context.Context) error {
	select {
	case p.slots <- struct{}{}:
		return nil
	default:
	}
	select {
	case p.waiters <- struct{}{}:
		defer func() { <-p.waiters }()
	default:
		return errHTTPSProxyCapacity
	}
	timer := time.NewTimer(httpsProxySlotWait)
	defer timer.Stop()
	select {
	case p.slots <- struct{}{}:
		return nil
	case <-timer.C:
		return errHTTPSProxyCapacity
	case <-ctx.Done():
		return ctx.Err()
	}
}

func validHTTPSConnectAddress(address string) bool {
	host, port, err := net.SplitHostPort(address)
	if err != nil || len(host) > 253 || !validProxyHost([]byte(host)) {
		return false
	}
	n, err := strconv.ParseUint(port, 10, 16)
	return err == nil && n != 0
}

func plainHTTPProxyAddress(authority string) string {
	if validHTTPSConnectAddress(authority) {
		return authority
	}
	host := authority
	if stripped, ok := strings.CutPrefix(host, "["); ok {
		var closed bool
		host, closed = strings.CutSuffix(stripped, "]")
		ip, err := netip.ParseAddr(host)
		if !closed || err != nil || !ip.Is6() || ip.Zone() != "" {
			return ""
		}
	} else if strings.ContainsAny(host, ":[]") {
		return ""
	}
	address := net.JoinHostPort(host, "80")
	if !validHTTPSConnectAddress(address) {
		return ""
	}
	return address
}

func (f *Forward) serveHTTPSRequest(c Client, w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	if f.ctx.Err() != nil {
		f.mu.Unlock()
		http.Error(w, "proxy is closed", http.StatusServiceUnavailable)
		return
	}
	f.wg.Add(1)
	f.mu.Unlock()
	defer f.wg.Done()
	if r.ProtoMajor != 2 {
		http.Error(w, "HTTP/2 required", http.StatusHTTPVersionNotSupported)
		return
	}
	address := r.Host
	isConnect := r.Method == http.MethodConnect
	if isConnect {
		if r.URL.Path != "" || r.URL.RawQuery != "" || r.Header.Get(":protocol") != "" || !validHTTPSConnectAddress(address) {
			http.Error(w, "CONNECT requires HOST:PORT", http.StatusBadRequest)
			return
		}
	} else {
		address = plainHTTPProxyAddress(r.Host)
		if address == "" || r.URL.IsAbs() || r.URL.Host != "" || r.Header.Get("Upgrade") != "" {
			http.Error(w, "invalid HTTP proxy request", http.StatusBadRequest)
			return
		}
	}
	ctx := r.Context()
	if err := f.httpsProxy.acquire(ctx); err != nil {
		if errors.Is(err, errHTTPSProxyCapacity) && ctx.Err() == nil {
			f.rejectHTTPSCapacity(w, err)
		}
		return
	}
	defer func() { <-f.httpsProxy.slots }()
	if !isConnect {
		f.proxyPlainHTTP(c, w, r, address)
		return
	}
	upstream, err := c.tunnel(ctx, f.info.Node, r.Host, openProxyPeerTCP)
	if err != nil {
		f.writeHTTPSProxyError(w, r, err)
		return
	}
	defer func() { _ = upstream.Close() }()
	relayHTTPSConnect(w, r.Body, upstream)
}

func (f *Forward) proxyPlainHTTP(c Client, w http.ResponseWriter, r *http.Request, address string) {
	// net/http presents an ordinary HTTP/2 request as an origin-form path.
	// On this proxy, plain requests go to HTTP; website TLS goes via CONNECT.
	// One worker dial per request avoids retaining idle peer streams or
	// replaying application requests on a stale connection.
	tr := &http.Transport{
		DialContext: func(context.Context, string, string) (net.Conn, error) {
			// Transport may let a dial outlive a request for connection reuse.
			// This connection instead belongs to the incoming request.
			return c.tunnel(r.Context(), f.info.Node, address, openProxyPeerTCP)
		},
		DisableKeepAlives: true, DisableCompression: true,
		ResponseHeaderTimeout: 30 * time.Second, MaxResponseHeaderBytes: 64 << 10,
	}
	defer tr.CloseIdleConnections()
	proxy := &httputil.ReverseProxy{
		Transport: tr,
		Rewrite: func(req *httputil.ProxyRequest) {
			req.Out.URL.Scheme = "http"
			req.Out.URL.Host = address
			req.Out.Host = r.Host
		},
		ModifyResponse: func(resp *http.Response) error {
			if resp.StatusCode == http.StatusSwitchingProtocols {
				return errors.New("HTTP Upgrade is not supported by this proxy")
			}
			return nil
		},
		ErrorHandler: f.writeHTTPSProxyError,
	}
	proxy.ServeHTTP(w, r)
}

func (f *Forward) writeHTTPSProxyError(w http.ResponseWriter, r *http.Request, err error) {
	if r.Context().Err() != nil {
		return
	}
	switch {
	case transport.IsStreamCapacity(err):
		f.rejectHTTPSCapacity(w, err)
	case socksReplyCode(err) == socksReplyNotAllowed:
		f.recordError(err)
		http.Error(w, "destination not allowed", http.StatusForbidden)
	default:
		f.recordError(err)
		http.Error(w, "worker could not reach destination", http.StatusBadGateway)
	}
}

func (f *Forward) rejectHTTPSCapacity(w http.ResponseWriter, err error) {
	f.mu.Lock()
	f.info.CapacityRejections++
	f.info.LastError = err.Error()
	f.mu.Unlock()
	http.Error(w, "proxy capacity exhausted", http.StatusServiceUnavailable)
}

type flushingProxyWriter struct {
	io.Writer
	controller *http.ResponseController
}

func (w flushingProxyWriter) Write(p []byte) (int, error) {
	n, err := w.Writer.Write(p)
	if err == nil {
		err = w.controller.Flush()
	}
	return n, err
}

func relayHTTPSConnect(w http.ResponseWriter, request io.ReadCloser, upstream net.Conn) {
	controller := http.NewResponseController(w)
	w.WriteHeader(http.StatusOK)
	if err := controller.Flush(); err != nil {
		return
	}
	sent := make(chan struct{})
	go func() {
		defer close(sent)
		_, err := io.Copy(upstream, request)
		if err != nil || transport.CloseWrite(upstream) != nil {
			_ = upstream.Close()
		}
	}()
	// Only the handler writes the response. A client END_STREAM still lets
	// the origin reply; an origin EOF completes this HTTP response.
	_, _ = io.Copy(flushingProxyWriter{Writer: w, controller: controller}, upstream)
	_ = upstream.Close()
	_ = request.Close()
	<-sent
}
