package client

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/koltyakov/control/internal/model"
)

func socksRequest(command, addressType byte, host []byte, port uint16) []byte {
	request := []byte{socksVersion, command, 0, addressType}
	if addressType == socksAddressDomain {
		request = append(request, byte(len(host)))
	}
	request = append(request, host...)
	return binary.BigEndian.AppendUint16(request, port)
}

func TestSOCKSRequestParser(t *testing.T) {
	t.Parallel()
	greeting := []byte{socksVersion, 1, socksMethodNone}
	connect := func(addressType byte, host []byte, port uint16) []byte {
		return append(append([]byte{}, greeting...), socksRequest(socksCommandConnect, addressType, host, port)...)
	}
	ipv6 := net.ParseIP("2001:db8::1").To16()
	long := bytes.Repeat([]byte("a"), 255)
	cases := []struct {
		name    string
		input   []byte
		address string
		method  []byte
		reply   byte
	}{
		{"ipv4", connect(socksAddressIPv4, []byte{10, 1, 2, 3}, 22), "10.1.2.3:22", []byte{5, 0}, 0},
		{"ipv6", connect(socksAddressIPv6, ipv6, 443), "[2001:db8::1]:443", []byte{5, 0}, 0},
		{"domain stays unresolved", connect(socksAddressDomain, []byte("db.internal"), 5432), "db.internal:5432", []byte{5, 0}, 0},
		{"maximum domain", connect(socksAddressDomain, long, 80), string(long) + ":80", []byte{5, 0}, 0},
		{"ipv6 literal as domain", connect(socksAddressDomain, []byte("::1"), 80), "[::1]:80", []byte{5, 0}, 0},
		{"no auth among several methods", append([]byte{5, 3, 2, 1, 0}, socksRequest(socksCommandConnect, socksAddressIPv4, []byte{127, 0, 0, 1}, 1)...), "127.0.0.1:1", []byte{5, 0}, 0},
		{"zoned literal domain", connect(socksAddressDomain, []byte("fe80::1%en0"), 80), "", []byte{5, 0}, socksReplyGeneralFailure},
		{"domain with separator", connect(socksAddressDomain, []byte("host:99"), 80), "", []byte{5, 0}, socksReplyGeneralFailure},
		{"empty domain", connect(socksAddressDomain, nil, 80), "", []byte{5, 0}, socksReplyGeneralFailure},
		{"port zero", connect(socksAddressIPv4, []byte{10, 0, 0, 1}, 0), "", []byte{5, 0}, socksReplyGeneralFailure},
		{"bind", append(append([]byte{}, greeting...), socksRequest(0x02, socksAddressIPv4, []byte{10, 0, 0, 1}, 80)...), "", []byte{5, 0}, socksReplyCommandUnsupported},
		{"udp associate", append(append([]byte{}, greeting...), socksRequest(0x03, socksAddressIPv4, []byte{10, 0, 0, 1}, 80)...), "", []byte{5, 0}, socksReplyCommandUnsupported},
		{"unknown address type", append(append([]byte{}, greeting...), 5, 1, 0, 9), "", []byte{5, 0}, socksReplyAddressUnsupported},
		{"reserved byte", append(append([]byte{}, greeting...), 5, 1, 1, 1, 10, 0, 0, 1, 0, 80), "", []byte{5, 0}, socksReplyGeneralFailure},
		{"request version", append(append([]byte{}, greeting...), 4, 1, 0, 1, 10, 0, 0, 1, 0, 80), "", []byte{5, 0}, socksReplyGeneralFailure},
		{"username/password only", []byte{5, 1, 2}, "", []byte{5, 0xff}, 0},
		{"no methods", []byte{5, 0}, "", []byte{5, 0xff}, 0},
		{"socks4", []byte{4, 1, 0, 80, 10, 0, 0, 1, 0}, "", nil, 0},
		{"empty", nil, "", nil, 0},
	}
	for _, tc := range cases {
		var written bytes.Buffer
		address, reply, err := readSOCKSRequest(bytes.NewReader(tc.input), &written)
		if address != tc.address || reply != tc.reply || (err == nil) != (tc.address != "") || !bytes.Equal(written.Bytes(), tc.method) {
			t.Errorf("%s: address %q reply %d err %v wrote %v", tc.name, address, reply, err, written.Bytes())
		}
	}
	// Every truncated prefix of a valid request fails without a reply code.
	full := connect(socksAddressDomain, []byte("example.test"), 443)
	for n := range len(full) {
		if address, reply, err := readSOCKSRequest(bytes.NewReader(full[:n]), io.Discard); err == nil || reply != 0 || address != "" {
			t.Errorf("truncated at %d: address %q reply %d err %v", n, address, reply, err)
		}
	}
}

