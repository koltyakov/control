package transport

import (
	"context"
	"errors"
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"
)

type flakyListener struct {
	net.Listener
	failures atomic.Int32
}

func (l *flakyListener) Accept() (net.Conn, error) {
	if l.failures.Add(-1) >= 0 {
		return nil, errors.New("accept: too many open files")
	}
	return l.Listener.Accept()
}

func TestTCPListenerRetriesTemporaryAcceptErrors(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	bound, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	flaky := &flakyListener{Listener: bound}
	flaky.failures.Store(3)
	clientSide, serverSide := net.Pipe()
	remote, err := NewTCPListener(ctx, clientSide, bound.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = remote.Close() }()
	served := make(chan error, 1)
	go func() { served <- ServeTCPListener(ctx, serverSide, flaky, func(c net.Conn) net.Conn { return c }) }()
	conn, err := net.Dial("tcp", bound.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := conn.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	accepted, err := remote.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = accepted.Close() }()
	_ = accepted.SetDeadline(time.Now().Add(5 * time.Second))
	var b [4]byte
	if _, err := io.ReadFull(accepted, b[:]); err != nil || string(b[:]) != "ping" {
		t.Fatalf("bridged %q %v", b, err)
	}
	select {
	case err := <-served:
		t.Fatal("listener stopped after temporary accept errors", err)
	default:
	}
	cancel()
	select {
	case <-served:
	case <-time.After(5 * time.Second):
		t.Fatal("listener did not stop on cancellation")
	}
}
