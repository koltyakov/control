package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/koltyakov/control/internal/transport"
)

// SOCKS5 (RFC 1928) mode of a process-owned forward: one local listener, one
// CONNECT per accepted socket, each opened as an ordinary tcp.open stream.
const (
	DefaultSOCKSConnections = 256
	MaxSOCKSConnections     = 384
	socksWaiters            = 32
	socksSlotWait           = 500 * time.Millisecond
	socksQueuedHandshake    = time.Second
	socksOverflowTimeout    = 100 * time.Millisecond

	socksVersion          = 5
	socksHandshakeTimeout = 10 * time.Second

	socksMethodNone         = 0x00
	socksMethodUnacceptable = 0xff

	socksCommandConnect = 0x01

	socksAddressIPv4   = 0x01
	socksAddressDomain = 0x03
	socksAddressIPv6   = 0x04

	socksReplySucceeded          = 0x00
	socksReplyGeneralFailure     = 0x01
	socksReplyNotAllowed         = 0x02
	socksReplyNetworkUnreachable = 0x03
	socksReplyHostUnreachable    = 0x04
	socksReplyConnectionRefused  = 0x05
	socksReplyCommandUnsupported = 0x07
	socksReplyAddressUnsupported = 0x08
)

var (
	errSOCKSNonLoopback     = errors.New("SOCKS listeners default to loopback; a non-loopback SOCKS listener is an unauthenticated open proxy into the fleet and requires allowNonLoopback (CLI --allow-non-loopback)")
	errSOCKSInstructionOnly = errors.New("SOCKS forwards are refused for instruction-only destinations, such as Hybrid System companions, because each connection there needs an exact grant for an explicitly chosen address; use an ordinary forward to a fixed HOST:PORT")
)

func validateSOCKSForward(spec ForwardSpec) error {
	if spec.MaxConnections < 0 || spec.MaxConnections > MaxSOCKSConnections {
		return fmt.Errorf("maxConnections must be between 1 and %d, or zero for the default", MaxSOCKSConnections)
	}
	if spec.Reverse {
		return errors.New("SOCKS mode cannot be combined with reverse forwarding")
	}
	if spec.Address != "" {
		return errors.New("SOCKS forwards take each destination from the SOCKS request; omit the destination address")
	}
	if spec.Listen == "" || spec.AllowNonLoopback {
		return nil
	}
	host, _, err := net.SplitHostPort(spec.Listen)
	if err != nil {
		return fmt.Errorf("listen address: %w", err)
	}
	// The bound address is checked again after listening, which also covers
	// a localhost name that resolves elsewhere.
	if ip, err := netip.ParseAddr(host); host == "localhost" || (err == nil && ip.Unmap().IsLoopback()) {
		return nil
	}
	return errSOCKSNonLoopback
}

func loopbackListener(addr net.Addr) bool {
	tcp, ok := addr.(*net.TCPAddr)
	return ok && tcp.IP.IsLoopback()
}

// checkSOCKSTarget refuses instruction-only destinations before binding a
// listener. openSOCKSPeerTCP repeats the check for every socket.
func (c Client) checkSOCKSTarget(ctx context.Context, target string) error {
	peer, err := c.backend(ctx)
	if err != nil {
		return err
	}
	if peer != nil {
		destination, err := peer.Lookup(ctx, target)
		if err != nil {
			return err
		}
		if destination.InstructionOnly {
			return errSOCKSInstructionOnly
		}
		return nil
	}
	data, err := c.Machines(ctx, MachineQuery{Node: target, Compact: true})
	if err != nil {
		return err
	}
	var machines []machineSummary
	if err = json.Unmarshal(data, &machines); err != nil {
		return err
	}
	for _, m := range machines {
		if m.InstructionOnly {
			return errSOCKSInstructionOnly
		}
	}
	return nil
}

// openSOCKSPeerTCP never prepares a grant. Unlike openPeerTCP, the address
// comes from an arbitrary local SOCKS client rather than an instruction the
// user named, so an instruction-only destination is refused instead.
func openSOCKSPeerTCP(ctx context.Context, peer *transport.Peer, target, address string) (net.Conn, error) {
	destination, err := peer.Lookup(ctx, target)
	if err != nil {
		return nil, err
	}
	if destination.InstructionOnly {
		return nil, errSOCKSInstructionOnly
	}
	return peer.OpenTCP(ctx, destination.ID, address)
}

