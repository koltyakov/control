package client

import (
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestHTTPSForwardValidation(t *testing.T) {
	t.Parallel()
	c := (Client{}).WithLifetime(t.Context())
	defer func() { _ = c.Close() }()
	for _, spec := range []ForwardSpec{
		{SOCKS: true}, {Reverse: true}, {Address: "example.org:443"},
		{Listen: "0.0.0.0:0"}, {AllowNonLoopback: true}, {MaxConnections: MaxProxyConnections + 1},
	} {
		spec.Node = "unused"
		spec.HTTPSProxy = true
		if _, err := c.StartForward(t.Context(), spec); err == nil {
			t.Errorf("accepted invalid proxy configuration %+v", spec)
		}
	}
	for _, authority := range []string{"example.org", "user@host:443", "[fe80::1%lo0]:443", "example.org:0", "example.org:65536", "https://example.org:443"} {
		if validHTTPSConnectAddress(authority) {
			t.Errorf("accepted bad CONNECT authority %q", authority)
		}
	}
	for _, authority := range []string{"example.org:443", "127.0.0.1:80", "[::1]:443"} {
		if !validHTTPSConnectAddress(authority) {
			t.Errorf("rejected valid CONNECT authority %q", authority)
		}
	}
	for authority, want := range map[string]string{
		"example.org": "example.org:80", "[::1]": "[::1]:80", "localhost:81": "localhost:81",
		"localhost:0": "", "user@example.org": "", "[example.org]": "", "::1": "", "[fe80::1%en0]": "", "": "",
	} {
		if got := plainHTTPProxyAddress(authority); got != want {
			t.Errorf("plain HTTP address for %q = %q, want %q", authority, got, want)
		}
	}
}

func TestHTTPSForward(t *testing.T) {
	t.Parallel()
	for _, relay := range []bool{true, false} {
		t.Run(fmt.Sprintf("relay=%t", relay), func(t *testing.T) {
			t.Parallel()
			ctx, c, _, _ := delegationClusterWithTimeout(t, relay, 45*time.Second, true)
			echo := echoListener(t, "tcp", "127.0.0.1:0")
			if echo == nil {
				t.Fatal("could not bind echo server")
			}
			target := net.JoinHostPort("localhost", fmt.Sprint(echo.Addr().(*net.TCPAddr).Port))
			if _, err := c.StartForward(ctx, ForwardSpec{Node: "second", HTTPSProxy: true}); !errors.Is(err, errProxyInstructionOnly) {
				t.Fatal("HTTPS proxy toward an instruction-only destination", err)
			}
			f, err := c.StartForward(ctx, ForwardSpec{Node: "first", HTTPSProxy: true, MaxConnections: 2})
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			info := f.Info()
			leaf := f.httpsProxy.server.TLSConfig.Certificates[0].Leaf
			hash := sha256.Sum256(leaf.RawSubjectPublicKeyInfo)
			if !info.HTTPSProxy || info.SOCKS || info.ProxyURL != "https://"+info.Listen || info.ConnectionLimit != 2 ||
				info.ProxySPKI != base64.StdEncoding.EncodeToString(hash[:]) {
				t.Fatalf("unexpected HTTPS proxy metadata: %+v", info)
			}
			roots := x509.NewCertPool()
			roots.AddCert(leaf)
			protocols := new(http.Protocols)
			protocols.SetHTTP2(true)
			transport := &http.Transport{Protocols: protocols, TLSClientConfig: &tls.Config{RootCAs: roots},
				HTTP2: &http.HTTP2Config{StrictMaxConcurrentRequests: true}}
			defer transport.CloseIdleConnections()
			do := func(method, authority string, body io.Reader, want int) *http.Response {
				t.Helper()
				req, err := http.NewRequestWithContext(ctx, method, info.ProxyURL, body)
				if err != nil {
					t.Fatal(err)
				}
				req.Host = authority
				response, err := transport.RoundTrip(req)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = response.Body.Close() })
				if response.StatusCode != want || response.ProtoMajor != 2 {
					t.Fatalf("got HTTP %s %s, want HTTP/2 %d", response.Proto, response.Status, want)
				}
				return response
			}
			_ = do(http.MethodGet, "localhost:0", nil, http.StatusBadRequest).Body.Close()
			_ = do(http.MethodConnect, "localhost", nil, http.StatusBadRequest).Body.Close()
			origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Proxy-Authorization") != "" || r.Header.Get("X-Forwarded-For") != "" {
					t.Error("proxy-only header reached the destination")
				}
				if r.URL.Path == "/redirect" {
					http.Redirect(w, r, "/other", http.StatusFound)
					return
				}
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
				}
				w.Header().Add("Set-Cookie", "first=1")
				w.Header().Add("Set-Cookie", "second=2")
				_, _ = fmt.Fprintf(w, "%s %s %s %s", r.Method, r.Host, r.URL.RequestURI(), body)
			}))
			defer origin.Close()
			originAuthority := "localhost:" + fmt.Sprint(origin.Listener.Addr().(*net.TCPAddr).Port)
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, info.ProxyURL+"/path?key=value", strings.NewReader("payload"))
			if err != nil {
				t.Fatal(err)
			}
			req.Host = originAuthority
			req.Header.Set("Proxy-Authorization", "test-only")
			req.Header.Set("X-Forwarded-For", "192.0.2.1")
			plain, err := transport.RoundTrip(req)
			if err != nil {
				t.Fatal(err)
			}
			body, err := io.ReadAll(plain.Body)
			_ = plain.Body.Close()
			if want := "POST " + originAuthority + " /path?key=value payload"; err != nil ||
				plain.ProtoMajor != 2 || plain.StatusCode != http.StatusOK || string(body) != want ||
				len(plain.Header.Values("Set-Cookie")) != 2 {
				t.Fatalf("plain HTTP response %s %q, want %q: %v", plain.Status, body, want, err)
			}
			redirectReq, err := http.NewRequestWithContext(ctx, http.MethodGet, info.ProxyURL+"/redirect", nil)
			if err != nil {
				t.Fatal(err)
			}
			redirectReq.Host = originAuthority
			redirect, err := transport.RoundTrip(redirectReq)
			if err != nil {
				t.Fatal(err)
			}
			_ = redirect.Body.Close()
			if redirect.StatusCode != http.StatusFound || redirect.Header.Get("Location") != "/other" {
				t.Fatalf("proxy followed or changed the origin redirect: %s %s", redirect.Status, redirect.Header.Get("Location"))
			}
			open := func() (*io.PipeWriter, io.ReadCloser) {
				reader, writer := io.Pipe()
				t.Cleanup(func() { _ = writer.Close(); _ = reader.Close() })
				return writer, do(http.MethodConnect, target, reader, http.StatusOK).Body
			}
			w1, r1 := open()
			w2, r2 := open()
			f.mu.Lock()
			carriers := len(f.connections)
			f.mu.Unlock()
			if carriers != 1 || f.Info().Active != 2 {
				t.Fatalf("expected two CONNECTs inside one browser-side TLS connection: %+v, carriers=%d", f.Info(), carriers)
			}
			started := time.Now()
			_ = do(http.MethodConnect, target, nil, http.StatusServiceUnavailable).Body.Close()
			if f.Info().CapacityRejections != 1 || time.Since(started) < httpsProxySlotWait {
				t.Fatal("capacity rejection was not counted or bounded", f.Info())
			}
			for i, s := range []struct {
				w *io.PipeWriter
				r io.ReadCloser
			}{{w1, r1}, {w2, r2}} {
				text := fmt.Sprintf("stream %d", i)
				if _, err := io.WriteString(s.w, text); err != nil {
					t.Fatal(err)
				}
				got := make([]byte, len(text))
				if _, err := io.ReadFull(s.r, got); err != nil || string(got) != text {
					t.Fatalf("echo %q, want %q: %v", got, text, err)
				}
			}
			_ = w1.Close()
			if _, err := io.ReadAll(r1); err != nil {
				t.Fatal("graceful client END_STREAM did not complete the response:", err)
			}
			done := make(chan struct{})
			go func() { f.Close(); close(done) }()
			select {
			case <-done:
			case <-ctx.Done():
				t.Fatal("HTTPS proxy did not close while a CONNECT was open")
			}
			if got := f.Info(); got.Active != 0 || got.WaitingConnections != 0 || got.CapacityRejections != 1 {
				t.Fatalf("proxy did not release requests on stop: %+v", got)
			}
		})
	}
}
