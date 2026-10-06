package transport

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"testing"
	"time"
)

func TestPacketConnBoundsAndChangingWriteDeadline(t *testing.T) {
	started := make(chan struct{})
	c := newConn(context.Background(), func(ctx context.Context, _ []byte) error { close(started); <-ctx.Done(); return ctx.Err() }, nil)
	defer func() { _ = c.Close() }()
	if c.push(make([]byte, 16*1024+1)) {
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
