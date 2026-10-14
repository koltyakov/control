package transport

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"testing"
	"time"

	"github.com/koltyakov/control/internal/protocol"
	"google.golang.org/protobuf/proto"
)

func TestPacketConnBoundsAndChangingWriteDeadline(t *testing.T) {
	started := make(chan struct{})
	c := newConn(context.Background(), func(ctx context.Context, _ []byte) error { close(started); <-ctx.Done(); return ctx.Err() }, nil)
	defer func() { _ = c.Close() }()
	if c.push(make([]byte, largePacketSize+1)) {
		t.Fatal("oversized packet accepted")
	}
	result := make(chan error, 1)
	go func() { _, err := c.Write([]byte("blocked")); result <- err }()
	<-started
	if err := c.SetWriteDeadline(time.Now().Add(20 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-result:
		if !errors.Is(err, os.ErrDeadlineExceeded) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("deadline change did not interrupt pending write")
	}
	for range cap(c.in) {
		if !c.push([]byte("x")) {
			t.Fatal("queue filled early")
		}
	}
	if c.push([]byte("overflow")) {
		t.Fatal("unbounded packet queue")
	}
	_ = c.SetReadDeadline(time.Now())
	if _, err := c.Read(make([]byte, 1)); !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatal("expired read consumed buffered bytes", err)
	}
}

func TestPacketConnDeadlineCanBeExtended(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	c := newConn(context.Background(), func(ctx context.Context, _ []byte) error {
		close(started)
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}, nil)
	defer func() { _ = c.Close() }()
	_ = c.SetWriteDeadline(time.Now().Add(100 * time.Millisecond))
	result := make(chan error, 1)
	go func() { _, err := c.Write([]byte("write")); result <- err }()
	<-started
	_ = c.SetWriteDeadline(time.Now().Add(time.Second))
	timer := time.NewTimer(150 * time.Millisecond)
	<-timer.C
	close(release)
	if err := <-result; err != nil {
		t.Fatal("extended deadline still expired", err)
	}
}

func TestPacketBuffersAreCopiedAndReleasedOnClose(t *testing.T) {
	c := newConn(context.Background(), nil, nil)
	data := []byte("packet")
	if !c.push(data) {
		t.Fatal("packet rejected")
	}
	copy(data, []byte("mutate"))
	var first [2]byte
	if _, err := io.ReadFull(c, first[:]); err != nil || string(first[:]) != "pa" {
		t.Fatal(first, err)
	}
	if !c.push([]byte("queued")) {
		t.Fatal("packet rejected")
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if c.current != nil || len(c.in) != 0 || c.buffer != nil {
		t.Fatal("closed connection retained pooled buffers")
	}
	if c.push([]byte("closed")) {
		t.Fatal("closed connection accepted packet")
	}
}

func BenchmarkPacketConnTransfer(b *testing.B) {
	for _, size := range []int{1024, 16 * 1024} {
		b.Run(map[int]string{1024: "1KiB", 16 * 1024: "16KiB"}[size], func(b *testing.B) {
			var receiver *packetConn
			sender := newConn(context.Background(), func(_ context.Context, data []byte) error {
				if !receiver.push(data) {
					return io.ErrShortWrite
				}
				return nil
			}, nil)
			receiver = newConn(context.Background(), nil, nil)
			defer func() { _ = sender.Close(); _ = receiver.Close() }()
			data := bytes.Repeat([]byte("x"), size)
			out := make([]byte, size)
			b.SetBytes(int64(size))
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				if _, err := sender.Write(data); err != nil {
					b.Fatal(err)
				}
				if _, err := io.ReadFull(receiver, out); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkRelayDataPacket(b *testing.B) {
	for _, owned := range []bool{false, true} {
		name := "copied"
		if owned {
			name = "owned"
		}
		b.Run(name, func(b *testing.B) { benchmarkRelayDataPacket(b, owned) })
	}
}

func benchmarkRelayDataPacket(b *testing.B, owned bool) {
	// One relay hop as the destination peer sees it: decode the gateway
	// packet, queue its payload, and read it back as a stream.
	payload := bytes.Repeat([]byte("x"), 16*1024)
	wire, err := proto.Marshal(&protocol.Packet{Kind: "data", To: "peer", Session: "session", Data: payload})
	if err != nil {
		b.Fatal(err)
	}
	receiver := newConn(context.Background(), nil, nil)
	defer func() { _ = receiver.Close() }()
	out := make([]byte, len(payload))
	b.SetBytes(int64(len(payload)))
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		var packet protocol.Packet
		if err := proto.Unmarshal(wire, &packet); err != nil {
			b.Fatal(err)
		}
		push := receiver.push
		if owned {
			push = receiver.pushOwned
		}
		if !push(packet.Data) {
			b.Fatal("packet rejected")
		}
		if _, err := io.ReadFull(receiver, out); err != nil {
			b.Fatal(err)
		}
	}
}

func TestPushOwnedTransfersWithoutCopy(t *testing.T) {
	c := newConn(context.Background(), nil, nil)
	defer func() { _ = c.Close() }()
	data := []byte("packet")
	if !c.pushOwned(data) {
		t.Fatal("packet rejected")
	}
	// Ownership transferred: the queued bytes are the caller's slice, so
	// later mutation is visible — push, by contrast, must copy.
	data[0] = 'P'
	out := make([]byte, len(data))
	if _, err := io.ReadFull(c, out); err != nil || string(out) != "Packet" {
		t.Fatalf("owned packet = %q, %v", out, err)
	}
	// A consumed owned buffer must not reappear from the packet pools.
	if !c.push([]byte("pooled")) {
		t.Fatal("pooled packet rejected")
	}
	out = make([]byte, 6)
	if _, err := io.ReadFull(c, out); err != nil || string(out) != "pooled" {
		t.Fatalf("pooled packet = %q, %v", out, err)
	}
}

func BenchmarkRelayMarshal(b *testing.B) {
	for _, pooled := range []bool{false, true} {
		name := "allocated"
		if pooled {
			name = "pooled"
		}
		b.Run(name, func(b *testing.B) { benchmarkRelayMarshal(b, pooled) })
	}
}

func benchmarkRelayMarshal(b *testing.B, reuse bool) {
	// The peer send path marshals each relay chunk into a recycled buffer.
	packet := &protocol.Packet{Kind: "data", To: "peer", Session: "session", Data: bytes.Repeat([]byte("x"), 16*1024)}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if !reuse {
			if _, err := proto.Marshal(packet); err != nil {
				b.Fatal(err)
			}
			continue
		}
		pooled := getWriteBuffer()
		wire, err := proto.MarshalOptions{}.MarshalAppend((*pooled)[:0], packet)
		if err != nil {
			b.Fatal(err)
		}
		*pooled = wire
		putWriteBuffer(pooled)
	}
}
