package transport

import (
	"encoding/binary"
	"net"
	"sync"
)

// yamux writes each frame header and body separately, and TLS seals each write
// in at least one record. frameConn sits between them and holds the packet
// adapter's output until a whole multiplexer frame has been sealed, so a data
// frame crosses the carrier in full packets rather than one message per record.
type frameConn struct {
	net.Conn
	packets *packetConn
	mu      sync.Mutex
	holding bool
}

func newFrameConn(secure net.Conn, packets *packetConn) *frameConn {
	return &frameConn{Conn: secure, packets: packets}
}

func (c *frameConn) Write(b []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.holding {
		c.packets.holdWrites()
		c.holding = true
	}
	n, err := c.Conn.Write(b)
	// yamux's single send loop always writes a data header's body next.
	if err == nil && yamuxDataHeader(b) {
		return n, nil
	}
	c.holding = false
	if releaseErr := c.packets.releaseWrites(); err == nil {
		err = releaseErr
	}
	return n, err
}

// yamuxDataHeader matches a version-0 data frame header announcing a body.
func yamuxDataHeader(b []byte) bool {
	return len(b) == 12 && b[0] == 0 && b[1] == 0 && binary.BigEndian.Uint32(b[8:]) != 0
}

// joinConn joins a nested multiplexer's data header to its body, so each nested
// frame is one write, and therefore one frame, on the outer stream.
type joinConn struct {
	net.Conn
	mu     sync.Mutex
	header []byte
	buf    []byte
}

func newJoinConn(conn net.Conn) *joinConn { return &joinConn{Conn: conn} }

func (c *joinConn) Write(b []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.header) == 0 {
		if yamuxDataHeader(b) {
			c.header = append(c.header[:0], b...)
			return len(b), nil
		}
		return c.Conn.Write(b)
	}
	c.buf = append(append(c.buf[:0], c.header...), b...)
	held := len(c.header)
	c.header = c.header[:0]
	n, err := c.Conn.Write(c.buf)
	if cap(c.buf) > 256*1024 {
		c.buf = nil
	}
	return max(0, n-held), err
}