func (f *Forward) recordError(err error) {
	message := err.Error()
	if len(message) > 512 {
		message = message[:512]
	}
	f.mu.Lock()
	f.info.LastError = message
	f.mu.Unlock()
}

func (f *Forward) rejectSOCKSCapacity(local net.Conn, err error) {
	if f.ctx.Err() != nil {
		return
	}
	f.mu.Lock()
	f.info.CapacityRejections++
	f.info.LastError = err.Error()
	f.mu.Unlock()
	writeSOCKSCapacityReply(local)
}

func writeSOCKSCapacityReply(conn net.Conn) {
	_ = conn.SetWriteDeadline(time.Now().Add(socksOverflowTimeout))
	_, _ = conn.Write(socksReply(socksReplyGeneralFailure))
}

// Admitted sockets hold a slot from accept. A bounded overflow can negotiate
// SOCKS, then wait briefly for a slot without opening any peer stream.
func (f *Forward) bridgeSOCKS(c Client, local net.Conn, admitted bool) {
	defer f.wg.Done()
	defer func() {
		_ = local.Close()
		f.mu.Lock()
		if admitted {
			<-f.socksSlots
		}
		delete(f.connections, local)
		f.mu.Unlock()
	}()
	handshakeTimeout := socksHandshakeTimeout
	if !admitted {
		handshakeTimeout = socksQueuedHandshake
	}
	address, err := socksHandshake(local, handshakeTimeout)
	if err != nil {
		f.recordError(fmt.Errorf("SOCKS handshake: %w", err))
		return
	}
	if !admitted {
		timer := time.NewTimer(socksSlotWait)
		defer timer.Stop()
		select {
		case f.socksSlots <- struct{}{}:
			admitted = true
		case <-timer.C:
			f.rejectSOCKSCapacity(local, errors.New("SOCKS connection limit reached"))
			return
		case <-f.ctx.Done():
			return
		}
	}
	if f.ctx.Err() != nil {
		return
	}
	remote, err := c.tunnel(f.ctx, f.info.Node, address, openSOCKSPeerTCP)
	if err != nil {
		if transport.IsStreamCapacity(err) {
			f.rejectSOCKSCapacity(local, err)
			return
		}
		f.recordError(err)
		_ = local.SetWriteDeadline(time.Now().Add(socksHandshakeTimeout))
		_, _ = local.Write(socksReply(socksReplyCode(err)))
		return
	}
	f.mu.Lock()
	f.connections[local] = remote
	f.mu.Unlock()
	defer func() { _ = remote.Close() }()
	_ = local.SetWriteDeadline(time.Now().Add(socksHandshakeTimeout))
	if _, err = local.Write(socksReply(socksReplySucceeded)); err != nil {
		return
	}
	if err = local.SetWriteDeadline(time.Time{}); err != nil {
		return
	}
	transport.Bridge(f.ctx, local, remote)
}

// socksHandshake bounds the whole negotiation by one deadline and leaves any
// pipelined application bytes unread in conn.
func socksHandshake(conn net.Conn, timeout time.Duration) (string, error) {
	if err := conn.SetDeadline(time.Now().Add(timeout)); err != nil {
		return "", err
	}
	address, reply, err := readSOCKSRequest(conn, conn)
	if err != nil {
		if reply != socksReplySucceeded {
			_, _ = conn.Write(socksReply(reply))
		}
		return "", err
	}
	return address, conn.SetDeadline(time.Time{})
}

