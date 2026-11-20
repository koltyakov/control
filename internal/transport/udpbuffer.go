package transport

import (
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/transport/v5"
	"github.com/pion/transport/v5/stdnet"
)

// udpSocketBuffer is requested for every WebRTC UDP socket. Operating-system
// defaults such as Linux's 208 KiB overflowed on one-vCPU receivers during
// bursts of a 141 ms transfer, and each drop collapsed the SCTP window. The
// kernel allocates queued datagrams only, so idle sockets do not consume it.
const udpSocketBuffer = 4 << 20

// interfaceRefresh bounds how long one interface snapshot serves ICE agents.
// Pion's default creates a fresh snapshot per agent; one gathering pass reads
// interfaces several times, and later connections must see network changes.
const interfaceRefresh = time.Second

// udpBufferStats records the smallest effective receive buffer obtained.
type udpBufferStats struct {
	minimum atomic.Int64
	warned  sync.Once
}

func (s *udpBufferStats) record(effective int) {
	if s == nil || effective <= 0 {
		return
	}
	for {
		current := s.minimum.Load()
		if current != 0 && current <= int64(effective) {
			break
		}
		if s.minimum.CompareAndSwap(current, int64(effective)) {
			break
		}
	}
	if effective < udpSocketBuffer/2 {
		s.warned.Do(func() {
			slog.Info("operating system limits WebRTC UDP buffers; high-latency transfers may drop packets",
				"effectiveBytes", effective, "requestedBytes", udpSocketBuffer, "hint", udpBufferHint)
		})
	}
}

func (s *udpBufferStats) value() int64 {
	if s == nil {
		return 0
	}
	return s.minimum.Load()
}

// bufferedNet is pion's standard network with larger UDP socket buffers.
// Socket creation and resolution stay in the embedded standard implementation.
type bufferedNet struct {
	*stdnet.Net
	stats *udpBufferStats

	mu         sync.Mutex
	interfaces *stdnet.Net
	sampled    time.Time
}

func newBufferedNet(stats *udpBufferStats) (*bufferedNet, error) {
	base, err := stdnet.NewNet()
	if err != nil {
		return nil, err
	}
	return &bufferedNet{Net: base, stats: stats, interfaces: base, sampled: time.Now()}, nil
}

// current returns an immutable interface snapshot no older than
// interfaceRefresh. A failed refresh keeps the previous snapshot.
func (n *bufferedNet) current() *stdnet.Net {
	n.mu.Lock()
	defer n.mu.Unlock()
	if time.Since(n.sampled) >= interfaceRefresh {
		if fresh, err := stdnet.NewNet(); err == nil {
			n.interfaces = fresh
		}
		n.sampled = time.Now()
	}
	return n.interfaces
}

func (n *bufferedNet) Interfaces() ([]*transport.Interface, error) {
	return n.current().Interfaces()
}

func (n *bufferedNet) InterfaceByIndex(index int) (*transport.Interface, error) {
	return n.current().InterfaceByIndex(index)
}

func (n *bufferedNet) InterfaceByName(name string) (*transport.Interface, error) {
	return n.current().InterfaceByName(name)
}

func (n *bufferedNet) ListenUDP(network string, address *net.UDPAddr) (transport.UDPConn, error) {
	conn, err := n.Net.ListenUDP(network, address)
	if err == nil {
		n.tune(conn)
	}
	return conn, err
}

// ListenPacket serves server-reflexive and TURN relay candidates.
func (n *bufferedNet) ListenPacket(network, address string) (net.PacketConn, error) {
	conn, err := n.Net.ListenPacket(network, address)
	if err == nil {
		n.tune(conn)
	}
	return conn, err
}

func (n *bufferedNet) DialUDP(network string, local, remote *net.UDPAddr) (transport.UDPConn, error) {
	conn, err := n.Net.DialUDP(network, local, remote)
	if err == nil {
		n.tune(conn)
	}
	return conn, err
}

// tune is best effort: a socket keeps working with the operating-system size.
func (n *bufferedNet) tune(conn any) {
	if udp, ok := conn.(*net.UDPConn); ok {
		n.stats.record(setUDPBuffers(udp, udpSocketBuffer))
	}
}
