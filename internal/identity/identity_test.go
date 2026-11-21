package identity

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadPersistsIdentity(t *testing.T) {
	dir := t.TempDir()
	first, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != second.ID || first.ID != ID(first.Public) {
		t.Fatalf("identity changed across loads: %s %s", first.ID, second.ID)
	}
	if err := os.WriteFile(filepath.Join(dir, "identity.key"), []byte("short"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dir); err == nil {
		t.Fatal("expected invalid key error")
	}
}

func TestCertificateValidityToleratesSkewAndUptime(t *testing.T) {
	id, err := Generate()
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(id.cert.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	// Older peers still check the window against their own clocks.
	for _, clock := range []time.Time{time.Now().AddDate(-10, 0, 0), time.Now().AddDate(50, 0, 0)} {
		if clock.Before(cert.NotBefore) || clock.After(cert.NotAfter) {
			t.Fatalf("certificate rejected by a peer clock at %s: %s..%s", clock, cert.NotBefore, cert.NotAfter)
		}
	}
}

// handshake runs a mutual TLS handshake where each side pins expected IDs.
// Buffered loopback sockets keep post-handshake writes from blocking.
func handshake(t *testing.T, server, client *Identity, serverExpects, clientExpects string) (serverErr, clientErr error) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	done := make(chan error, 1)
	go func() {
		raw, err := listener.Accept()
		if err != nil {
			done <- err
			return
		}
		defer func() { _ = raw.Close() }()
		_ = raw.SetDeadline(time.Now().Add(5 * time.Second))
		done <- tls.Server(raw, server.TLS(serverExpects)).Handshake()
	}()
	raw, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = raw.Close() }()
	_ = raw.SetDeadline(time.Now().Add(5 * time.Second))
	conn := tls.Client(raw, client.TLS(clientExpects))
	clientErr = conn.Handshake()
	if clientErr == nil {
		// TLS 1.3 clients finish before the server verifies them; a read
		// surfaces the server's rejection alert.
		_ = raw.SetReadDeadline(time.Now().Add(time.Second))
		var b [1]byte
		_, _ = conn.Read(b[:])
	}
	_ = raw.Close()
	return <-done, clientErr
}

func TestTLSPinsPeerIdentityBothWays(t *testing.T) {
	server, err := Generate()
	if err != nil {
		t.Fatal(err)
	}
	client, err := Generate()
	if err != nil {
		t.Fatal(err)
	}
	other, err := Generate()
	if err != nil {
		t.Fatal(err)
	}
	if serr, cerr := handshake(t, server, client, client.ID, server.ID); serr != nil || cerr != nil {
		t.Fatalf("pinned handshake failed: server=%v client=%v", serr, cerr)
	}
	if _, cerr := handshake(t, server, client, client.ID, other.ID); cerr == nil {
		t.Fatal("client accepted an unexpected server key")
	}
	if serr, _ := handshake(t, server, client, other.ID, server.ID); serr == nil {
		t.Fatal("server accepted an unexpected client key")
	}
}

func TestTLSAcceptsPinnedPeerOutsideValidityWindow(t *testing.T) {
	server, err := Generate()
	if err != nil {
		t.Fatal(err)
	}
	// An older peer issued a short window that this clock considers expired.
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	legacy, err := fromKey(key)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: legacy.ID},
		NotBefore: time.Now().AddDate(-2, 0, 0), NotAfter: time.Now().AddDate(-1, 0, 0),
		KeyUsage:    x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, legacy.Public, legacy.Private)
	if err != nil {
		t.Fatal(err)
	}
	legacy.cert = tls.Certificate{Certificate: [][]byte{der}, PrivateKey: legacy.Private}
	if serr, cerr := handshake(t, server, legacy, legacy.ID, server.ID); serr != nil || cerr != nil {
		t.Fatalf("expired-window pinned peer rejected: server=%v client=%v", serr, cerr)
	}
}

func TestTLSRejectsMultipleCertificates(t *testing.T) {
	server, err := Generate()
	if err != nil {
		t.Fatal(err)
	}
	client, err := Generate()
	if err != nil {
		t.Fatal(err)
	}
	extra, err := Generate()
	if err != nil {
		t.Fatal(err)
	}
	chained := *client
	chained.cert = tls.Certificate{Certificate: [][]byte{client.cert.Certificate[0], extra.cert.Certificate[0]}, PrivateKey: client.Private}
	serr, _ := handshake(t, server, &chained, client.ID, server.ID)
	if serr == nil {
		t.Fatal("server accepted a multi-certificate chain")
	}
}
