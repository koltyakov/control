package testutil

import (
	"bufio"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// ConnectProxy is a minimal CONNECT-only HTTP proxy for gateway route tests.
// It forwards only allowlisted targets, resolving them itself, so a child
// that cannot resolve or reach the gateway can still connect through it.
type ConnectProxy struct {
	URL       string
	listener  net.Listener
	resolve   map[string]string
	mu        sync.Mutex
	targets   []string
	plaintext int
	conns     map[net.Conn]bool
	closed    bool
	wg        sync.WaitGroup
}

// NewConnectProxy maps CONNECT host:port targets to dial addresses. Any other
// target is refused with 403, like a proxy with a destination allowlist.
func NewConnectProxy(t testing.TB, resolve map[string]string) *ConnectProxy {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &ConnectProxy{URL: "http://" + listener.Addr().String(), listener: listener, resolve: resolve, conns: map[net.Conn]bool{}}
	p.wg.Add(1)
	go p.serve()
	t.Cleanup(p.Close)
	return p
}

// Targets returns every CONNECT target requested so far, including refused ones.
func (p *ConnectProxy) Targets() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.targets...)
}

// PlaintextRequests counts non-CONNECT requests, without retaining their
// headers or bodies. Gateway credentials must never reach this path.
func (p *ConnectProxy) PlaintextRequests() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.plaintext
}

// Close stops the listener and every tunnel, simulating a proxy outage.
func (p *ConnectProxy) Close() {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	_ = p.listener.Close()
	for c := range p.conns {
		_ = c.Close()
	}
	p.mu.Unlock()
	p.wg.Wait()
}

func (p *ConnectProxy) track(c net.Conn) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		_ = c.Close()
		return false
	}
	p.conns[c] = true
	return true
}

func (p *ConnectProxy) untrack(c net.Conn) {
	p.mu.Lock()
	delete(p.conns, c)
	p.mu.Unlock()
	_ = c.Close()
}

func (p *ConnectProxy) serve() {
	defer p.wg.Done()
	for {
		c, err := p.listener.Accept()
		if err != nil {
			return
		}
		if !p.track(c) {
			continue
		}
		p.wg.Add(1)
		go func() {
			defer p.wg.Done()
			defer p.untrack(c)
			p.tunnel(c)
		}()
	}
}

func (p *ConnectProxy) tunnel(c net.Conn) {
	_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
	reader := bufio.NewReader(c)
	req, err := http.ReadRequest(reader)
	if err != nil {
		return
	}
	_ = c.SetReadDeadline(time.Time{})
	if req.Method != http.MethodConnect {
		p.mu.Lock()
		p.plaintext++
		p.mu.Unlock()
		_, _ = io.WriteString(c, "HTTP/1.1 405 Method Not Allowed\r\nContent-Length: 0\r\n\r\n")
		return
	}
	p.mu.Lock()
	p.targets = append(p.targets, req.Host)
	address, allowed := p.resolve[req.Host]
	p.mu.Unlock()
	if !allowed {
		_, _ = io.WriteString(c, "HTTP/1.1 403 Forbidden\r\nContent-Length: 0\r\n\r\n")
		return
	}
	upstream, err := net.DialTimeout("tcp", address, 5*time.Second)
	if err != nil {
		_, _ = io.WriteString(c, "HTTP/1.1 502 Bad Gateway\r\nContent-Length: 0\r\n\r\n")
		return
	}
	if !p.track(upstream) {
		return
	}
	defer p.untrack(upstream)
	if _, err = io.WriteString(c, "HTTP/1.1 200 Connection established\r\n\r\n"); err != nil {
		return
	}
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(upstream, reader); done <- struct{}{} }()
	go func() { _, _ = io.Copy(c, upstream); done <- struct{}{} }()
	<-done
}

// TLSServer serves handler with a leaf certificate for hosts, 127.0.0.1, and
// ::1, issued by a fresh test CA. Callers trust the returned pool explicitly;
// certificate verification stays enabled.
func TLSServer(t testing.TB, handler http.Handler, hosts ...string) (*httptest.Server, *x509.CertPool) {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	caTemplate := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Control test CA"}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(24 * time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	ca, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	leafTemplate := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "Control test gateway"}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(24 * time.Hour), DNSNames: hosts, IPAddresses: []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback}, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTemplate, ca, &leafKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(handler)
	server.TLS = &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{{Certificate: [][]byte{leafDER, caDER}, PrivateKey: leafKey}}}
	server.StartTLS()
	t.Cleanup(server.Close)
	pool := x509.NewCertPool()
	pool.AddCert(ca)
	return server, pool
}
