package transport

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"testing"
	"time"
)

func TestDuplexEOFKeepsResponseDirectionOpen(t *testing.T) {
	a, b := net.Pipe()
	client, server := NewDuplexConn(a), NewDuplexConn(b)
	defer func() { _ = client.Close(); _ = server.Close() }()
	_ = client.SetDeadline(time.Now().Add(time.Second))
	_ = server.SetDeadline(time.Now().Add(time.Second))
	result := make(chan error, 1)
	go func() {
		data, err := io.ReadAll(server)
		if err == nil && string(data) != "request" {
			err = errors.New("request changed")
		}
		if err == nil {
			_, err = server.Write([]byte("response"))
		}
		if err == nil {
			err = server.CloseWrite()
		}
		result <- err
	}()
	if _, err := client.Write([]byte("request")); err != nil {
		t.Fatal(err)
	}
	if err := client.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(client)
	if err != nil || string(data) != "response" {
		t.Fatal(string(data), err)
	}
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	if _, err := client.Write([]byte("after EOF")); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatal(err)
	}
}

func TestDuplexRejectsOversizedAndTruncatedRecords(t *testing.T) {
	for _, truncated := range []bool{false, true} {
		a, b := net.Pipe()
		c := NewDuplexConn(a)
		go func() {
			defer func() { _ = b.Close() }()
			var header [4]byte
			if truncated {
				binary.BigEndian.PutUint32(header[:], 2)
			} else {
				binary.BigEndian.PutUint32(header[:], duplexFrameSize+1)
			}
			_, _ = b.Write(header[:])
			if truncated {
				_, _ = b.Write([]byte("x"))
			}
		}()
		_, err := io.ReadAll(c)
		_ = c.Close()
		if err == nil {
			t.Fatal("malformed record accepted")
		}
	}
}

func BenchmarkDuplexTransfer(b *testing.B) {
	a, other := net.Pipe()
	writer, reader := NewDuplexConn(a), NewDuplexConn(other)
	defer func() { _ = writer.Close(); _ = reader.Close() }()
	done := make(chan error, 1)
	go func() { _, err := io.Copy(io.Discard, reader); done <- err }()
	data := bytes.Repeat([]byte("x"), 32*1024)
	b.SetBytes(int64(len(data)))
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if _, err := writer.Write(data); err != nil {
			b.Fatal(err)
		}
	}
	b.StopTimer()
	if err := writer.CloseWrite(); err != nil {
		b.Fatal(err)
	}
	if err := <-done; err != nil {
		b.Fatal(err)
	}
}
