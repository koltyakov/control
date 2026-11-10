package wire

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestReadFramePreservesFollowingPayload(t *testing.T) {
	for _, size := range []int{0, 1024, 1 << 20, MaxFrame - 2} {
		var stream bytes.Buffer
		want := strings.Repeat("x", size)
		if err := WriteFrame(&stream, want); err != nil {
			t.Fatal(err)
		}
		stream.WriteString("payload")
		var got string
		if err := ReadFrame(&stream, &got); err != nil || got != want {
			t.Fatalf("size %d: decoded %d bytes: %v", size, len(got), err)
		}
		if stream.String() != "payload" {
			t.Fatal("frame read consumed following streamed bytes")
		}
	}
}

type emptyFrameBody struct{ requested int }

func (r *emptyFrameBody) Read(p []byte) (int, error) {
	r.requested = max(r.requested, len(p))
	return 0, io.EOF
}

func TestReadFrameDoesNotPreallocateClaimedLength(t *testing.T) {
	var prefix [4]byte
	binary.BigEndian.PutUint32(prefix[:], MaxFrame)
	body := &emptyFrameBody{}
	var got any
	err := ReadFrame(io.MultiReader(bytes.NewReader(prefix[:]), body), &got)
	if !errors.Is(err, io.EOF) {
		t.Fatal("missing body", err)
	}
	if body.requested > 64*1024 {
		t.Fatalf("allocated a %d-byte read buffer before receiving any body", body.requested)
	}
}

func TestReadFrameLimitsAndTruncation(t *testing.T) {
	for _, tc := range []struct {
		name   string
		length uint32
		body   string
		want   error
	}{
		{"no body", 10, "", io.EOF},
		{"partial body", 10, "{}", io.ErrUnexpectedEOF},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var prefix [4]byte
			binary.BigEndian.PutUint32(prefix[:], tc.length)
			var got any
			err := ReadFrame(io.MultiReader(bytes.NewReader(prefix[:]), strings.NewReader(tc.body)), &got)
			if !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
		})
	}
	for _, length := range []uint32{1025, MaxFrame + 1} {
		var prefix [4]byte
		binary.BigEndian.PutUint32(prefix[:], length)
		body := &emptyFrameBody{}
		var got any
		if err := ReadFrameLimit(io.MultiReader(bytes.NewReader(prefix[:]), body), &got, 1024); err == nil || body.requested != 0 {
			t.Fatal("oversized frame read its body", err, body.requested)
		}
	}
	var got any
	if err := ReadFrame(bytes.NewReader(make([]byte, 4)), &got); err == nil {
		t.Fatal("accepted an empty JSON frame")
	} else {
		var syntax *json.SyntaxError
		if !errors.As(err, &syntax) {
			t.Fatal("empty frame did not retain JSON validation", err)
		}
	}
}