// readSOCKSRequest negotiates "no authentication" and parses one request with
// fixed-size reads. On error, a nonzero reply is the code to send before
// closing; zero means no request reply is due. Domain names stay unresolved.
func readSOCKSRequest(r io.Reader, w io.Writer) (string, byte, error) {
	var buf [255]byte
	if _, err := io.ReadFull(r, buf[:2]); err != nil {
		return "", 0, err
	}
	if buf[0] != socksVersion {
		return "", 0, fmt.Errorf("unsupported SOCKS version %d", buf[0])
	}
	methods := buf[:buf[1]]
	if _, err := io.ReadFull(r, methods); err != nil {
		return "", 0, err
	}
	method := byte(socksMethodUnacceptable)
	for _, m := range methods {
		if m == socksMethodNone {
			method = socksMethodNone
		}
	}
	if _, err := w.Write([]byte{socksVersion, method}); err != nil {
		return "", 0, err
	}
	if method != socksMethodNone {
		return "", 0, errors.New("SOCKS client offered no supported authentication method")
	}
	if _, err := io.ReadFull(r, buf[:4]); err != nil {
		return "", 0, err
	}
	if buf[0] != socksVersion || buf[2] != 0 {
		return "", socksReplyGeneralFailure, errors.New("malformed SOCKS request")
	}
	command := buf[1]
	var host string
	switch buf[3] {
	case socksAddressIPv4:
		if _, err := io.ReadFull(r, buf[:4]); err != nil {
			return "", 0, err
		}
		host = netip.AddrFrom4([4]byte(buf[:4])).String()
	case socksAddressIPv6:
		if _, err := io.ReadFull(r, buf[:16]); err != nil {
			return "", 0, err
		}
		host = netip.AddrFrom16([16]byte(buf[:16])).String()
	case socksAddressDomain:
		if _, err := io.ReadFull(r, buf[:1]); err != nil {
			return "", 0, err
		}
		name := buf[:buf[0]]
		if _, err := io.ReadFull(r, name); err != nil {
			return "", 0, err
		}
		if !validSOCKSDomain(name) {
			return "", socksReplyGeneralFailure, errors.New("invalid SOCKS domain name")
		}
		host = string(name)
	default:
		return "", socksReplyAddressUnsupported, fmt.Errorf("unsupported SOCKS address type %d", buf[3])
	}
	if _, err := io.ReadFull(r, buf[:2]); err != nil {
		return "", 0, err
	}
	port := int(buf[0])<<8 | int(buf[1])
	if command != socksCommandConnect {
		return "", socksReplyCommandUnsupported, fmt.Errorf("unsupported SOCKS command %d", command)
	}
	if port == 0 {
		return "", socksReplyGeneralFailure, errors.New("invalid SOCKS destination port 0")
	}
	return net.JoinHostPort(host, strconv.Itoa(port)), 0, nil
}

// validSOCKSDomain accepts DNS names and unzoned IP literals that some
// clients send as names. Anything else cannot be a dialable host.
func validSOCKSDomain(name []byte) bool {
	if len(name) == 0 {
		return false
	}
	plain := func(b byte) bool {
		return b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9' || b == '-' || b == '.' || b == '_'
	}
	if !slices.ContainsFunc(name, func(b byte) bool { return !plain(b) }) {
		return true
	}
	ip, err := netip.ParseAddr(string(name))
	return err == nil && ip.Zone() == ""
}

// socksReply reports no bound address; the worker's local endpoint is not
// returned by tcp.open.
func socksReply(code byte) []byte {
	return []byte{socksVersion, code, 0, socksAddressIPv4, 0, 0, 0, 0, 0, 0}
}

// socksReplyCode maps dial and authorization failures, which can arrive as
// remote error text from any worker OS, to RFC 1928 reply codes.
func socksReplyCode(err error) byte {
	if errors.Is(err, errSOCKSInstructionOnly) {
		return socksReplyNotAllowed
	}
	message := strings.ToLower(err.Error())
	has := func(parts ...string) bool {
		for _, part := range parts {
			if strings.Contains(message, part) {
				return true
			}
		}
		return false
	}
	switch {
	case has("connection refused", "actively refused"):
		return socksReplyConnectionRefused
	case has("network is unreachable", "unreachable network"):
		return socksReplyNetworkUnreachable
	case has("no such host", "host is unreachable", "unreachable host", "no route to host", "host is down", "i/o timeout", "server misbehaving", "timed out", "did not properly respond"):
		return socksReplyHostUnreachable
	case has("permission denied", "access permissions", "not authorized", "unauthorized", "forbidden", "outside this fleet", "requires orchestrator delegation", "delegat", "not allowed", "disabled"):
		return socksReplyNotAllowed
	}
	return socksReplyGeneralFailure
}
