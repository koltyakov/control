package transport

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"sync"

	"github.com/hashicorp/yamux"
)

const TCPListenerProtocol = "tcp-listen-v1"
const MaxTCPListenerConnections = 128

// OpenTCPListener binds a remote port without granting the receiver access to
// arbitrary services on the caller. Only sockets on this stream are forwarded.
func (p *Peer) OpenTCPListener(ctx context.Context, target, listen string) (net.Conn, string, error) {
	conn, result, err := p.OpenRPC(ctx, target, "tcp.listen", map[string]any{"listen": listen, "protocol": TCPListenerProtocol})
	if err != nil {
		return nil, "", err
	}
	var ack struct {
		Listen   string `json:"listen"`
		Protocol string `json:"protocol"`
	}
	err = json.Unmarshal(result, &ack)
	if err == nil && (ack.Protocol != TCPListenerProtocol || ack.Listen == "") {
		err = errors.New("unsupported TCP listener protocol")
	}
	if err != nil {
		_ = conn.Close()
		return nil, "", err
	}
	return conn, ack.Listen, nil
}

type tcpListener struct {
	mux  *yamux.Session
	addr net.Addr
	stop func() bool
}

func (l *tcpListener) Accept() (net.Conn, error) {
	conn, err := l.mux.AcceptStream()
	if err != nil {
		return nil, err
	}
	return NewDuplexConn(conn), nil
}

func (l *tcpListener) Addr() net.Addr { return l.addr }
func (l *tcpListener) Close() error {
	l.stop()
	return l.mux.Close()
}

// NewTCPListener receives sockets multiplexed within an authorized tcp.listen
// stream. EOF records preserve TCP half-close independently of yamux FIN.
func NewTCPListener(ctx context.Context, conn net.Conn, address string) (net.Listener, error) {
	addr, err := net.ResolveTCPAddr("tcp", address)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	mux, err := yamux.Client(newJoinConn(conn), muxConfig(ControlLane))
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	l := &tcpListener{mux: mux, addr: addr}
	l.stop = context.AfterFunc(ctx, func() { _ = mux.Close() })
	return l, nil
}

// ServeTCPListener owns the bound listener and all of its sockets. Losing the
// requesting stream closes them even while no TCP connections are active.
func ServeTCPListener(ctx context.Context, stream net.Conn, listener net.Listener, wrap func(net.Conn) net.Conn) error {
	defer func() { _ = listener.Close() }()
	mux, err := yamux.Server(newJoinConn(stream), muxConfig(ControlLane))
	if err != nil {
		return err
	}
	defer func() { _ = mux.Close() }()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	watchDone := make(chan struct{})
	go func() {
		defer close(watchDone)
		select {
		case <-ctx.Done():
		case <-mux.CloseChan():
		}
		cancel()
		_ = listener.Close()
		_ = mux.Close()
	}()
	var wg sync.WaitGroup
	defer func() { cancel(); <-watchDone; wg.Wait() }()
	slots := make(chan struct{}, MaxTCPListenerConnections)
	for {
		conn, err := listener.Accept()
		if err != nil {
			return err
		}
		select {
		case slots <- struct{}{}:
		default:
			_ = conn.Close()
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-slots; _ = conn.Close() }()
			remote, err := mux.OpenStream()
			if err != nil {
				return
			}
			defer abortStream(remote)
			Bridge(ctx, wrap(NewDuplexConn(remote)), conn)
		}()
	}
}