func FuzzSOCKSRequest(f *testing.F) {
	f.Add([]byte{5, 1, 0, 5, 1, 0, 1, 127, 0, 0, 1, 0, 80})
	f.Add([]byte{5, 1, 0, 5, 1, 0, 3, 4, 'h', 'o', 's', 't', 1, 187})
	f.Add([]byte{5, 2, 0, 2, 5, 3, 0, 4, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1, 0, 1})
	f.Fuzz(func(t *testing.T, data []byte) {
		var written bytes.Buffer
		address, reply, err := readSOCKSRequest(bytes.NewReader(data), &written)
		if written.Len() > 2 {
			t.Fatalf("method reply too long: %v", written.Bytes())
		}
		if err != nil {
			if address != "" {
				t.Fatalf("failed parse returned %q", address)
			}
			return
		}
		host, port, splitErr := net.SplitHostPort(address)
		if splitErr != nil || host == "" || port == "0" || reply != 0 {
			t.Fatalf("accepted undialable address %q reply %d: %v", address, reply, splitErr)
		}
	})
}

func TestSOCKSHandshakeTimeout(t *testing.T) {
	t.Parallel()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	for _, prefix := range [][]byte{nil, {5, 1, 0}, {5, 1, 0, 5, 1, 0, 3, 200, 'a'}} {
		client, err := net.Dial("tcp", listener.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		server, err := listener.Accept()
		if err != nil {
			t.Fatal(err)
		}
		if _, err = client.Write(prefix); err != nil {
			t.Fatal(err)
		}
		started := time.Now()
		_, err = socksHandshake(server, 100*time.Millisecond)
		if !errors.Is(err, os.ErrDeadlineExceeded) || time.Since(started) > 5*time.Second {
			t.Fatalf("prefix %v: stalled handshake returned %v after %v", prefix, err, time.Since(started))
		}
		_ = server.Close()
		_ = client.Close()
	}
}

func TestSOCKSReplyCodes(t *testing.T) {
	t.Parallel()
	cases := map[string]byte{
		"dial tcp 10.0.0.5:5432: connect: connection refused":                                                            socksReplyConnectionRefused,
		"dial tcp 10.0.0.5:5432: connectex: No connection could be made because the target machine actively refused it.": socksReplyConnectionRefused,
		"dial tcp 10.9.0.1:80: connect: network is unreachable":                                                          socksReplyNetworkUnreachable,
		"dial tcp: lookup db.internal: no such host":                                                                     socksReplyHostUnreachable,
		"dial tcp 10.0.0.9:22: connect: no route to host":                                                                socksReplyHostUnreachable,
		"dial tcp 10.0.0.9:22: i/o timeout":                                                                              socksReplyHostUnreachable,
		"caller is not authorized for tcp.open":                                                                          socksReplyNotAllowed,
		"system execution requires an exact instruction-bound delegation; upgrade the client or use peers.call":          socksReplyNotAllowed,
		"node is disabled": socksReplyNotAllowed,
		"peer offline":     socksReplyGeneralFailure,
	}
	for message, want := range cases {
		if got := socksReplyCode(&model.RPCError{Code: "remote_error", Message: message}); got != want {
			t.Errorf("%q: reply %d, want %d", message, got, want)
		}
	}
	if got := socksReplyCode(fmt.Errorf("wrapped: %w", errSOCKSInstructionOnly)); got != socksReplyNotAllowed {
		t.Errorf("instruction-only refusal reply %d", got)
	}
}

func TestSOCKSForwardValidation(t *testing.T) {
	t.Parallel()
	for _, spec := range []ForwardSpec{
		{Node: "w", SOCKS: true, Reverse: true},
		{Node: "w", SOCKS: true, Address: "db:5432"},
		{Node: "w", SOCKS: true, Listen: "0.0.0.0:1080"},
		{Node: "w", SOCKS: true, Listen: ":1080"},
		{Node: "w", SOCKS: true, Listen: "[::]:1080"},
		{Node: "w", SOCKS: true, Listen: "192.168.1.5:1080"},
		{Node: "w", SOCKS: true, Listen: "proxy.example:1080"},
		{Node: "w", SOCKS: true, MaxConnections: -1},
		{Node: "w", SOCKS: true, MaxConnections: MaxSOCKSConnections + 1},
	} {
		if validateSOCKSForward(spec) == nil {
			t.Errorf("accepted %+v", spec)
		}
	}
	for _, spec := range []ForwardSpec{
		{Node: "w", SOCKS: true},
		{Node: "w", SOCKS: true, Listen: "127.0.0.1:1080"},
		{Node: "w", SOCKS: true, Listen: "[::1]:0"},
		{Node: "w", SOCKS: true, Listen: "localhost:0"},
		{Node: "w", SOCKS: true, Listen: "0.0.0.0:1080", AllowNonLoopback: true},
		{Node: "w", SOCKS: true, MaxConnections: 1},
		{Node: "w", SOCKS: true, MaxConnections: MaxSOCKSConnections},
	} {
		if err := validateSOCKSForward(spec); err != nil {
			t.Errorf("refused %+v: %v", spec, err)
		}
	}
	persistent := PersistentTunnelSpec{ID: "proxy", ForwardSpec: ForwardSpec{Node: "w", Address: "db:5432", Listen: "127.0.0.1:1080", SOCKS: true}}
	if validatePersistentTunnel(persistent) == nil {
		t.Error("retained tunnel accepted SOCKS mode")
	}
	c := (Client{}).WithLifetime(t.Context())
	defer func() { _ = c.Close() }()
	if _, err := c.StartForward(t.Context(), ForwardSpec{Node: "w", Address: "db:5432", AllowNonLoopback: true}); err == nil {
		t.Error("ordinary forward accepted allowNonLoopback")
	}
	if _, err := c.StartForward(t.Context(), ForwardSpec{Node: "w", Address: "db:5432", MaxConnections: 2}); err == nil {
		t.Error("ordinary forward accepted maxConnections")
	}
}

// socksStart pipelines the request, then reads the no-auth selection. CONNECT
// can still be waiting for capacity when this returns.
func socksStart(t *testing.T, listen string, request []byte, payload []byte) net.Conn {
	t.Helper()
	conn, err := net.DialTimeout("tcp", listen, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.SetDeadline(time.Now().Add(20 * time.Second))
	// Pipeline greeting, request and first payload; the proxy must not over-read.
	message := append(append([]byte{socksVersion, 1, socksMethodNone}, request...), payload...)
	if _, err = conn.Write(message); err != nil {
		t.Fatal(err)
	}
	var method [2]byte
	if _, err = io.ReadFull(conn, method[:]); err != nil || method != [2]byte{socksVersion, socksMethodNone} {
		_ = conn.Close()
		t.Fatalf("SOCKS method %v: %v", method, err)
	}
	return conn
}

func socksResult(t *testing.T, conn net.Conn) byte {
	t.Helper()
	var reply [10]byte
	var err error
	if _, err = io.ReadFull(conn, reply[:]); err != nil {
		_ = conn.Close()
		t.Fatal("SOCKS reply", err)
	}
	if reply[0] != socksVersion || reply[2] != 0 || reply[3] != socksAddressIPv4 {
		t.Fatalf("malformed SOCKS reply %v", reply)
	}
	return reply[1]
}

func socksOpen(t *testing.T, listen string, request []byte, payload []byte) (net.Conn, byte) {
	t.Helper()
	conn := socksStart(t, listen, request, payload)
	return conn, socksResult(t, conn)
}

func echoListener(t *testing.T, network, address string) net.Listener {
	t.Helper()
	listener, err := net.Listen(network, address)
	if err != nil {
		return nil
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() { defer func() { _ = conn.Close() }(); _, _ = io.Copy(conn, conn) }()
		}
	}()
	return listener
}

func expectEcho(t *testing.T, conn net.Conn, first string) {
	t.Helper()
	if _, err := conn.Write([]byte("-more")); err != nil {
		t.Fatal(err)
	}
	want := first + "-more"
	got := make([]byte, len(want))
	if _, err := io.ReadFull(conn, got); err != nil || string(got) != want {
		t.Fatalf("echo %q, want %q: %v", got, want, err)
	}
}

func expectClosed(t *testing.T, conn net.Conn) {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	var b [1]byte
	if _, err := conn.Read(b[:]); err == nil || errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("socket stayed open: %v", err)
	}
}

