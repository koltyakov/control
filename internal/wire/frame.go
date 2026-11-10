// Package wire frames JSON control messages without consuming streamed payloads.
package wire

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"

	"github.com/koltyakov/control/internal/model"
)

const MaxFrame = 8 << 20

var ErrFrameTooLarge = errors.New("control message exceeds 8 MiB")

// BoundResponse lets both peer and local API callers receive an actionable
// error even when a provider returns more than the control protocol can carry.
func BoundResponse(response model.Response) model.Response {
	if len(model.JSON(response)) <= MaxFrame {
		return response
	}
	return model.ErrorResponse(&model.RPCError{Code: "result_too_large", Outcome: "unknown", Message: "response exceeds 8 MiB; the operation may have completed; use a task with an artifact result"})
}

func WriteFrame(w io.Writer, value any) error {
	b, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if len(b) > MaxFrame {
		return ErrFrameTooLarge
	}
	// One write keeps the prefix and body in one multiplexer frame and packet.
	frame := make([]byte, 4, 4+len(b))
	binary.BigEndian.PutUint32(frame, uint32(len(b)))
	_, err = w.Write(append(frame, b...))
	return err
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
