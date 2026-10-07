package transport

import (
	"bytes"
	"context"
	"crypto/tls"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hashicorp/yamux"
	"github.com/koltyakov/control/internal/identity"
	"github.com/koltyakov/control/internal/wire"
)

type packetCounter struct {
	packets, tiny, largest atomic.Int64
}

// countedMux connects two multiplexers through TLS and packet adapters that
// count carrier messages, as WebRTC data channels or relay packets would.
func countedMux(t *testing.T, packetSize int) (*yamux.Session, *yamux.Session, *packetCounter) {
	t.Helper()
	counter := &packetCounter{}
	var a, b *packetConn
	forward := func(dst **packetConn) func(context.Context, []byte) error {
		return func(ctx context.Context, p []byte) error {
			counter.packets.Add(1)
			if len(p) < 128 {
				counter.tiny.Add(1)
			}
			for {
				largest := counter.largest.Load()
				if int64(len(p)) <= largest || counter.largest.CompareAndSwap(largest, int64(len(p))) {
					break
				}
			}
			for !(*dst).push(p) {
				if err := ctx.Err(); err != nil {
					return err
				}
				if (*dst).ctx.Err() != nil {
					return io.ErrClosedPipe
				}
				time.Sleep(20 * time.Microsecond)
			}
			return nil
		}
	}
	a = newConn(context.Background(), forward(&b), nil)
	b = newConn(context.Background(), forward(&a), nil)
	a.packetSize, b.packetSize = packetSize, packetSize
	clientID, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	serverID, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	server := tls.Server(b, serverID.TLS(clientID.ID))
	var wg sync.WaitGroup
	wg.Go(func() { _ = server.Handshake() })
	client := tls.Client(a, clientID.TLS(serverID.ID))
	if err := client.Handshake(); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	clientMux, err := yamux.Client(newFrameConn(client, a), muxConfig(BulkLane))
	if err != nil {
		t.Fatal(err)
	}
	serverMux, err := yamux.Server(newFrameConn(server, b), muxConfig(IncomingLane))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = clientMux.Close(); _ = serverMux.Close(); _ = a.Close(); _ = b.Close() })
	return clientMux, serverMux, counter
}

func TestBulkFramesUseFullPackets(t *testing.T) {
	const size = 8 << 20
	const frames = size / (32 << 10)
	for _, tc := range []struct {
		name       string
		packetSize int
		maxPackets int64
		maxTiny    int64
	}{
		// Splitting 16,406-byte TLS records previously produced 1,299 messages,
		// 782 of them under 128 bytes, for this transfer. At 16 KiB a 32 KiB
		// frame still leaves one short tail packet.
		{"legacy", legacyPacketSize, size/legacyPacketSize + frames + 16, frames + 16},
		{"large", largePacketSize, frames + 16, 16},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clientMux, serverMux, counter := countedMux(t, tc.packetSize)
			received := make(chan int64, 1)
			go func() {
				stream, err := clientMux.AcceptStream()
				if err != nil {
					received <- -1
					return
				}
				n, _ := io.Copy(io.Discard, stream)
				_ = stream.Close()
				received <- n
			}()
			stream, err := serverMux.OpenStream()
			if err != nil {
				t.Fatal(err)
			}
			time.Sleep(20 * time.Millisecond)
			counter.packets.Store(0)
			counter.tiny.Store(0)
			if _, err := io.CopyN(struct{ io.Writer }{stream}, bytes.NewReader(make([]byte, size)), size); err != nil {
				t.Fatal(err)
			}
			_ = stream.Close()
			if n := <-received; n != size {
				t.Fatalf("received %d bytes", n)
			}
			packets, tiny := counter.packets.Load(), counter.tiny.Load()
			if packets > tc.maxPackets || tiny > tc.maxTiny {
				t.Fatalf("%d packets (%d under 128 bytes) for %d bytes", packets, tiny, size)
			}
			if counter.largest.Load() > int64(tc.packetSize) {
				t.Fatalf("packet of %d bytes exceeds negotiated %d", counter.largest.Load(), tc.packetSize)
			}
		})
	}
}

func TestSmallRecordsAndFramesUseOnePacket(t *testing.T) {
	clientMux, serverMux, counter := countedMux(t, legacyPacketSize)
	go func() {
		stream, err := clientMux.AcceptStream()
		if err == nil {
			_, _ = io.Copy(io.Discard, stream)
		}
	}()
	stream, err := serverMux.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	settle := func() {
		deadline := time.Now().Add(time.Second)
		for last := counter.packets.Load(); time.Now().Before(deadline); {
			time.Sleep(20 * time.Millisecond)
			if current := counter.packets.Load(); current == last {
				return
			} else {
				last = current
			}
		}
	}
	settle()
	for name, write := range map[string]func() error{
		"duplex record": func() error { _, err := NewDuplexConn(stream).Write([]byte("x")); return err },
		"control frame": func() error { return wire.WriteFrame(stream, map[string]string{"method": "tasks.get"}) },
	} {
		counter.packets.Store(0)
		if err := write(); err != nil {
			t.Fatal(name, err)
		}
		settle()
		// The receiver may separately return a window update.
		if packets := counter.packets.Load(); packets != 1 && packets != 2 {
			t.Fatalf("%s used %d packets", name, packets)
		}
	}
}

func TestJoinConnWritesNestedFramesOnce(t *testing.T) {
	var writes [][]byte
	conn := newJoinConn(&recordingConn{writes: &writes})
	header := []byte{0, 0, 0, 0, 0, 0, 0, 1, 0, 0, 0, 3}
	if n, err := conn.Write(header); err != nil || n != len(header) {
		t.Fatal(n, err)
	}
	if n, err := conn.Write([]byte("abc")); err != nil || n != 3 {
		t.Fatal(n, err)
	}
	ping := []byte{0, 2, 0, 1, 0, 0, 0, 0, 0, 0, 0, 7}
	if _, err := conn.Write(ping); err != nil {
		t.Fatal(err)
	}
	if len(writes) != 2 || !bytes.Equal(writes[0], append(append([]byte{}, header...), "abc"...)) || !bytes.Equal(writes[1], ping) {
		t.Fatalf("unexpected writes %q", writes)
	}
}

type recordingConn struct {
	net.Conn
	writes *[][]byte
}

func (c *recordingConn) Write(b []byte) (int, error) {
	*c.writes = append(*c.writes, append([]byte(nil), b...))
	return len(b), nil
}
