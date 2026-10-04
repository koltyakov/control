package transport

import (
	"context"
	"io"
	"net"
	"os"
	"sync"
	"time"
)

// packetConn turns ordered, reliable messages into the byte stream TLS expects.
// The session multiplexer above TLS supplies stream-level flow control.
type packetConn struct {
	ctx           context.Context
	cancel        context.CancelFunc
	in            chan []byte
	send          func(context.Context, []byte) error
	onClose       func()
	once          sync.Once
	readMu        sync.Mutex
	writeMu       sync.Mutex
	buffer        []byte
	mu            sync.Mutex
	readDeadline  time.Time
	writeDeadline time.Time
	changed       chan struct{}
}

func newConn(parent context.Context, send func(context.Context, []byte) error, closeFn func()) *packetConn {
	ctx, cancel := context.WithCancel(parent)
	return &packetConn{ctx: ctx, cancel: cancel, in: make(chan []byte, 256), send: send, onClose: closeFn, changed: make(chan struct{})}
}

func (c *packetConn) push(b []byte) bool {
	select {
	case <-c.ctx.Done():
		return false
	default:
	}
	select {
	case c.in <- append([]byte(nil), b...):
		return true
	default:
		return false
	}
}

func (c *packetConn) Read(b []byte) (int, error) {
	c.readMu.Lock()
	defer c.readMu.Unlock()
	if len(b) == 0 {
		return 0, nil
	}
	for len(c.buffer) == 0 {
		c.mu.Lock()
		deadline, changed := c.readDeadline, c.changed
		c.mu.Unlock()
		var timer *time.Timer
		var timeout <-chan time.Time
		if !deadline.IsZero() {
			timer = time.NewTimer(time.Until(deadline))
			timeout = timer.C
		}
		select {
		case c.buffer = <-c.in:
		case <-c.ctx.Done():
			if timer != nil {
				timer.Stop()
			}
			return 0, io.EOF
		case <-timeout:
			return 0, os.ErrDeadlineExceeded
		case <-changed:
		}
		if timer != nil {
			timer.Stop()
		}
	}
	n := copy(b, c.buffer)
	c.buffer = c.buffer[n:]
	return n, nil
}

func (c *packetConn) Write(b []byte) (int, error) {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	c.mu.Lock()
	deadline := c.writeDeadline
	c.mu.Unlock()
	ctx := c.ctx
	var cancel context.CancelFunc
	if !deadline.IsZero() {
		ctx, cancel = context.WithDeadline(ctx, deadline)
		defer cancel()
	}
	written := 0
	for len(b) > 0 {
		if err := ctx.Err(); err != nil {
			return written, err
		}
		n := min(len(b), 16*1024)
		if err := c.send(ctx, b[:n]); err != nil {
			return written, err
		}
		written += n
		b = b[n:]
	}
	return written, nil
}

func (c *packetConn) Close() error {
	c.once.Do(func() {
		c.cancel()
		if c.onClose != nil {
			c.onClose()
		}
	})
	return nil
}

type address string

func (a address) Network() string          { return "control" }
func (a address) String() string           { return string(a) }
func (c *packetConn) LocalAddr() net.Addr  { return address("local") }
func (c *packetConn) RemoteAddr() net.Addr { return address("peer") }
func (c *packetConn) SetDeadline(t time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.readDeadline, c.writeDeadline = t, t
	close(c.changed)
	c.changed = make(chan struct{})
	return nil
}
func (c *packetConn) SetReadDeadline(t time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.readDeadline = t
	close(c.changed)
	c.changed = make(chan struct{})
	return nil
}
func (c *packetConn) SetWriteDeadline(t time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.writeDeadline = t
	return nil
}
