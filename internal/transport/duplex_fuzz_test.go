package transport

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"math"
	"net"
	"testing"

	"github.com/koltyakov/control/internal/model"
)

// bufferConn is a net.Conn over in-memory bytes. Only Read and Write are used.
type bufferConn struct {
	net.Conn
	in  *bytes.Reader
	out bytes.Buffer
}

func (c *bufferConn) Read(b []byte) (int, error)  { return c.in.Read(b) }
func (c *bufferConn) Write(b []byte) (int, error) { return c.out.Write(b) }

// duplexReference decodes records independently: a zero length is EOF, a
// length above 32 KiB is a protocol error, and a short record or missing
// header is an unexpected EOF.
func duplexReference(stream []byte) ([]byte, error) {
	var data []byte
	for {
		if len(stream) < 4 {
			return data, io.ErrUnexpectedEOF
		}
		n := binary.BigEndian.Uint32(stream)
		stream = stream[4:]
		if n == 0 {
			return data, io.EOF
		}
		if n > duplexFrameSize {
			return data, errors.New("oversized")
		}
		if uint64(n) > uint64(len(stream)) {
			return append(data, stream...), io.ErrUnexpectedEOF
		}
		data = append(data, stream[:n]...)
		stream = stream[n:]
	}
}

// FuzzDuplexConn reads arbitrary peer records through DuplexConn with
// fuzzer-chosen read sizes and compares the delivered bytes and terminal
// error with an independent decoder. It also checks that written payloads and
// a write-side EOF round-trip through the record encoding.
func FuzzDuplexConn(f *testing.F) {
	record := func(n uint32, body string) []byte { return append(binary.BigEndian.AppendUint32(nil, n), body...) }
	for _, seed := range []struct {
		stream  []byte
		payload []byte
		sizes   []byte
	}{
		{append(record(5, "hello"), record(0, "")...), []byte("hello"), []byte{1}},
		{append(record(3, "abc"), record(2, "de")...), nil, []byte{2, 7}},
		{record(duplexFrameSize+1, ""), bytes.Repeat([]byte("x"), duplexFrameSize+3), []byte{255}},
		{record(10, "abc"), []byte{}, []byte{0, 1}},
		{[]byte{0, 0}, []byte("tail"), nil},
		{append(record(0, ""), record(1, "x")...), nil, []byte{9}},
	} {
		f.Add(seed.stream, seed.payload, seed.sizes)
	}
	f.Fuzz(func(t *testing.T, stream, payload, sizes []byte) {
		conn := NewDuplexConn(&bufferConn{in: bytes.NewReader(stream)})
		// A zero-length read consumes nothing, even before the first header.
		if n, err := conn.Read(nil); n != 0 || err != nil {
			t.Fatalf("zero-length read returned %d, %v", n, err)
		}
		want, wantErr := duplexReference(stream)
		var got []byte
		var err error
		for i := 0; err == nil; i++ {
			if i > len(stream)+1 {
				t.Fatal("reads stopped making progress")
			}
			size := 1
			if len(sizes) > 0 {
				size += int(sizes[i%len(sizes)])
			}
			buffer := make([]byte, size)
			var n int
			n, err = conn.Read(buffer)
			if n < 0 || n > size {
				t.Fatalf("read returned %d for a %d-byte buffer", n, size)
			}
			got = append(got, buffer[:n]...)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("delivered %q, want %q", got, want)
		}
		switch {
		case errors.Is(wantErr, io.EOF):
			if !errors.Is(err, io.EOF) {
				t.Fatalf("got %v at end of stream, want EOF", err)
			}
			if n, again := conn.Read(make([]byte, 1)); n != 0 || !errors.Is(again, io.EOF) {
				t.Fatal("EOF is not sticky", n, again)
			}
		case errors.Is(wantErr, io.ErrUnexpectedEOF):
			if !errors.Is(err, io.ErrUnexpectedEOF) {
				t.Fatalf("got %v for a truncated stream", err)
			}
		default:
			if err == nil || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				t.Fatalf("got %v for an oversized record", err)
			}
		}

		out := &bufferConn{in: bytes.NewReader(nil)}
		writer := NewDuplexConn(out)
		if n, err := writer.Write(payload); n != len(payload) || err != nil {
			t.Fatalf("wrote %d of %d bytes: %v", n, len(payload), err)
		}
		if err := writer.CloseWrite(); err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Write([]byte("x")); !errors.Is(err, io.ErrClosedPipe) {
			t.Fatalf("write after EOF: %v", err)
		}
		reader := NewDuplexConn(&bufferConn{in: bytes.NewReader(out.out.Bytes())})
		echoed, err := io.ReadAll(reader)
		if err != nil || !bytes.Equal(echoed, payload) {
			t.Fatalf("round trip returned %d of %d bytes: %v", len(echoed), len(payload), err)
		}
		if decoded, end := duplexReference(out.out.Bytes()); !errors.Is(end, io.EOF) || !bytes.Equal(decoded, payload) {
			t.Fatalf("written records are not canonical: %v", end)
		}
	})
}

// FuzzValidateLogChunk checks that an accepted streamed log chunk is bounded
// and advances the caller's offset by exactly its data length, so a peer can
// neither rewind nor skip log bytes, including near the int64 limit.
func FuzzValidateLogChunk(f *testing.F) {
	for _, seed := range []struct {
		data          []byte
		offset, start int64
		message       string
	}{
		{[]byte("line\n"), 5, 0, ""},
		{nil, 0, 0, ""},
		{[]byte("x"), 0, 0, ""},
		{[]byte("x"), 11, 10, "boom"},
		{bytes.Repeat([]byte("x"), MaxLogChunk+1), MaxLogChunk + 1, 0, ""},
		{[]byte("xy"), math.MinInt64, math.MaxInt64 - 1, ""},
		{[]byte("xy"), math.MinInt64 + 1, math.MaxInt64, ""},
	} {
		f.Add(seed.data, seed.offset, seed.start, seed.message)
	}
	f.Fuzz(func(t *testing.T, data []byte, chunkOffset, offset int64, message string) {
		if offset < 0 {
			// FollowTaskLogs rejects negative starting offsets before streaming.
			return
		}
		chunk := model.TaskLogChunk{Data: data, Offset: chunkOffset, Error: message}
		err := ValidateLogChunk(chunk, offset)
		if message != "" {
			if err == nil || err.Error() != message {
				t.Fatalf("peer error %q reported as %v", message, err)
			}
			return
		}
		if err != nil {
			return
		}
		if len(data) > MaxLogChunk {
			t.Fatalf("accepted a %d-byte chunk", len(data))
		}
		if chunkOffset < offset || chunkOffset-offset != int64(len(data)) {
			t.Fatalf("accepted offset %d after %d for %d bytes", chunkOffset, offset, len(data))
		}
	})
}
