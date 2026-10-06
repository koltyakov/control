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
	in            chan *packetBuffer
	send          func(context.Context, []byte) error
	onClose       func()
	once          sync.Once
	readMu        sync.Mutex
	writeMu       sync.Mutex
	buffer        []byte
	current       *packetBuffer
	mu            sync.Mutex
	readDeadline  time.Time
	writeDeadline time.Time
	changed       chan struct{}
	writeCancel   context.CancelFunc
	writeTimer    *time.Timer
}

type packetBuffer struct {
	data   []byte
	bucket int
}

var packetBuffers = [3]sync.Pool{
	{New: func() any { return &packetBuffer{data: make([]byte, 1024), bucket: 0} }},
	{New: func() any { return &packetBuffer{data: make([]byte, 4096), bucket: 1} }},
	{New: func() any { return &packetBuffer{data: make([]byte, 16*1024), bucket: 2} }},
}

func releasePacket(b *packetBuffer) {
	if b != nil {
		packetBuffers[b.bucket].Put(b)
	}
}

func newConn(parent context.Context, send func(context.Context, []byte) error, closeFn func()) *packetConn {
	ctx, cancel := context.WithCancel(parent)
	return &packetConn{ctx: ctx, cancel: cancel, in: make(chan *packetBuffer, 256), send: send, onClose: closeFn, changed: make(chan struct{})}
}

func (c *packetConn) push(b []byte) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(b) == 0 || len(b) > 16*1024 || len(c.in) == cap(c.in) {
		return false
	}
	select {
	case <-c.ctx.Done():
		return false
	default:
	}
	bucket := 0
	if len(b) > 1024 {
		bucket = 1
	}
	if len(b) > 4096 {
		bucket = 2
	}
	packet := packetBuffers[bucket].Get().(*packetBuffer)
	packet.data = packet.data[:len(b)]
	copy(packet.data, b)
	select {
	case c.in <- packet:
		return true
	default:
		releasePacket(packet)
		return false
	}
}

func (c *packetConn) Read(b []byte) (int, error) {
	c.readMu.Lock()
	defer c.readMu.Unlock()
	if len(b) == 0 {
		return 0, nil
	}
	c.mu.Lock()
	expired := !c.readDeadline.IsZero() && !time.Now().Before(c.readDeadline)
	c.mu.Unlock()
	if expired {
		return 0, os.ErrDeadlineExceeded
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
		case c.current = <-c.in:
			c.buffer = c.current.data
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
	if len(c.buffer) == 0 {
		releasePacket(c.current)
		c.current = nil
	}
	return n, nil
}

func (c *packetConn) Write(b []byte) (int, error) {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	c.mu.Lock()
	if !c.writeDeadline.IsZero() && !time.Now().Before(c.writeDeadline) {
		c.mu.Unlock()
		return 0, os.ErrDeadlineExceeded
	}
	ctx, cancel := context.WithCancel(c.ctx)
	c.writeCancel = cancel
	c.armWriteTimerLocked()
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		c.writeCancel = nil
		if c.writeTimer != nil {
			c.writeTimer.Stop()
			c.writeTimer = nil
		}
		c.mu.Unlock()
		cancel()
	}()
	written := 0
	for len(b) > 0 {
		if err := ctx.Err(); err != nil {
			return written, c.writeError(err)
		}
		n := min(len(b), 16*1024)
		if err := c.send(ctx, b[:n]); err != nil {
			return written, c.writeError(err)
		}
		written += n
		b = b[n:]
	}
	return written, nil
}

func (c *packetConn) Close() error {
	c.once.Do(func() {
		c.cancel()
		c.readMu.Lock()
		c.mu.Lock()
		releasePacket(c.current)
		c.current = nil
		c.buffer = nil
		for len(c.in) != 0 {
			releasePacket(<-c.in)
		}
		c.mu.Unlock()
		c.readMu.Unlock()
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
	c.armWriteTimerLocked()
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
	c.armWriteTimerLocked()
	return nil
}

func (c *packetConn) armWriteTimerLocked() {
	if c.writeTimer != nil {
		c.writeTimer.Stop()
		c.writeTimer = nil
	}
	if c.writeCancel == nil || c.writeDeadline.IsZero() {
		return
	}
	if !time.Now().Before(c.writeDeadline) {
		c.writeCancel()
		return
	}
	c.writeTimer = time.AfterFunc(time.Until(c.writeDeadline), func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		if c.writeCancel != nil && !c.writeDeadline.IsZero() && !time.Now().Before(c.writeDeadline) {
			c.writeCancel()
		}
	})
}

func (c *packetConn) writeError(err error) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ctx.Err() == nil && !c.writeDeadline.IsZero() && !time.Now().Before(c.writeDeadline) {
		return os.ErrDeadlineExceeded
	}
	return err
}
