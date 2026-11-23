// Package wire frames JSON control messages without consuming streamed payloads.
package wire

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"sync"

	"github.com/koltyakov/control/internal/model"
)

const MaxFrame = 8 << 20

var ErrFrameTooLarge = errors.New("control message exceeds 8 MiB")

// EncodeResponse bounds the exact bytes sent on either API.
func EncodeResponse(response model.Response) ([]byte, error) {
	b, err := json.Marshal(response)
	if err != nil || len(b) <= MaxFrame {
		return b, err
	}
	return json.Marshal(model.ErrorResponse(&model.RPCError{Code: "result_too_large", Outcome: "unknown", Message: "response exceeds 8 MiB; the operation may have completed; use a task with an artifact result"}))
}

func WriteResponse(w io.Writer, response model.Response) error {
	b, err := EncodeResponse(response)
	if err != nil {
		return err
	}
	if len(b) > MaxFrame {
		return ErrFrameTooLarge
	}
	buffer := getFrameBuffer()
	defer putFrameBuffer(buffer)
	buffer.Write(make([]byte, 4))
	buffer.Write(b)
	return writeFrameBuffer(w, buffer.Bytes())
}

// WriteFrame encodes value after a reserved length prefix in a pooled buffer,
// so a frame is not copied again to prepend its length.
func WriteFrame(w io.Writer, value any) error {
	buffer := getFrameBuffer()
	defer putFrameBuffer(buffer)
	buffer.Write(make([]byte, 4))
	if err := json.NewEncoder(buffer).Encode(value); err != nil {
		return err
	}
	// Encode appends a newline that json.Marshal would not; keep the bytes
	// identical to earlier senders.
	frame := buffer.Bytes()
	return writeFrameBuffer(w, frame[:len(frame)-1])
}

// writeFrameBuffer fills the reserved prefix. One write keeps the prefix and
// body in one multiplexer frame and packet.
func writeFrameBuffer(w io.Writer, frame []byte) error {
	size := len(frame) - 4
	if size > MaxFrame {
		return ErrFrameTooLarge
	}
	binary.BigEndian.PutUint32(frame, uint32(size))
	_, err := w.Write(frame)
	return err
}

var frameBuffers = sync.Pool{New: func() any { return new(bytes.Buffer) }}

func getFrameBuffer() *bytes.Buffer { return frameBuffers.Get().(*bytes.Buffer) }

func putFrameBuffer(b *bytes.Buffer) {
	if b.Cap() > 1<<20 {
		return
	}
	b.Reset()
	frameBuffers.Put(b)
}

func ReadFrame(r io.Reader, value any) error {
	return ReadFrameLimit(r, value, MaxFrame)
}

func ReadFrameLimit(r io.Reader, value any, limit uint32) error {
	var size [4]byte
	if _, err := io.ReadFull(r, size[:]); err != nil {
		return err
	}
	n := binary.BigEndian.Uint32(size[:])
	if n > limit || n > MaxFrame {
		return errors.New("control message exceeds frame limit")
	}
	// Grow with received bytes, not the claimed size. A stalled peer must not
	// reserve a full MaxFrame buffer on each of its open streams.
	b, err := io.ReadAll(io.LimitReader(r, int64(n)))
	if err != nil {
		return err
	}
	if len(b) != int(n) {
		if len(b) == 0 {
			return io.EOF
		}
		return io.ErrUnexpectedEOF
	}
	return json.Unmarshal(b, value)
}