func TestSOCKSForward(t *testing.T) {
	t.Parallel()
	for _, relay := range []bool{true, false} {
		t.Run(fmt.Sprintf("relay=%v", relay), func(t *testing.T) {
			t.Parallel()
			ctx, c, _, _ := delegationClusterWithTimeout(t, relay, 90*time.Second, true)
			echo := echoListener(t, "tcp", "127.0.0.1:0")
			port := uint16(echo.Addr().(*net.TCPAddr).Port)
			if _, err := c.StartForward(ctx, ForwardSpec{Node: "first", SOCKS: true, Reverse: true}); err == nil {
				t.Fatal("reverse SOCKS accepted")
			}
			if _, err := c.StartForward(ctx, ForwardSpec{Node: "first", SOCKS: true, Listen: "0.0.0.0:0"}); !errors.Is(err, errSOCKSNonLoopback) {
				t.Fatal("non-loopback SOCKS listener without opt-in", err)
			}
			// Instruction-only destinations get no automatic per-socket grant.
			if _, err := c.StartForward(ctx, ForwardSpec{Node: "second", SOCKS: true}); !errors.Is(err, errSOCKSInstructionOnly) {
				t.Fatal("SOCKS toward an instruction-only destination", err)
			}
			peer, err := c.backend(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if conn, err := openSOCKSPeerTCP(ctx, peer, "second", echo.Addr().String()); !errors.Is(err, errSOCKSInstructionOnly) {
				if conn != nil {
					_ = conn.Close()
				}
				t.Fatal("per-socket instruction-only check", err)
			}
			var grants []model.Delegation
			if err = c.Call(ctx, "second", "access.list", nil, &grants); err != nil || len(grants) != 0 {
				t.Fatal("SOCKS refusal issued System grants", grants, err)
			}

			f, err := c.StartForward(ctx, ForwardSpec{Node: "first", SOCKS: true})
			if err != nil {
				t.Fatal(err)
			}
			info := f.Info()
			if !info.SOCKS || info.Address != "" || !strings.HasPrefix(info.Listen, "127.0.0.1:") || info.ConnectionLimit != 256 {
				t.Fatal("SOCKS forward metadata", info)
			}
			if listed := c.Forwards(); len(listed) != 1 || !listed[0].SOCKS {
				t.Fatal("SOCKS forward listing", listed)
			}
			conn, reply := socksOpen(t, info.Listen, socksRequest(socksCommandConnect, socksAddressIPv4, []byte{127, 0, 0, 1}, port), []byte("ipv4"))
			if reply != socksReplySucceeded {
				t.Fatal("IPv4 CONNECT reply", reply)
			}
			expectEcho(t, conn, "ipv4")
			_ = conn.Close()
			conn, reply = socksOpen(t, info.Listen, socksRequest(socksCommandConnect, socksAddressDomain, []byte("localhost"), port), []byte("domain"))
			if reply != socksReplySucceeded {
				t.Fatal("domain CONNECT reply", reply)
			}
			expectEcho(t, conn, "domain")
			// Keep this stream open across stop.
			connected := conn
			if echo6 := echoListener(t, "tcp", "[::1]:0"); echo6 != nil {
				conn, reply = socksOpen(t, info.Listen, socksRequest(socksCommandConnect, socksAddressIPv6, net.IPv6loopback, uint16(echo6.Addr().(*net.TCPAddr).Port)), []byte("ipv6"))
				if reply != socksReplySucceeded {
					t.Fatal("IPv6 CONNECT reply", reply)
				}
				expectEcho(t, conn, "ipv6")
				_ = conn.Close()
			}
			closed, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			closedPort := uint16(closed.Addr().(*net.TCPAddr).Port)
			_ = closed.Close()
			conn, reply = socksOpen(t, info.Listen, socksRequest(socksCommandConnect, socksAddressIPv4, []byte{127, 0, 0, 1}, closedPort), nil)
			if reply != socksReplyConnectionRefused {
				t.Fatal("refused dial reply", reply)
			}
			expectClosed(t, conn)
			_ = conn.Close()
			if lastError := f.Info().LastError; !strings.Contains(lastError, "refused") {
				t.Fatal("dial failure not reported", lastError)
			}
			for _, command := range []byte{0x02, 0x03} {
				conn, reply = socksOpen(t, info.Listen, socksRequest(command, socksAddressIPv4, []byte{127, 0, 0, 1}, port), nil)
				if reply != socksReplyCommandUnsupported {
					t.Fatal("unsupported command reply", command, reply)
				}
				expectClosed(t, conn)
				_ = conn.Close()
			}
			raw, err := net.Dial("tcp", info.Listen)
			if err != nil {
				t.Fatal(err)
			}
			_ = raw.SetDeadline(time.Now().Add(10 * time.Second))
			if _, err = raw.Write([]byte{5, 1, 2}); err != nil {
				t.Fatal(err)
			}
			var method [2]byte
			if _, err = io.ReadFull(raw, method[:]); err != nil || method != [2]byte{5, 0xff} {
				t.Fatal("unsupported authentication reply", method, err)
			}
			expectClosed(t, raw)
			_ = raw.Close()
			for _, malformed := range [][]byte{{4, 1, 0, 80, 127, 0, 0, 1, 0}, {5, 1}} {
				raw, err = net.Dial("tcp", info.Listen)
				if err != nil {
					t.Fatal(err)
				}
				_, _ = raw.Write(malformed)
				_ = raw.(*net.TCPConn).CloseWrite()
				_ = raw.SetReadDeadline(time.Now().Add(10 * time.Second))
				// Closing with unread input may reset rather than end the stream.
				if data, err := io.ReadAll(raw); errors.Is(err, os.ErrDeadlineExceeded) || len(data) != 0 {
					t.Fatal("malformed handshake was answered", malformed, data, err)
				}
				_ = raw.Close()
			}

			// Hold 256 established CONNECTs across the same peer session,
			// exceeding both previous transport budgets.
			idle := make([]net.Conn, 0, DefaultSOCKSConnections)
			defer func() {
				for _, conn := range idle {
					_ = conn.Close()
				}
			}()
			deadline := time.Now().Add(20 * time.Second)
			// Only the connected stream remains once refused sockets are released.
			for f.Info().Active != 1 {
				if time.Now().After(deadline) {
					t.Fatal("released socket count", f.Info().Active)
				}
				time.Sleep(10 * time.Millisecond)
			}
			request := socksRequest(socksCommandConnect, socksAddressIPv4, []byte{127, 0, 0, 1}, port)
			for len(idle) < DefaultSOCKSConnections-1 {
				conn, reply := socksOpen(t, info.Listen, request, nil)
				if reply != socksReplySucceeded {
					t.Fatalf("CONNECT %d reply: %d", len(idle)+2, reply)
				}
				idle = append(idle, conn)
			}
			if got := f.Info().Active; got != DefaultSOCKSConnections {
				t.Fatal("socket count", got)
			}
			started := time.Now()
			over, reply := socksOpen(t, info.Listen, request, nil)
			if reply != socksReplyGeneralFailure || time.Since(started) < socksSlotWait {
				t.Fatal("over-capacity CONNECT did not wait and reply with general failure", reply)
			}
			expectClosed(t, over)
			_ = over.Close()
			if stats := f.Info(); stats.CapacityRejections != 1 || !strings.Contains(stats.LastError, "limit") {
				t.Fatal("socket cap not reported", stats)
			}
			waiting := socksStart(t, info.Listen, request, []byte("queued"))
			defer func() { _ = waiting.Close() }()
			_ = idle[0].Close()
			if reply := socksResult(t, waiting); reply != socksReplySucceeded {
				t.Fatal("waiting CONNECT was not admitted after a slot opened", reply)
			}
			expectEcho(t, waiting, "queued")

			stopped := make(chan error, 1)
			go func() { stopped <- c.StopForward(info.ID) }()
			select {
			case err := <-stopped:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("stop did not join SOCKS sockets")
			}
			select {
			case <-f.Done():
			default:
				t.Fatal("forward not done after stop")
			}
			expectClosed(t, connected)
			expectClosed(t, waiting)
			_ = connected.Close()
			for _, conn := range idle[1:] {
				expectClosed(t, conn)
			}
			if conn, err := net.DialTimeout("tcp", info.Listen, time.Second); err == nil {
				_ = conn.Close()
				t.Fatal("listener survived stop")
			}
			if len(c.Forwards()) != 0 {
				t.Fatal("stopped forward still listed")
			}

			small, err := c.StartForward(ctx, ForwardSpec{Node: "first", SOCKS: true, MaxConnections: 1})
			if err != nil {
				t.Fatal(err)
			}
			defer small.Close()
			if info := small.Info(); info.ConnectionLimit != 1 {
				t.Fatal("custom limit not reflected in metadata", info)
			}
			first, reply := socksOpen(t, small.Info().Listen, request, nil)
			defer func() { _ = first.Close() }()
			if reply != socksReplySucceeded {
				t.Fatal("custom limit did not permit first connection", reply)
			}
			second, reply := socksOpen(t, small.Info().Listen, request, nil)
			defer func() { _ = second.Close() }()
			if reply != socksReplyGeneralFailure || small.Info().CapacityRejections != 1 {
				t.Fatal("custom limit did not reject second connection", reply, small.Info())
			}
		})
	}
}

func TestSOCKSWaitQueueIsBounded(t *testing.T) {
	t.Parallel()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	f := &Forward{ctx: ctx, cancel: cancel, listener: listener, done: make(chan struct{}),
		connections: map[net.Conn]net.Conn{}, socksSlots: make(chan struct{}, 1), info: ForwardInfo{SOCKS: true, ConnectionLimit: 1}}
	f.wg.Add(1)
	go f.serve(Client{})
	defer f.Close()
	listen := listener.Addr().String()
	for range 1 + socksWaiters {
		conn, err := net.DialTimeout("tcp", listen, time.Second)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = conn.Close() }()
	}
	request := socksRequest(socksCommandConnect, socksAddressIPv4, []byte{127, 0, 0, 1}, 80)
	overflow, reply := socksOpen(t, listen, request, nil)
	defer func() { _ = overflow.Close() }()
	if reply != socksReplyGeneralFailure {
		t.Fatal("full wait queue did not reply with general failure", reply)
	}
	expectClosed(t, overflow)
	if info := f.Info(); info.Active != 1 || info.WaitingConnections != socksWaiters || info.CapacityRejections != 1 {
		t.Fatal("unexpected hard overflow accounting", info)
	}
	f.Close()
	if info := f.Info(); info.Active != 0 || info.WaitingConnections != 0 || info.CapacityRejections != 1 {
		t.Fatal("shutdown did not release queued connections", info)
	}
}
