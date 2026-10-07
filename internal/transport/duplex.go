package transport

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"sync"
	"time"
)

const DuplexVersion = 1
const duplexFrameSize = 32 * 1024

var ErrHalfCloseUnsupported = errors.New("connection does not support write-side EOF")

var bridgeBuffers = sync.Pool{New: func() any { b := make([]byte, 32*1024); return &b }}
var recordBuffers = sync.Pool{New: func() any { b := make([]byte, 4+duplexFrameSize); return &b }}

// DuplexConn adds bounded data records and a write-side EOF record to a byte
// stream. This keeps TCP half-close independent of yamux and WebSocket Close.
type DuplexConn struct {
	net.Conn
	readMu, writeMu   sync.Mutex
	remaining         uint32
	readEOF, writeEOF bool
	readError         error
}

func NewDuplexConn(conn net.Conn) *DuplexConn { return &DuplexConn{Conn: conn} }

func (c *DuplexConn) Read(b []byte) (int, error) {
	c.readMu.Lock()
	defer c.readMu.Unlock()
	if len(b) == 0 {
		return 0, nil
	}
	if c.readEOF {
		return 0, io.EOF
	}
	if c.readError != nil {
		return 0, c.readError
	}
	if c.remaining == 0 {
		var header [4]byte
		if _, err := io.ReadFull(c.Conn, header[:]); err != nil {
			if err == io.EOF {
				err = io.ErrUnexpectedEOF
			}
			return 0, err
		}
		c.remaining = binary.BigEndian.Uint32(header[:])
		if c.remaining == 0 {
			c.readEOF = true
			return 0, io.EOF
		}
		if c.remaining > duplexFrameSize {
			c.readError = errors.New("duplex data record exceeds 32 KiB")
			return 0, c.readError
		}
	}
	n, err := c.Conn.Read(b[:min(len(b), int(c.remaining))])
	c.remaining -= uint32(n)
	if err == io.EOF {
		err = io.ErrUnexpectedEOF
	}
	return n, err
}

func (c *DuplexConn) Write(b []byte) (int, error) {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if c.writeEOF {
		return 0, io.ErrClosedPipe
	}
	record := recordBuffers.Get().(*[]byte)
	defer recordBuffers.Put(record)
	written := 0
	for len(b) != 0 {
		n := min(len(b), duplexFrameSize)
		// Send the length and payload together so one record is one stream frame.
		frame := (*record)[:4+n]
		binary.BigEndian.PutUint32(frame, uint32(n))
		copy(frame[4:], b[:n])
		count, err := c.Conn.Write(frame)
		if count > 4 {
			written += count - 4
		}
		if err != nil {
			return written, err
		}
		if count != len(frame) {
			return written, io.ErrShortWrite
		}
		b = b[n:]
	}
	return written, nil
}

func (c *DuplexConn) CloseWrite() error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if c.writeEOF {
		return nil
	}
	c.writeEOF = true
	var header [4]byte
	n, err := c.Conn.Write(header[:])
	if err == nil && n != len(header) {
		err = io.ErrShortWrite
	}
	return err
}

func (c *DuplexConn) Close() error { _ = c.SetDeadline(time.Now()); return c.Conn.Close() }

func CloseWrite(conn net.Conn) error {
	if half, ok := conn.(interface{ CloseWrite() error }); ok {
		return half.CloseWrite()
	}
	return ErrHalfCloseUnsupported
}

// Bridge preserves the opposite direction after graceful EOF when the
// destination supports CloseWrite. Errors, cancellation, and legacy full-close
// streams end both directions. Every copy goroutine is joined before return.
func Bridge(ctx context.Context, a, b net.Conn) {
	done := make(chan error, 2)
	copyTo := func(dst, src net.Conn) {
		buffer := bridgeBuffers.Get().(*[]byte)
		_, err := io.CopyBuffer(dst, src, *buffer)
		bridgeBuffers.Put(buffer)
		if err == nil {
			err = CloseWrite(dst)
		}
		done <- err
	}
	go copyTo(a, b)
	go copyTo(b, a)
	remaining := 2
	for remaining != 0 {
		select {
		case <-ctx.Done():
			abortStream(a)
			abortStream(b)
		case err := <-done:
			remaining--
			if err != nil {
				abortStream(a)
				abortStream(b)
			}
		}
		if ctx.Err() != nil {
			for range remaining {
				<-done
			}
			break
		}
	}
	abortStream(a)
	abortStream(b)
}
