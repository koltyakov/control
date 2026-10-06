// Package wire frames JSON control messages without consuming streamed payloads.
package wire

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
)

const MaxFrame = 8 << 20

func WriteFrame(w io.Writer, value any) error {
	b, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if len(b) > MaxFrame {
		return errors.New("control message exceeds 8 MiB")
	}
	var size [4]byte
	binary.BigEndian.PutUint32(size[:], uint32(len(b)))
	if _, err = w.Write(size[:]); err != nil {
		return err
	}
	_, err = w.Write(b)
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
	b := make([]byte, n)
	if _, err := io.ReadFull(r, b); err != nil {
		return err
	}
	return json.Unmarshal(b, value)
}
