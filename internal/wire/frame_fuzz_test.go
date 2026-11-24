package wire

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"testing"
)

func prefixed(length uint32, body string) []byte {
	b := binary.BigEndian.AppendUint32(nil, length)
	return append(b, body...)
}

// FuzzReadFrameLimit feeds arbitrary peer bytes to the frame reader. An
// accepted frame must consume exactly its prefix and declared body, never more
// than the limit, and re-encode to an equivalent frame. Oversized prefixes are
// refused before any body byte is read.
func FuzzReadFrameLimit(f *testing.F) {
	for _, seed := range []struct {
		data  []byte
		limit uint32
	}{
		{prefixed(2, `{}`), MaxFrame},
		{prefixed(7, `"a<b&c"payload`), MaxFrame},
		{prefixed(2, `{}`), 1},
		{prefixed(10, `{}`), MaxFrame},
		{prefixed(0, ``), MaxFrame},
		{prefixed(MaxFrame+1, `{}`), ^uint32(0)},
		{prefixed(^uint32(0), ``), ^uint32(0)},
		{prefixed(3, `[1,`), MaxFrame},
		{[]byte{0, 0}, MaxFrame},
		{prefixed(12, `{"a":[null]}`), 12},
	} {
		f.Add(seed.data, seed.limit)
	}
	f.Fuzz(func(t *testing.T, data []byte, limit uint32) {
		reader := bytes.NewReader(data)
		var got any
		err := ReadFrameLimit(reader, &got, limit)
		consumed := len(data) - reader.Len()
		if len(data) < 4 {
			if err == nil || consumed != len(data) {
				t.Fatalf("short prefix accepted or over-read: %v, consumed %d", err, consumed)
			}
			return
		}
		n := binary.BigEndian.Uint32(data)
		if n > limit || n > MaxFrame {
			// The body of an oversized frame must remain unread.
			if err == nil || consumed != 4 {
				t.Fatalf("frame of %d bytes over limit %d accepted or read: %v, consumed %d", n, limit, err, consumed)
			}
			return
		}
		available := uint64(len(data) - 4)
		if uint64(n) > available {
			want := io.ErrUnexpectedEOF
			if available == 0 {
				want = io.EOF
			}
			if !errors.Is(err, want) || consumed != len(data) {
				t.Fatalf("truncated %d-byte frame with %d bytes: %v, consumed %d", n, available, err, consumed)
			}
			return
		}
		if consumed != 4+int(n) {
			t.Fatalf("consumed %d bytes for a %d-byte frame", consumed, n)
		}
		// The frame body is exactly what json.Unmarshal sees; nothing is
		// borrowed from following stream bytes.
		body := data[4 : 4+n]
		var want any
		wantErr := json.Unmarshal(body, &want)
		if (err == nil) != (wantErr == nil) {
			t.Fatalf("decoding mismatch for %q: %v, want %v", body, err, wantErr)
		}
		if err != nil {
			return
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("decoded %#v, want %#v", got, want)
		}
		// Re-encoding the decoded value must produce a frame that decodes equally.
		var stream bytes.Buffer
		if err := WriteFrame(&stream, got); err != nil {
			t.Fatal(err)
		}
		var again any
		if err := ReadFrame(&stream, &again); err != nil || stream.Len() != 0 {
			t.Fatalf("re-encoded frame: %v, %d trailing bytes", err, stream.Len())
		}
		if !reflect.DeepEqual(got, again) {
			t.Fatalf("round trip changed %#v to %#v", got, again)
		}
	})
}
